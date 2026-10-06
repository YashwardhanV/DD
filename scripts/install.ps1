<#
.SYNOPSIS
  Install DriveDex from an unzipped release folder.

.DESCRIPTION
  Download and unzip drivedex-<version>-windows-<arch>.zip, open PowerShell in
  that folder and run:

      powershell -ExecutionPolicy Bypass -File .\install.ps1

  This just starts `drivedex.exe install`, which does the real work and asks
  y/N before EACH change to your system:
    1. copy the programs to %LOCALAPPDATA%\Programs\DriveDex
    2. add that folder to your user PATH
    3. add the Explorer right-click menu (optional)
    4. start the background agent at log-in (optional)
    5. run `drivedex doctor`
  No administrator rights are needed. `drivedex uninstall` reverses everything.

  Windows may show a "SmartScreen" warning for programs that aren't code-signed:
  choose "More info" then "Run anyway" - or build DriveDex from source yourself.
  (This script does not change any security settings.)

  Extra arguments are passed on, e.g.  .\install.ps1 --yes
#>
$ErrorActionPreference = "Stop"

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
# In a release zip the programs sit next to this script; in a source checkout they are in ..\bin.
$candidates = @((Join-Path $here "drivedex.exe"), (Join-Path (Split-Path -Parent $here) "bin\drivedex.exe"))
$exe = $candidates | Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $exe) {
    throw "drivedex.exe wasn't found next to install.ps1. Unzip the whole release folder first (or run scripts\build.ps1 in a source checkout), then run this script again."
}

& $exe install @args
exit $LASTEXITCODE
