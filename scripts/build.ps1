# Build DriveDex: vet, test, then produce bin/drivedex.exe.
# Run from the repo root:  powershell -ExecutionPolicy Bypass -File scripts/build.ps1
$ErrorActionPreference = "Stop"

# Version comes from git tags; falls back to "dev" outside a git checkout.
$v = (git describe --tags --always --dirty 2>$null); if (-not $v) { $v = "dev" }

# No CGO: the result is a single .exe with no extra DLLs.
$env:CGO_ENABLED = "0"

go vet ./...
if ($LASTEXITCODE -ne 0) { throw "go vet failed" }
go test ./...
if ($LASTEXITCODE -ne 0) { throw "go test failed" }

go build -trimpath -ldflags "-s -w -X main.version=$v" -o bin/drivedex.exe ./cmd/drivedex
if ($LASTEXITCODE -ne 0) { throw "go build failed (drivedex)" }

# The background agent is a Windows GUI-subsystem program (-H=windowsgui) so it never shows a console window.
go build -trimpath -ldflags "-s -w -H=windowsgui -X main.version=$v" -o bin/drivedex-agent.exe ./cmd/drivedex-agent
if ($LASTEXITCODE -ne 0) { throw "go build failed (drivedex-agent)" }

Write-Host "Built bin/drivedex.exe and bin/drivedex-agent.exe ($v)"
