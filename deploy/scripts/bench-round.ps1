param(
  [Parameter(Mandatory = $true)][string]$Engine,  # postdove | mailezine
  [Parameter(Mandatory = $true)][string]$Round,   # A | B (order in which the stack is tested)
  [string]$Repo    = 'D:\code\mailez-hq\mailez',
  [string]$Bench   = 'D:\code\mailez-hq\.bench-fetch\bench.exe',
  [string]$DataDir = 'D:\code\mailez-hq\.bench-mysql\data',
  [string]$LogDir  = 'D:\code\mailez-hq\.bench-fetch'
)

$ErrorActionPreference = 'Stop'
$log = Join-Path $LogDir "round$Round-$Engine.log"
Remove-Item -LiteralPath $log -ErrorAction SilentlyContinue

function Log([string]$m) {
  $ts = Get-Date -Format 'HH:mm:ss'
  Write-Host "[$ts] $m"
  Add-Content -LiteralPath $log -Value "[$ts] $m"
}

function Wait-Healthy([string]$name, [int]$tries = 60) {
  for ($i = 0; $i -lt $tries; $i++) {
    $st = docker inspect -f '{{.State.Health.Status}}' $name 2>&1
    if ($st -eq 'healthy') { return }
    Start-Sleep -Seconds 2
  }
  throw "container $name not healthy after $tries tries"
}

function Run-Bench([string[]]$benchArgs) {
  $line = "& $Bench $($benchArgs -join ' ')"
  Log "RUN: $line"
  $prev = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  & $Bench @benchArgs 2>&1 | Tee-Object -FilePath $log -Append
  $ErrorActionPreference = $prev
  if ($LASTEXITCODE -ne 0) { Log "WARN: exit code $LASTEXITCODE" }
}

function Reset-MysqlQuota {
  docker exec -e MYSQL_PWD=benchmysql bench-mysql mysql -uroot -e "USE mailez; UPDATE user SET quota_bytes_used=0;" 2>&1 | Out-Null
}

function Reset-Redis {
  docker exec bench-postdove-redis-1 redis-cli FLUSHALL 2>&1 | Out-Null
}

function Sample-Memory([string]$label, [int]$seconds, [string]$containers) {
  Log "MEM($label): sampling $containers for ${seconds}s"
  & $Bench -dur "${seconds}s" -containers $containers stats 2>&1 | Tee-Object -FilePath $log -Append
}

Log "==== Round ${Round}: $Engine ===="

