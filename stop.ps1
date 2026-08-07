# Stop the server. Leaves the database running and your data untouched.
#
#   .\stop.ps1            stop the server only
#   .\stop.ps1 -All       stop the database too

param([switch]$All)

Set-Location -Path $PSScriptRoot
$Port = 8090

# By listening port, not by process name: a binary from another folder holding
# the port is exactly the case a name filter misses.
$conns = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($conns) {
    $conns.OwningProcess | Select-Object -Unique | ForEach-Object {
        $p = Get-Process -Id $_ -ErrorAction SilentlyContinue
        if ($p) {
            Write-Host ("Stopping server (pid " + $p.Id + ")") -ForegroundColor Cyan
            Stop-Process -Id $_ -Force
        }
    }
} else {
    Write-Host "The server was not running." -ForegroundColor DarkGray
}

if ($All) {
    Write-Host "Stopping the database" -ForegroundColor Cyan
    docker stop dkm-pg | Out-Null
    Write-Host "Your memories are safe -- they live in a Docker volume, not in the container." -ForegroundColor DarkGray
}

Write-Host "Done." -ForegroundColor Green
