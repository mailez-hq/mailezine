# ab-bench.ps1 — A/B driver for the delivery micro-batch benchmark.
# Runs ONE mailezine engine container (fresh data volume) and executes the
# standard seed (inbound SMTP delivery) + IMAP workloads, printing a summary
# line per run. Usage:
#
#   .\ab-bench.ps1 -Image mailezine-bench:latest -Tag old
#   .\ab-bench.ps1 -Image mailezine-bench:batch  -Tag new
param(
    [string]$Image = "mailezine-bench:latest",
    [string]$Tag = "run",
    [int]$Msgs = 2000,
    [int]$Conns = 20,
    [switch]$SkipImap
)
$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$conf = Join-Path $root "..\..\docs\bench-comparison\mailezine"
$name = "bench-mailezine-$Tag"
$vol = "bench-mailezine-data-$Tag"

cmd /c "docker rm -f $name >nul 2>&1"
cmd /c "docker volume rm $vol >nul 2>&1"

docker run -d --name $name -p 12143:143 -p 12025:25 -p 11587:1587 -p 12180:11480 `
    -v "${conf}\users.json:/conf/users.json:ro" `
    -v "${conf}\passwords.json:/conf/passwords.json:ro" `
    -v "${conf}\config.toml:/conf/config.toml:ro" `
    -v "${vol}:/data" `
    -e MAILEZINE_CONFIG=/conf/config.toml `
    $Image | Out-Null
if ($LASTEXITCODE -ne 0) { throw "docker run failed" }

# Wait for the IMAP listener, then for the health endpoint (engine fully
# initialized — a TCP-only wait let the bench hit a half-started engine).
$deadline = (Get-Date).AddSeconds(60)
$up = $false
while ((Get-Date) -lt $deadline) {
    try {
        $c = New-Object Net.Sockets.TcpClient
        $c.Connect("127.0.0.1", 12143)
        $c.Close()
        $up = $true
        break
    } catch { Start-Sleep -Milliseconds 500 }
}
if (-not $up) { throw "engine did not open IMAP within 60s" }
$healthy = $false
$deadline = (Get-Date).AddSeconds(30)
while ((Get-Date) -lt $deadline) {
    try {
        $h = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:12180/health" -TimeoutSec 2
        if ($h.StatusCode -eq 200) { $healthy = $true; break }
    } catch { Start-Sleep -Milliseconds 500 }
}
if (-not $healthy) { throw "engine health never went 200" }
Start-Sleep -Seconds 2

$bench = Join-Path $root "bench.exe"
"=== seed ($Tag): $Msgs msgs, $Conns conns, 4096B, inbound SMTP 12025 ==="
& $bench seed -in-smtp 127.0.0.1:12025 -to u00001@example.test -msgs $Msgs -conns $Conns -size 4096 -engine $Tag 2>&1 | Tee-Object -Variable seedOut | Select-Object -Last 12
$seedOut | Out-File -Encoding utf8 "seed-$Tag.txt"

if (-not $SkipImap) {
"=== imap ($Tag): 50 conns, 20s ==="
& $bench imap -imap 127.0.0.1:12143 -user u00001@example.test -pass benchpass -conns 50 -dur 20s 2>&1 | Select-Object -Last 8
}

docker rm -f $name | Out-Null
docker volume rm $vol 2>$null | Out-Null
