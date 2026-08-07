# Rebuild dkm.exe and restart the server.
#
# Exists because the obvious `go build -o bin\dkm.exe` fails whenever an MCP
# process is holding the file:
#
#   open bin/dkm.exe: The process cannot access the file because it is being
#   used by another process
#
# Every connected agent -- Claude Desktop, Claude Code, OpenCode, Codex, Kimi --
# keeps a long-lived `dkm.exe mcp` process open, so on a normal working machine
# that lock is the default state rather than the exception.
#
# The blunt fix is to kill every dkm.exe, which is what I did by hand once and
# it silently disconnected Claude Desktop's memory tools mid-session. This does
# the same thing deliberately and then tells you which hosts need restarting,
# so a disconnected agent is never a surprise.
#
#   .\rebuild.ps1

$ErrorActionPreference = "Stop"
Set-Location -Path $PSScriptRoot

Write-Host ""
Write-Host "Rebuilding dkm" -ForegroundColor White
Write-Host ""

$mcp = @(Get-CimInstance Win32_Process -Filter "Name='dkm.exe'" |
         Where-Object { $_.CommandLine -match '\bmcp\b' })

if ($mcp.Count) {
    Write-Host ("  Closing " + $mcp.Count + " agent connection(s) holding the binary") -ForegroundColor Yellow
    $mcp | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
    Start-Sleep -Milliseconds 600
}

# The server holds it too.
$conns = Get-NetTCPConnection -LocalPort 8090 -State Listen -ErrorAction SilentlyContinue
if ($conns) {
    $conns.OwningProcess | Select-Object -Unique | ForEach-Object {
        Stop-Process -Id $_ -Force -ErrorAction SilentlyContinue
    }
    Start-Sleep -Milliseconds 400
}

Write-Host "  Building" -ForegroundColor Cyan
go build -o bin\dkm.exe .\cmd\dkm
if ($LASTEXITCODE -ne 0) { Write-Host "  Build failed." -ForegroundColor Red; exit 1 }
Write-Host ("  Built " + (Get-Item bin\dkm.exe).LastWriteTime) -ForegroundColor DarkGray

& "$PSScriptRoot\start.ps1"

if ($mcp.Count) {
    Write-Host "  Restart these to reconnect their memory tools:" -ForegroundColor Yellow
    Write-Host "      Claude Desktop, Claude Code, OpenCode, Codex, Kimi" -ForegroundColor Yellow
    Write-Host "  A host does not respawn its MCP process on its own." -ForegroundColor DarkGray
    Write-Host ""
}
