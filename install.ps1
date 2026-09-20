# Install cmdbus on Windows: clone (or update) the source, build it, put it on
# your user PATH.
#
#   powershell -ExecutionPolicy Bypass -File install.ps1
#
# The repository is private, so git must be able to reach it (SSH key or a
# credential helper). Override any of these with environment variables:
#
#   CMDBUS_REPO  git URL to clone       (default: git@github.com:prerit714/cmdbus.git)
#   CMDBUS_SRC   where the source goes  (default: %LOCALAPPDATA%\cmdbus\src)
#   CMDBUS_BIN   where the binary goes  (default: %LOCALAPPDATA%\cmdbus\bin)
#
# On Linux and macOS use install.sh instead.

$ErrorActionPreference = 'Stop'

if (-not $env:LOCALAPPDATA) { throw 'install.ps1 is for Windows; on Linux and macOS run install.sh' }

$Repo = if ($env:CMDBUS_REPO) { $env:CMDBUS_REPO } else { 'git@github.com:prerit714/cmdbus.git' }
$Src  = if ($env:CMDBUS_SRC)  { $env:CMDBUS_SRC }  else { Join-Path $env:LOCALAPPDATA 'cmdbus\src' }
$Bin  = if ($env:CMDBUS_BIN)  { $env:CMDBUS_BIN }  else { Join-Path $env:LOCALAPPDATA 'cmdbus\bin' }

foreach ($tool in 'git', 'go') {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "'$tool' is required but was not found" }
}

# Native programs do not throw on failure; check their exit code.
function Invoke-Native {
    param([string]$Exe, [string[]]$Arguments)
    & $Exe @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Exe $($Arguments -join ' ') failed with exit code $LASTEXITCODE" }
}

# 1. Clone, or fast-forward an existing clone.
if (Test-Path (Join-Path $Src '.git')) {
    Write-Host "==> updating $Src"
    Invoke-Native git @('-C', $Src, 'pull', '--ff-only')
} else {
    Write-Host "==> cloning $Repo into $Src"
    New-Item -ItemType Directory -Force -Path (Split-Path $Src -Parent) | Out-Null
    Invoke-Native git @('clone', $Repo, $Src)
}

# 2. Build the single binary.
$Exe = Join-Path $Bin 'cmdbus.exe'
Write-Host "==> building $Exe"
New-Item -ItemType Directory -Force -Path $Bin | Out-Null
Push-Location $Src
try { Invoke-Native go @('build', '-o', $Exe, '.') } finally { Pop-Location }

# 3. Put $Bin on the user's PATH (persisted in the registry), once.
$UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$Entries = @($UserPath -split ';' | Where-Object { $_ })
if ($Entries -contains $Bin) {
    Write-Host "==> $Bin is already on your user PATH"
} else {
    [Environment]::SetEnvironmentVariable('Path', (($Entries + $Bin) -join ';'), 'User')
    Write-Host "==> added $Bin to your user PATH (new terminals will see it)"
}
# Make it usable in this session too.
if (($env:Path -split ';') -notcontains $Bin) { $env:Path = "$env:Path;$Bin" }

Write-Host '==> installed. Try:  cmdbus -h   |   cmdbus docs'
