# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Installs signal-headless on Windows from its GitHub releases, per user (no
# administrator rights):
#
#   irm https://github.com/jaggedmountain/signal-headless/releases/latest/download/install.ps1 | iex
#
# With options (a script block, since `| iex` can't take parameters):
#
#   & ([scriptblock]::Create((irm https://github.com/jaggedmountain/signal-headless/releases/latest/download/install.ps1))) -Version v0.1.0
#
# Options: -Version vX.Y.Z (default: latest), -InstallDir DIR (default:
# %LOCALAPPDATA%\Programs\signal-headless), -NoPath (don't add it to the user
# PATH). The same as environment variables: SIGNAL_HEADLESS_VERSION,
# SIGNAL_HEADLESS_INSTALL_DIR, SIGNAL_HEADLESS_NO_PATH=1.
# SIGNAL_HEADLESS_BASE_URL points at a mirror (default
# https://github.com/jaggedmountain/signal-headless/releases).
#
# Works in Windows PowerShell 5.1 and PowerShell 7. Needs tar.exe, which is
# part of Windows 10 (1803) and later.
param(
  [string]$Version = $env:SIGNAL_HEADLESS_VERSION,
  [string]$InstallDir = $env:SIGNAL_HEADLESS_INSTALL_DIR,
  [switch]$NoPath = ($env:SIGNAL_HEADLESS_NO_PATH -eq '1'),
  [string]$BaseUrl = $env:SIGNAL_HEADLESS_BASE_URL
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Invoke-WebRequest is very slow with it on 5.1

function Say([string]$m) { Write-Host $m }
function Fail([string]$m) { Write-Host "install.ps1: $m" -ForegroundColor Red; throw "install.ps1: $m" }
# Runs a native command, stderr discarded. Windows PowerShell 5.1 turns a
# native command's stderr into a terminating error under 'Stop', even
# redirected; the preference here is local to the function. Exit status in
# $LASTEXITCODE.
function Native([string]$exe) {
  $ErrorActionPreference = 'Continue'
  & $exe @args 2>$null
}

if (-not $Version) { $Version = 'latest' }
if (-not $BaseUrl) { $BaseUrl = 'https://github.com/jaggedmountain/signal-headless/releases' }
$BaseUrl = $BaseUrl.TrimEnd('/')
if (-not $InstallDir) {
  $root = $env:LOCALAPPDATA
  if (-not $root) { $root = Join-Path $HOME '.local' } # PowerShell on Linux/macOS (tests)
  $InstallDir = Join-Path (Join-Path $root 'Programs') 'signal-headless'
}

# Only an x86-64 build exists; Windows 11 on ARM runs it under emulation.
$arch = $env:PROCESSOR_ARCHITECTURE
if ($arch -and $arch -notin @('AMD64', 'ARM64')) { Fail "no release for $arch (x86-64 only)" }
$asset = 'signal-headless-windows-x64.tar.gz'

if ($Version -eq 'latest') {
  $url = "$BaseUrl/latest/download"
} else {
  if (-not $Version.StartsWith('v')) { $Version = "v$Version" }
  $url = "$BaseUrl/download/$Version"
}

if ($PSVersionTable.PSVersion.Major -lt 6) {
  # Windows PowerShell 5.1 may still default to TLS 1.0, which GitHub refuses.
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}
if (-not (Get-Command tar -ErrorAction SilentlyContinue)) { Fail 'needs tar.exe (Windows 10 1803 or later)' }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ('signal-headless-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory $tmp | Out-Null
try {
  Say "Downloading $asset ($Version)..."
  try {
    Invoke-WebRequest -UseBasicParsing -Uri "$url/$asset" -OutFile (Join-Path $tmp $asset)
    Invoke-WebRequest -UseBasicParsing -Uri "$url/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')
  } catch {
    Fail "download failed from ${url}: $($_.Exception.Message)"
  }

  $want = $null
  foreach ($line in Get-Content (Join-Path $tmp 'SHA256SUMS')) {
    $f = $line.Trim() -split '\s+'
    if ($f.Count -eq 2 -and $f[1].TrimStart('*') -eq $asset) { $want = $f[0] }
  }
  if (-not $want) { Fail "$asset is not listed in SHA256SUMS" }
  $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $asset)).Hash
  if ($got -ne $want.ToUpperInvariant()) { Fail "checksum mismatch for $asset - not installing" }

  Native tar -xzf (Join-Path $tmp $asset) -C $tmp
  if ($LASTEXITCODE -ne 0) { Fail "couldn't unpack $asset" }
  $new = Join-Path (Join-Path $tmp 'signal-headless') 'signal-headless.exe'
  if (-not (Test-Path $new)) { Fail "$asset has no signal-headless.exe" }
  $v = Native $new --version
  if ($LASTEXITCODE -ne 0) { Fail "the binary doesn't run here ($v)" }

  New-Item -ItemType Directory -Force $InstallDir | Out-Null
  $exe = Join-Path $InstallDir 'signal-headless.exe'
  $old = "$exe.old"
  Remove-Item -Force $old -ErrorAction SilentlyContinue
  if (Test-Path $exe) {
    # A running daemon keeps its .exe open; Windows allows renaming it, not
    # replacing it. The old copy goes on the next install.
    Move-Item -Force $exe $old
  }
  Copy-Item $new $exe
  foreach ($doc in 'LICENSE', 'README.md') {
    Copy-Item (Join-Path (Join-Path $tmp 'signal-headless') $doc) $InstallDir -Force
  }
  Say "Installed $(Native $exe --version) to $exe"
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$onWindows = ($PSVersionTable.PSVersion.Major -lt 6) -or $IsWindows
$onPath = $false
foreach ($p in $env:PATH.Split([IO.Path]::PathSeparator)) {
  if ($p -and ($p.TrimEnd('\', '/') -ieq $InstallDir.TrimEnd('\', '/'))) { $onPath = $true }
}
if (-not $onPath) {
  if ($NoPath -or -not $onWindows) {
    Say "Note: $InstallDir is not on PATH; add it to run signal-headless by name."
  } else {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $parts = @()
    if ($userPath) { $parts = $userPath.Split(';') | Where-Object { $_ } }
    if (-not ($parts | Where-Object { $_.TrimEnd('\') -ieq $InstallDir.TrimEnd('\') })) {
      [Environment]::SetEnvironmentVariable('Path', (($parts + $InstallDir) -join ';'), 'User')
    }
    $env:PATH = "$env:PATH;$InstallDir"
    Say "Added $InstallDir to the user PATH (new terminals and a restarted VS Code see it)."
  }
}

$exe = Join-Path $InstallDir 'signal-headless.exe'
$newVersion = ((Native $exe --version) -split ' ')[1]
$status = Native $exe --status
if ($LASTEXITCODE -eq 0) {
  $running = ($status | Where-Object { $_ -match '^daemon:\s+(\S+)' } | ForEach-Object { $Matches[1] }) | Select-Object -First 1
  if ($running -and $running -ne $newVersion) {
    Say "The running daemon is $running; restart it on $newVersion with: signal-headless --stop   (it starts again when needed)"
  }
} else {
  Native $exe --check | Out-Null
  if ($LASTEXITCODE -eq 0) {
    Say 'Linked. Start with: signal-headless'
  } else {
    Say 'Next: link this computer - signal-headless --link   (scan the QR code: phone > Settings > Linked devices)'
  }
}

# Success, whatever the probes above returned (not `exit`: under `irm | iex`
# that would close the PowerShell window).
$global:LASTEXITCODE = 0
