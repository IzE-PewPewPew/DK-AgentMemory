# Start DevKuong Memories and open the viewer.
#
# Everything the stack needs, in dependency order, with a real check after each
# step rather than a sleep and a hope. Written because the sequence -- start
# Docker, wait for Postgres to actually accept connections, export the LLM key
# into *this* shell, start the server, wait for it to listen, then open the
# right URL -- is five things that each fail differently, and the failure
# messages do not say which one happened.
#
#   Right-click this file -> Run with PowerShell
#   or:  .\start.ps1
#
# Stop it later with:  .\stop.ps1

$ErrorActionPreference = "Stop"
Set-Location -Path $PSScriptRoot

$Port      = 8090
$Container = "dkm-pg"
$Url       = "http://127.0.0.1:$Port/viewer/"
$LogDir    = Join-Path $PSScriptRoot "logs"

function Step($n, $msg) { Write-Host ("[$n/5] " + $msg) -ForegroundColor Cyan }
function Ok($msg)       { Write-Host ("      " + $msg) -ForegroundColor DarkGray }
function Die($msg, $fix) {
    Write-Host ""
    Write-Host ("  " + $msg) -ForegroundColor Red
    if ($fix) { Write-Host ("  " + $fix) -ForegroundColor Yellow }
    Write-Host ""
    Read-Host "Press Enter to close"
    exit 1
}

Write-Host ""
Write-Host "DevKuong Memories" -ForegroundColor White
Write-Host ""

# ---------------------------------------------------------------- 1. Docker
Step 1 "Checking Docker"
try { docker info *> $null } catch {
    Die "Docker is not running." "Open Docker Desktop, wait for the whale icon to go steady, then run this again."
}
if ($LASTEXITCODE -ne 0) {
    Die "Docker is not running." "Open Docker Desktop, wait for the whale icon to go steady, then run this again."
}
Ok "Docker is up"

# -------------------------------------------------------------- 2. Postgres
Step 2 "Starting the database"
$state = (docker inspect -f '{{.State.Running}}' $Container 2>$null)
if ($LASTEXITCODE -ne 0) {
    Die "No container named '$Container' exists." "The database was never created on this machine. Ask Claude to set it up again."
}
if ($state -ne "true") { docker start $Container | Out-Null }

# Poll until Postgres actually accepts connections. "Container running" and
# "database ready" are seconds apart, and connecting in that gap is the most
# common way this fails.
$ready = $false
foreach ($i in 1..30) {
    docker exec $Container pg_isready -U dkm *> $null
    if ($LASTEXITCODE -eq 0) { $ready = $true; break }
    Start-Sleep -Milliseconds 700
}
if (-not $ready) { Die "The database did not become ready." "Try: docker logs $Container --tail 30" }
Ok "Database is accepting connections"

# ------------------------------------------------------------- 3. LLM key
Step 3 "Loading your Kimi API key"
if (-not $env:DKM_LLM_API_KEY) {
    $env:DKM_LLM_API_KEY = [Environment]::GetEnvironmentVariable("DKM_LLM_API_KEY", "User")
}
if ($env:DKM_LLM_API_KEY) {
    Ok "Key loaded"
} else {
    # Not fatal. Everything except Compose and reading new sessions still works,
    # and saying so is better than a provider 401 an hour from now.
    Write-Host "      No key found. Compose and reading sessions will be off;" -ForegroundColor Yellow
    Write-Host "      everything already stored still works." -ForegroundColor Yellow
    Write-Host '      Fix with:  setx DKM_LLM_API_KEY "your-key"   (then open a new window)' -ForegroundColor Yellow
}

# ---------------------------------------------------------------- 4. Server
Step 4 "Starting the server"
$listening = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($listening) {
    # Kill by listening port, not by process name. A stale binary from another
    # folder holding this port once served an old build for hours while every
    # restart reported success.
    $listening.OwningProcess | Select-Object -Unique | ForEach-Object {
        $p = Get-Process -Id $_ -ErrorAction SilentlyContinue
        if ($p) { Ok ("Stopping the old server (pid " + $p.Id + ")"); Stop-Process -Id $_ -Force }
    }
    Start-Sleep -Milliseconds 600
}

if (-not (Test-Path "bin\dkm.exe")) {
    Ok "Building dkm.exe (first run takes a minute)"
    go build -o bin\dkm.exe .\cmd\dkm
    if ($LASTEXITCODE -ne 0) { Die "The build failed." "Check that Go is installed: go version" }
}

if (-not (Test-Path $LogDir)) { New-Item -ItemType Directory -Path $LogDir | Out-Null }
Start-Process -FilePath ".\bin\dkm.exe" `
    -ArgumentList "serve", "--config", "config.yaml" `
    -RedirectStandardOutput (Join-Path $LogDir "server.out.log") `
    -RedirectStandardError  (Join-Path $LogDir "server.err.log") `
    -WindowStyle Hidden

$up = $false
foreach ($i in 1..30) {
    Start-Sleep -Milliseconds 600
    try {
        $r = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/v1/healthz" -UseBasicParsing -TimeoutSec 2
        if ($r.StatusCode -ge 200) { $up = $true; break }
    } catch {
        # 401 means it is listening and asking for a key -- that is up.
        if ($_.Exception.Response -and $_.Exception.Response.StatusCode.value__ -eq 401) { $up = $true; break }
    }
}
if (-not $up) {
    $err = Join-Path $LogDir "server.err.log"
    Write-Host ""
    Write-Host "  The server did not start. Last few lines:" -ForegroundColor Red
    if (Test-Path $err) { Get-Content $err -Tail 8 | ForEach-Object { Write-Host ("    " + $_) -ForegroundColor DarkGray } }
    Die "" "Full log: $err"
}
Ok "Server is listening on port $Port"

# ---------------------------------------------------------------- 5. Viewer
Step 5 "Opening the viewer"
Start-Process $Url
Write-Host ""
Write-Host "  Ready:  $Url" -ForegroundColor Green
Write-Host ""
Write-Host "  It will ask for an API key. Get one with:" -ForegroundColor White
Write-Host "      .\bin\dkm.exe admin key issue" -ForegroundColor White
Write-Host ""
Write-Host "  Leave this window open or close it -- the server keeps running." -ForegroundColor DarkGray
Write-Host "  Stop it with:  .\stop.ps1" -ForegroundColor DarkGray
Write-Host ""
