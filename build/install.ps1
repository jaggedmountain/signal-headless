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
  try { $v = Native $new --version } catch { $v = $_.Exception.Message; $global:LASTEXITCODE = 1 }
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

# The uninstaller, next to the .exe (signal-headless --uninstall points to
# it): this install's directory, then a fixed body.
$uninstallBody = @'
$ErrorActionPreference = 'Stop'
$exe = Join-Path $InstallDir 'signal-headless.exe'

# Native commands with stderr and a relaxed error preference (see install.ps1).
function Native([string]$cmd) {
  $ErrorActionPreference = 'Continue'
  & $cmd @args 2>$null
}
function Interactive([string]$cmd) {
  $ErrorActionPreference = 'Continue'
  & $cmd @args
}
function Ask([string]$q) {
  if ($Yes) { return $true }
  return ((Read-Host "$q [y/N]") -match '^(y|yes)$')
}

$data = $null
$linked = $false
if (Test-Path $exe) {
  $info = Native $exe --check --json
  $linked = ($LASTEXITCODE -eq 0)
  try { $data = ($info | ConvertFrom-Json).dataDir } catch { }
}
$Purge = -not $Retain
if ($Purge) {
  $msg = "This removes signal-headless ($exe)"
  if ($linked) { $msg += ', unlinks this computer from the Signal account' }
  if ($data -and (Test-Path $data)) { $msg += ", and deletes its message history and keys ($data)" }
  Write-Host "$msg."
  Write-Host 'To keep the history and keys instead, uninstall with -Retain.'
} else {
  Write-Host "This removes signal-headless ($exe); message history and keys stay in $data."
}
if (-not (Ask 'Continue?')) { Write-Host 'Nothing changed.'; return }

if ($Purge -and $linked) {
  Interactive $exe --unlink
  if ($LASTEXITCODE -ne 0) {
    Write-Host 'Unlinking failed. Deleting the data anyway leaves this computer listed on the phone'
    Write-Host '(remove it there: Settings > Linked devices).'
    if (-not (Ask 'Delete the data anyway?')) { Write-Host 'Stopped; the program is still installed.'; return }
  }
}
if (Test-Path $exe) {
  Native $exe --stop | Out-Null
  for ($i = 0; $i -lt 50; $i++) {
    Native $exe --status | Out-Null
    if ($LASTEXITCODE -ne 0) { break }
    Start-Sleep -Milliseconds 200
  }
  # The .exe can't be deleted while the daemon still has it open.
  Get-Process signal-headless -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -eq $exe } | Wait-Process -Timeout 10 -ErrorAction SilentlyContinue
}
if ($Purge -and $data -and (Test-Path $data)) {
  Remove-Item -Recurse -Force $data
  Write-Host "Deleted $data."
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath) {
  $kept = $userPath.Split(';') | Where-Object { $_ -and ($_.TrimEnd('\') -ine $InstallDir.TrimEnd('\')) }
  [Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'User')
}
# Only our own files, never the whole directory: -InstallDir may be shared.
foreach ($f in 'signal-headless.exe', 'signal-headless.exe.old', 'LICENSE', 'README.md', 'update.ps1', 'uninstall.ps1') {
  Remove-Item -Force (Join-Path $InstallDir $f) -ErrorAction SilentlyContinue
}
if (-not (Get-ChildItem -Force $InstallDir -ErrorAction SilentlyContinue)) {
  Remove-Item -Force $InstallDir -ErrorAction SilentlyContinue
}
Write-Host 'Removed signal-headless.'
if (-not $Purge -and $data -and (Test-Path $data)) {
  Write-Host "Kept $data."
  if ($linked) {
    Write-Host 'This computer is still linked. To remove it from the account later: reinstall and run'
    Write-Host 'signal-headless --unlink, or remove it on the phone (Settings > Linked devices).'
  }
}
'@
$uninstaller = @(
  "# Written by signal-headless's install.ps1; removes that install.",
  '#',
  '#   uninstall.ps1           unlink this computer from the Signal account, delete its',
  '#                           message history and keys, and remove the program',
  '#   uninstall.ps1 -Retain   remove only the program; history and keys stay',
  '#   -Yes                    no "are you sure" (unlinking still asks for the number)',
  'param([switch]$Retain, [switch]$Yes)',
  ('$InstallDir = ''' + $InstallDir.Replace("'", "''") + ''''),
  $uninstallBody
) -join [Environment]::NewLine
Set-Content -Path (Join-Path $InstallDir 'uninstall.ps1') -Value $uninstaller -Encoding ASCII

# The updater: the latest release's install.ps1 with this install's options.
# `signal-headless --update` runs it.
$updateBody = @'
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
if ($PSVersionTable.PSVersion.Major -lt 6) {
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
}
if (-not $Version) { $Version = 'latest' }
if ($Version -eq 'latest') {
  $url = "$BaseUrl/latest/download"
} else {
  if (-not $Version.StartsWith('v')) { $Version = "v$Version" }
  $url = "$BaseUrl/download/$Version"
}
$tmp = Join-Path ([IO.Path]::GetTempPath()) ('signal-headless-update-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory $tmp | Out-Null
try {
  $installer = Join-Path $tmp 'install.ps1'
  Invoke-WebRequest -UseBasicParsing -Uri "$url/install.ps1" -OutFile $installer
  Invoke-WebRequest -UseBasicParsing -Uri "$url/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS')
  $want = $null
  foreach ($line in Get-Content (Join-Path $tmp 'SHA256SUMS')) {
    $f = $line.Trim() -split '\s+'
    if ($f.Count -eq 2 -and $f[1].TrimStart('*') -eq 'install.ps1') { $want = $f[0] }
  }
  if (-not $want -or (Get-FileHash -Algorithm SHA256 $installer).Hash -ne $want.ToUpperInvariant()) {
    throw "update.ps1: install.ps1 doesn't match the release's SHA256SUMS; not running it"
  }
  $opts = @{ Version = $Version; InstallDir = $InstallDir; BaseUrl = $BaseUrl }
  if ($NoPath) { $opts.NoPath = $true }
  & $installer @opts
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
'@
$updater = @(
  "# Written by signal-headless's install.ps1; updates that install with the same",
  '# options. History, keys and the link are kept.',
  '#',
  '#   update.ps1                    the latest release',
  '#   update.ps1 -Version vX.Y.Z    a specific one',
  'param([string]$Version = ''latest'')',
  ('$InstallDir = ''' + $InstallDir.Replace("'", "''") + ''''),
  ('$BaseUrl = ''' + $BaseUrl.Replace("'", "''") + ''''),
  ('$NoPath = $' + ([bool]$NoPath).ToString().ToLowerInvariant()),
  $updateBody
) -join [Environment]::NewLine
Set-Content -Path (Join-Path $InstallDir 'update.ps1') -Value $updater -Encoding ASCII

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
