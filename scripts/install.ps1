# DevKuong Memories installer for Windows.
#
#   irm https://raw.githubusercontent.com/IzE-PewPewPew/DK-AgentMemory/main/scripts/install.ps1 | iex
#
# Short on purpose. It downloads one binary, verifies its checksum, puts it in
# %LOCALAPPDATA%\Programs\dkm, and adds that directory to your user PATH. It
# prints every one of those steps and does nothing else.

$ErrorActionPreference = 'Stop'

$Repo       = 'IzE-PewPewPew/DK-AgentMemory'
$InstallDir = if ($env:DKM_INSTALL_DIR) { $env:DKM_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\dkm' }
$Version    = if ($env:DKM_VERSION) { $env:DKM_VERSION } else { 'latest' }

function Die($msg) { Write-Error "install: $msg"; exit 1 }

# --- platform ---------------------------------------------------------------

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Die "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

# --- resolve the version ----------------------------------------------------

if ($Version -eq 'latest') {
    try {
        $release = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
        $Version = $release.tag_name
    } catch {
        Die "could not determine the latest release. Set `$env:DKM_VERSION='vX.Y.Z'"
    }
}

$Asset = "dkm_windows_$arch.zip"
$Base  = "https://github.com/$Repo/releases/download/$Version"

Write-Host "DevKuong Memories $Version  (windows/$arch)"
Write-Host ""

# --- download and verify ----------------------------------------------------

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("dkm-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
    Write-Host "  downloading $Asset"
    Invoke-WebRequest -Uri "$Base/$Asset" -OutFile (Join-Path $tmp $Asset) -UseBasicParsing

    try {
        Invoke-WebRequest -Uri "$Base/dkm_checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt') -UseBasicParsing

        $expected = (Get-Content (Join-Path $tmp 'checksums.txt') |
            Where-Object { $_ -match [regex]::Escape($Asset) } |
            ForEach-Object { ($_ -split '\s+')[0] } | Select-Object -First 1)

        if ($expected) {
            $actual = (Get-FileHash (Join-Path $tmp $Asset) -Algorithm SHA256).Hash.ToLower()
            if ($actual -ne $expected.ToLower()) {
                Die "checksum mismatch - the download is corrupt or tampered with"
            }
            Write-Host "  checksum verified"
        } else {
            Write-Host "  checksum NOT verified (no entry for $Asset)"
        }
    } catch {
        Write-Host "  checksum NOT verified (checksum file unavailable)"
    }

    Expand-Archive -Path (Join-Path $tmp $Asset) -DestinationPath $tmp -Force

    $binary = Join-Path $tmp 'dkm.exe'
    if (-not (Test-Path $binary)) { Die "the archive did not contain dkm.exe" }

    # --- install ------------------------------------------------------------

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $target = Join-Path $InstallDir 'dkm.exe'

    # A running MCP server holds the file open. Say so plainly rather than
    # failing with a permissions error that suggests the wrong fix.
    try {
        Copy-Item $binary $target -Force
    } catch {
        Die "could not replace $target. Close any agent using dkm (Claude Desktop, Cursor) and run this again."
    }

    Write-Host "  installed $target"

    # --- PATH ---------------------------------------------------------------

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ($userPath -notlike "*$InstallDir*") {
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$InstallDir", 'User')
        Write-Host "  added $InstallDir to your user PATH"
        Write-Host ""
        Write-Host "  Open a new terminal for that to take effect."
    }

    $env:Path = "$env:Path;$InstallDir"

    Write-Host ""
    & $target version
    Write-Host ""
    Write-Host "Next:"
    Write-Host "    dkm login https://your-server"
    Write-Host "    dkm connect --all"
    Write-Host "    dkm doctor"
}
finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
