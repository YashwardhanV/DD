<#
.SYNOPSIS
  Build DriveDex: vet, test, then produce the release zips.

.DESCRIPTION
  Run from anywhere:   powershell -ExecutionPolicy Bypass -File scripts\build.ps1

  1. go vet and go test (skip with -SkipChecks, e.g. when CI has just run them)
  2. builds drivedex.exe and drivedex-agent.exe for windows/amd64 and windows/arm64
     (the agent with -H=windowsgui, so it never opens a console window)
  3. writes bin\drivedex-<version>-windows-<arch>.zip containing both programs,
     install.ps1, README.md and LICENSE
  4. leaves bin\drivedex.exe and bin\drivedex-agent.exe built for THIS PC, for
     quick use during development

  The version comes from `git describe --tags --always --dirty` ("dev" if there is no git).
  No CGO: the programs are single .exe files with no extra DLLs.
#>
param(
    [switch]$SkipChecks
)
$ErrorActionPreference = "Stop"

# Always work from the repository root, wherever the script was started.
Set-Location (Split-Path -Parent $PSScriptRoot)

$v = (git describe --tags --always --dirty 2>$null)
if (-not $v) { $v = "dev" }
Write-Host "Building DriveDex $v"

# No CGO: the result is a single .exe with no extra DLLs.
$env:CGO_ENABLED = "0"

function Invoke-Go {
    param([string[]]$GoArgs, [string]$What)
    & go @GoArgs
    if ($LASTEXITCODE -ne 0) { throw "$What failed" }
}

if (-not $SkipChecks) {
    # Checks run for the machine's own OS/CPU, not the cross-compile target.
    Remove-Item Env:GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
    Invoke-Go @("vet", "./...") "go vet"
    Invoke-Go @("test", "./...") "go test"
}

New-Item -ItemType Directory -Force -Path bin | Out-Null

$arches = @("amd64", "arm64")
foreach ($arch in $arches) {
    $name = "drivedex-$v-windows-$arch"
    $stage = Join-Path "bin" $name
    $zip = Join-Path "bin" "$name.zip"

    # Start from an empty staging folder (our own folder under bin\, nothing else).
    if (Test-Path $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $stage | Out-Null

    $env:GOOS = "windows"
    $env:GOARCH = $arch
    Invoke-Go @("build", "-trimpath", "-ldflags", "-s -w -X main.version=$v",
        "-o", (Join-Path $stage "drivedex.exe"), "./cmd/drivedex") "go build (drivedex, $arch)"
    # The background agent is a Windows GUI-subsystem program (-H=windowsgui): no console window.
    Invoke-Go @("build", "-trimpath", "-ldflags", "-s -w -H=windowsgui -X main.version=$v",
        "-o", (Join-Path $stage "drivedex-agent.exe"), "./cmd/drivedex-agent") "go build (drivedex-agent, $arch)"

    Copy-Item README.md, LICENSE, scripts\install.ps1 -Destination $stage

    if (Test-Path $zip) { Remove-Item -LiteralPath $zip -Force }
    Compress-Archive -Path (Join-Path $stage "*") -DestinationPath $zip
    Write-Host "  wrote $zip"
}
Remove-Item Env:GOOS -ErrorAction SilentlyContinue
Remove-Item Env:GOARCH -ErrorAction SilentlyContinue

# Convenience copy built for the PC running this script (handy while developing).
$hostArch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$hostStage = Join-Path "bin" "drivedex-$v-windows-$hostArch"
Copy-Item (Join-Path $hostStage "drivedex.exe"), (Join-Path $hostStage "drivedex-agent.exe") -Destination bin -Force

Write-Host "Built bin\drivedex.exe and bin\drivedex-agent.exe ($v, $hostArch) and the release zips in bin\"
