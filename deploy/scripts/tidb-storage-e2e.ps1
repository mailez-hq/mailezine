# TiDB storage backend full e2e (Windows / PowerShell).
#
#   1. idempotently starts a single-node TiDB container on 127.0.0.1:4000
#   2. runs the TiDB KV contract suite (go test ./internal/store, gated by
#      MAILEZINE_TEST_TIDB_DSN)
#   3. starts the engine with MAILEZINE_STORAGE_BACKEND=tidb, sends one
#      message via authenticated SMTP, reads it back via IMAP + POP3
#   4. restarts the engine and verifies the message survives (persistence in
#      TiDB)
#
# Usage:
#   powershell .\deploy\scripts\tidb-storage-e2e.ps1
param(
    [string]$Container = "mailez-tidb",
    [int]$TidbPort = 4000,
    [string]$Dsb = "root@tcp(127.0.0.1:4000)/test"
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)

function Wait-Tcp([int]$port, [int]$seconds) {
    for ($i = 0; $i -lt $seconds * 2; $i++) {
        $c = New-Object Net.Sockets.TcpClient
        try {
            if ($c.ConnectAsync("127.0.0.1", $port).Wait(500)) { return $true }
        } catch {} finally { $c.Dispose() }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

function Invoke-Smoke([string]$mode, [string]$subject) {
    Push-Location $repo
    try {
        $args = @("-mode", $mode, "-imap", "127.0.0.1:11143", "-pop3", "127.0.0.1:10110", "-subject", $subject)
        if ($mode -eq "send") { $args += @("-smtp", "127.0.0.1:11587") }
        go run ./cmd/storage-smoke @args
        if ($LASTEXITCODE -ne 0) { throw "storage-smoke $mode failed (exit $LASTEXITCODE)" }
    } finally { Pop-Location }
}

# ---------- 1. TiDB container ----------
$name = docker ps --filter "name=^/$Container$" --format "{{.Names}}"
if (-not $name) {
    cmd /c "docker rm $Container >NUL 2>&1"
    Write-Host "== starting TiDB container '$Container' (pingcap/tidb:v8.5.1, unistore)"
    docker run -d --name $Container `
        -p "127.0.0.1:${TidbPort}:4000" `
        -v "${Container}-data:/tmp/tidb" `
        pingcap/tidb:v8.5.1 --store=unistore --path=/tmp/tidb | Out-Null
} else {
    Write-Host "== reusing running TiDB container '$Container'"
}
if (-not (Wait-Tcp $TidbPort 30)) { throw "TiDB did not open port $TidbPort" }
Start-Sleep -Seconds 2
Write-Host "== TiDB ready on 127.0.0.1:$TidbPort"

# ---------- 2. clean slate + KV contract suite ----------
# The engine's kv table is fixed ("mailezine_kv"); a reused table from an
# earlier run may point at blobs that no longer exist (configurable blob
# root), so drop it and clear the blob dir before the run.
$env:MAILEZINE_TEST_TIDB_DSN = $Dsb
Write-Host "== cleaning engine kv table and blob dir"
Push-Location $repo
try {
    go run ./cmd/kvctl -dsn $Dsb -drop
    if ($LASTEXITCODE -ne 0) { throw "kv table cleanup failed" }
} finally { Pop-Location }

Write-Host "== running TiDB KV contract tests"
Push-Location $repo
try {
    go test ./internal/store/ -run "TestContractsTiDB" -count=1
    if ($LASTEXITCODE -ne 0) { throw "TiDB KV contract tests failed" }
} finally { Pop-Location }

# ---------- 3/4. engine e2e: send/read + restart persistence ----------
$exe = Join-Path $env:TEMP "mailezine-tidb-e2e.exe"
$data = Join-Path $env:TEMP "mailezine-tidb-e2e-data"
if (Test-Path $data) { Remove-Item -Recurse -Force $data }
New-Item -ItemType Directory -Force -Path $data | Out-Null

$env:MAILEZINE_DIRECTORY_MODE = "dev"
$env:MAILEZINE_DIRECTORY_FILE = Join-Path $repo "internal\directory\testdata\dev-directory.json"
$env:MAILEZINE_AUTH_MODE = "dev"
$env:MAILEZINE_AUTH_DEV_FILE = Join-Path $repo "internal\auth\testdata\dev-passwords.json"
$env:MAILEZINE_STORAGE_BACKEND = "tidb"
$env:MAILEZINE_STORAGE_DSN = $Dsb
$env:MAILEZINE_ROCKS_PATH = $data
$env:MAILEZINE_OUTBOUND_ENABLED = "false"
$env:MAILEZINE_HOSTNAME = "localhost"
$env:MAILEZINE_SMTP_ADDR = ":12025"
$env:MAILEZINE_SUBMISSION_ADDR = ":11587"
$env:MAILEZINE_IMAP_ADDR = ":11143"
$env:MAILEZINE_POP3_ADDR = ":10110"
$env:MAILEZINE_MANAGESIEVE_ADDR = ":11490"

Write-Host "== building engine binary"
Push-Location $repo
try { go build -o $exe ./cmd/mailezine } finally { Pop-Location }

$log = Join-Path $env:TEMP "mailezine-tidb-e2e.log"
function Start-Engine {
    $proc = Start-Process -FilePath $exe -WorkingDirectory $repo `
        -RedirectStandardOutput $log -RedirectStandardError "$log.err" -PassThru
    if (-not (Wait-Tcp 11480 30)) {
        Get-Content "$log.err", $log -ErrorAction SilentlyContinue
        throw "engine health port 11480 never came up (pid $($proc.Id))"
    }
    for ($i = 0; $i -lt 60; $i++) {
        try {
            $h = Invoke-RestMethod -Uri "http://127.0.0.1:11480/health" -TimeoutSec 2
            break
        } catch { Start-Sleep -Seconds 1 }
    }
    Write-Host ("== health: " + ($h | ConvertTo-Json -Compress))
    return $proc
}

$subject = "tidb-e2e-$(Get-Date -UFormat %s)"
Write-Host "== starting engine (run 1) and sending '$subject'"
$proc = Start-Engine
try {
    Invoke-Smoke "send" $subject
    Invoke-Smoke "check" $subject
} finally {
    if (-not $proc.HasExited) { cmd /c "taskkill /PID $($proc.Id) /T /F >NUL 2>&1" }
}

Write-Host "== restarting engine and verifying persistence"
Start-Sleep -Seconds 1
$proc = Start-Engine
try {
    Invoke-Smoke "check" $subject
} finally {
    if (-not $proc.HasExited) { cmd /c "taskkill /PID $($proc.Id) /T /F >NUL 2>&1" }
}

Write-Host "== TiDB storage e2e passed"