if ($Engine -eq 'postdove') {
  $smtp   = '127.0.0.1:21587'
  $inSmtp = '127.0.0.1:25025'
  $imap   = '127.0.0.1:21143'
  $containers = 'bench-postdove-gateway-1,bench-postdove-postfix-1,bench-postdove-dovecot-1'

  Log "RESET: bench-postdove stack (down -v, up)"
  docker compose -f "$Repo\deploy\docker-compose.bench-postdove.yml" down -v
  docker compose -f "$Repo\deploy\docker-compose.bench-postdove.yml" up -d
  Wait-Healthy 'bench-postdove-gateway-1'
  Wait-Healthy 'bench-postdove-postfix-1'
  Start-Sleep -Seconds 5
  Reset-Redis
  Reset-MysqlQuota
  Sample-Memory 'idle' 20 $containers
} else {
  $smtp   = '127.0.0.1:31587'
  $inSmtp = '127.0.0.1:35025'
  $imap   = '127.0.0.1:3143'
  # enterprise has no dedicated caddy gateway anymore (the nginx gateway is
  # HTTP-only and outside the mail path); sample the engine container only.
  $containers = 'bench-mz2'

  Log "RESET: mailezine engine container + MinIO bucket"
  docker rm -f bench-mz2 2>&1 | Out-Null
  if (Test-Path $DataDir) {
    Get-ChildItem -LiteralPath $DataDir -Force | Remove-Item -Recurse -Force
  }
  New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
  docker exec bench-minio2 sh -c "mc alias set local http://127.0.0.1:9000 benchminio benchminio123 >/dev/null 2>&1; mc rm --recursive --force local/mailezine" 2>&1 | Out-Null
  docker run -d --name bench-mz2 -p 127.0.0.1:35025:25 -p 127.0.0.1:3143:143 -p 127.0.0.1:31587:1587 -p 127.0.0.1:34190:4190 `
    -e MAILEZINE_SMTP_ADDR=:25 -e MAILEZINE_SUBMISSION_ADDR=:1587 -e MAILEZINE_IMAP_ADDR=:143 `
    -e MAILEZINE_MANAGESIEVE_ADDR=:4190 -e MAILEZINE_POP3_ADDR=:110 `
    -e MAILEZINE_STORAGE_BACKEND=pebble -e MAILEZINE_ROCKS_PATH=/data/rocks `
    -e MAILEZINE_S3_ENDPOINT=http://host.docker.internal:19000 `
    -e MAILEZINE_S3_ACCESS_KEY=benchminio -e MAILEZINE_S3_SECRET_KEY=benchminio123 `
    -e MAILEZINE_S3_BUCKET=mailezine `
    -e MAILEZINE_DIRECTORY_MODE=mailez -e MAILEZINE_AUTH_MODE=mailez `
    -e MAILEZINE_BACKEND_ADDRESS=host.docker.internal:8080 `
    -e MAILEZINE_LOG_LEVEL=warn `
    -e MAILEZINE_OUTBOUND_FIXED_HOST=host.docker.internal -e MAILEZINE_OUTBOUND_FIXED_PORT=25252 `
    -e MAILEZINE_QUEUE_POLL_INTERVAL_SECONDS=1 `
    -v "${DataDir}:/data" mailez/mailezine:local
  Wait-Healthy 'bench-mz2'
  Start-Sleep -Seconds 3
  Reset-Redis
  Reset-MysqlQuota
  Sample-Memory 'idle' 20 $containers
}

Log "PHASE 1: verify"
Run-Bench @('-engine', $Engine, '-smtp', $smtp, '-imap', $imap, '-user', 'u00001@example.com', '-pass', 'benchpass', 'verify')

Log "PHASE 2: inbound seed throughput (200 @ 20 conns, random recipients)"
Run-Bench @('-engine', $Engine, '-in-smtp', $inSmtp, '-size', '16384', 'seed', '-conns', '20', '-msgs', '200')

Log "PHASE 3: IMAP prep (100 sequential messages, one per user u00001..u00100)"
Run-Bench @('-engine', $Engine, '-in-smtp', $inSmtp, '-to', 'u%05d@example.com', '-size', '16384', '-seq', 'seed', '-conns', '10', '-msgs', '100')

Log "PHASE 4: SMTP submission (100 @ 20 conns, 1000 senders) + load memory sampling"
$memFile = Join-Path $LogDir "round$Round-$Engine-load-mem.txt"
Remove-Item -LiteralPath $memFile -ErrorAction SilentlyContinue
Start-Process -FilePath $Bench -ArgumentList '-dur','60s','-containers',$containers,'stats' `
  -RedirectStandardOutput $memFile -RedirectStandardError "$memFile.err" -WindowStyle Hidden
Start-Sleep -Seconds 3
Run-Bench @('-engine', $Engine, '-smtp', $smtp, '-size', '16384', '-users', '1000', 'smtp', '-conns', '20', '-msgs', '100')
Start-Sleep -Seconds 40
Get-Content -LiteralPath $memFile | Tee-Object -FilePath $log -Append
Log "LOAD-MEM-FILE: $memFile"

Log "PHASE 5: IMAP concurrent (100 conns, 100 users, 30s)"
Run-Bench @('-engine', $Engine, '-imap', $imap, '-user', 'u00001@example.com', '-pass', 'benchpass', '-users', '100', 'imap', '-conns', '100', '-dur', '30s')

Log "PHASE 6: outbound queue (200 @ 20 conns, 1000 senders, mock MX)"
Run-Bench @('-engine', $Engine, '-smtp', $smtp, '-mx', '127.0.0.1:25252', '-size', '16384', '-users', '1000', '-wait', '3m', 'queue', '-conns', '20', '-msgs', '200')

Log "==== Round ${Round}: $Engine done ===="
