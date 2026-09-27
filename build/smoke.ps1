# SPDX-FileCopyrightText: 2026 Jeff Mattson
# SPDX-License-Identifier: AGPL-3.0-or-later
# build/smoke.sh for plain Windows: runs the .exe from PowerShell, outside
# MSYS2, so a missing DLL (e.g. the mingw runtime) fails here the way it
# would on a normal PC.
#
#   pwsh build/smoke.ps1 bin\signal-headless.exe
param([Parameter(Mandatory)][string]$Bin)
$ErrorActionPreference = 'Stop'
$Bin = (Resolve-Path $Bin).Path
$dir = Join-Path ([IO.Path]::GetTempPath()) ("shsmoke-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory $dir | Out-Null
$sock = Join-Path $dir 's.sock'
$common = @('--fake', '--data', (Join-Path $dir 'data'), '--socket', $sock)

& $Bin --version --json
if ($LASTEXITCODE -ne 0) { throw "--version exited $LASTEXITCODE" }

$daemon = Start-Process -FilePath $Bin -ArgumentList (@('--daemon') + $common) -PassThru -NoNewWindow `
  -RedirectStandardOutput (Join-Path $dir 'out') -RedirectStandardError (Join-Path $dir 'log')
$up = $false
for ($i = 0; $i -lt 100 -and -not $up; $i++) {
  $status = & $Bin --status @common 2>&1
  if ($LASTEXITCODE -eq 0) { $up = $true } else { Start-Sleep -Milliseconds 200 }
}
if (-not $up) {
  Get-Content (Join-Path $dir 'log') -ErrorAction SilentlyContinue
  Stop-Process -Id $daemon.Id -Force -ErrorAction SilentlyContinue
  throw "daemon didn't answer --status: $status"
}
$status
& $Bin --stop @common
if ($LASTEXITCODE -ne 0) { throw "--stop exited $LASTEXITCODE" }
if (-not $daemon.WaitForExit(20000)) { throw 'daemon still running 20s after --stop' }
Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue
'smoke (plain Windows): ok'
