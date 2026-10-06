# DriveDex — Progress

## Stages
- [x] 0 Project setup
- [ ] 1 Basic scanner
- [ ] 2 Index + search
- [ ] 3 Drive identity & offline search
- [ ] 4 Parallel scanner
- [ ] 5 Android via ADB
- [ ] 6 NTFS MFT + USN
- [ ] 7 Fast copy
- [ ] 8 Interactive UI
- [ ] 9 Auto-index agent
- [ ] 10 Explorer integration
- [ ] 11 Doctor, config, power search
- [ ] 12 Packaging & release

## Current status
Stage: 0 complete — next: stage 1

## Decisions
- 2026-10-06: module path = `github.com/yashwardhanv/dd` (matches the actual GitHub repo `YashwardhanV/DD`, so `go install` paths work). BUILD.md suggested `github.com/<user>/drivedex`.
- 2026-10-06: repo layout from BUILD.md section 6 lives at the repo root (not inside a `drivedex/` subfolder).
- 2026-10-06: development happens on a Linux cloud container. Windows-only code is verified with `GOOS=windows go build/vet` (cross-compile); it cannot be *run* here.
- 2026-10-06: LICENSE copyright holder = "Yashwardhan Verma" (taken from the owner's account email — change if wrong).

## Deviations from BUILD.md
- (none yet)

## Blockers / needs owner
- Hardware / admin acceptance criteria (real drives, phone, Administrator terminal, Explorer timings) can't be run in the cloud container. They are listed per stage below as "owner to verify" and are NOT marked passed.

## Owner to verify (hardware-dependent checks)
- (none yet)

## Benchmarks
| Date | Test | Drive (model, FS, port) | Files | Explorer | DriveDex | Mode/Workers | Notes |
|------|------|-------------------------|-------|----------|----------|--------------|-------|
