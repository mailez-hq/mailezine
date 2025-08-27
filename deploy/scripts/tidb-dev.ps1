# TiDB 存储后端开发启动器（Windows / PowerShell）。
#
# 动作:
#   1. 幂等启动单节点 TiDB 容器（pingcap/tidb v8.5.1，--store=unistore，
#      MySQL 协议监听 127.0.0.1:4000）；容器已运行则直接复用。
#   2. 以 MAILEZINE_STORAGE_BACKEND=tidb 在宿主机前台启动 mailezine；
#      所有监听搬到高位端口，避免与本机已在跑的 mailez compose 栈
#      (25/143/1587...) 冲突。
#   3. -Smoke: 引擎转后台运行，执行 storage-smoke 发信/收信回归后自动停止。
#
# 用法:
#   powershell .\deploy\scripts\tidb-dev.ps1            # 前台启动引擎
#   powershell .\deploy\scripts\tidb-dev.ps1 -Smoke     # 后台引擎 + 冒烟验证
param(
    [string]$Container = "mailez-tidb",
    [int]$TidbPort = 4000,
    [switch]$Smoke
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

# ---------- 1. TiDB 容器 ----------
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
# TCP 通了再缓冲两秒，等 SQL 层完全就绪。
Start-Sleep -Seconds 2
$dsn = "root@tcp(127.0.0.1:$TidbPort)/test"
Write-Host "== TiDB ready on 127.0.0.1:$TidbPort (DSN: root@tcp(127.0.0.1:$TidbPort)/test)"

# ---------- 2. mailezine 环境 ----------
$env:MAILEZINE_DIRECTORY_MODE = "dev"
$env:MAILEZINE_DIRECTORY_FILE = Join-Path $repo "internal\directory\testdata\dev-directory.json"
$env:MAILEZINE_AUTH_MODE = "dev"
$env:MAILEZINE_AUTH_DEV_FILE = Join-Path $repo "internal\auth\testdata\dev-passwords.json"
$env:MAILEZINE_STORAGE_BACKEND = "tidb"
$env:MAILEZINE_STORAGE_DSN = $dsn
$env:MAILEZINE_OUTBOUND_ENABLED = "false"
$env:MAILEZINE_HOSTNAME = "localhost"
$env:MAILEZINE_SMTP_ADDR = ":12025"
$env:MAILEZINE_SUBMISSION_ADDR = ":11587"
$env:MAILEZINE_IMAP_ADDR = ":11143"
$env:MAILEZINE_POP3_ADDR = ":10110"

if (-not $Smoke) {
    Write-Host "== starting mailezine in foreground (Ctrl+C to stop; TiDB stays up)"
    Push-Location $repo
    try { go run ./cmd/mailezine } finally { Pop-Location }
    return
}

# ---------- 3. -Smoke: 后台引擎 + 冒烟回归 ----------
$exe = Join-Path $env:TEMP "mailezine-tidb-dev.exe"
Write-Host "== building engine binary"
Push-Location $repo
try { go build -o $exe ./cmd/mailezine } finally { Pop-Location }

$log = Join-Path $env:TEMP "mailezine-tidb-dev.log"
$proc = Start-Process -FilePath $exe -WorkingDirectory $repo `
    -RedirectStandardOutput $log -RedirectStandardError "$log.err" -PassThru
Write-Host "== engine pid $($proc.Id), log $log"

try {
    if (-not (Wait-Tcp 11480 30)) {
        Get-Content "$log.err", $log -ErrorAction SilentlyContinue
        throw "engine health port 11480 never came up"
    }
    for ($i = 0; $i -lt 60; $i++) {
        try {
            $h = Invoke-RestMethod -Uri "http://127.0.0.1:11480/health" -TimeoutSec 2
            break
        } catch { Start-Sleep -Seconds 1 }
    }
    Write-Host ("== health: " + ($h | ConvertTo-Json -Compress))

    $subject = "tidb-smoke-$(Get-Date -UFormat %s)"
    Push-Location $repo
    try {
        go run ./cmd/storage-smoke -mode send -smtp 127.0.0.1:11587 -imap 127.0.0.1:11143 -pop3 127.0.0.1:10110 -subject $subject
        if ($LASTEXITCODE -ne 0) { throw "storage-smoke send failed" }
        go run ./cmd/storage-smoke -mode check -imap 127.0.0.1:11143 -pop3 127.0.0.1:10110 -subject $subject
        if ($LASTEXITCODE -ne 0) { throw "storage-smoke check failed" }
    } finally { Pop-Location }

    Write-Host "== TiDB storage smoke passed"
} finally {
    # 只杀进程树里的 exe；go run 包装层的孤儿问题通过直跑二进制规避。
    if (-not $proc.HasExited) {
        cmd /c "taskkill /PID $($proc.Id) /T /F >NUL 2>&1"
    }
}
