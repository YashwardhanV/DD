# DriveDex — Progress

## Stages
Legend: `[x]` done and verified · `[~]` code complete and tested in the cloud container, but some acceptance checks need the owner's hardware (see "Owner to verify") · `[ ]` not started.

- [x] 0 Project setup
- [~] 1 Basic scanner
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
Stage: 1 code complete (owner checks pending) — next: stage 2

## Decisions
- 2026-10-06: module path = `github.com/yashwardhanv/dd` (matches the actual GitHub repo `YashwardhanV/DD`, so `go install` paths work). BUILD.md suggested `github.com/<user>/drivedex`.
- 2026-10-06: repo layout from BUILD.md section 6 lives at the repo root (not inside a `drivedex/` subfolder).
- 2026-10-06: development happens on a Linux cloud container. Windows-only code is verified with `GOOS=windows go build/vet` (cross-compile); it cannot be *run* here.
- 2026-10-06: LICENSE copyright holder = "Yashwardhan Verma" (taken from the owner's account email — change if wrong).

## Deviations from BUILD.md
- Stage 1: `internal/index/model.go` and `path.go` were created in stage 1 (BUILD.md lists them under stage 2) because the `Scanner` interface (section 7.5) takes an `*index.Builder`. Stage 2 adds the file format, store and search on top.
- Stage 1: added `internal/fixture` (the generator logic) so `cmd/genfixture` and unit tests share one implementation. `cmd/genfixture` is a thin wrapper.
- Stage 1: `ui` also contains console setup (ANSI colors + UTF-8 code page on Windows, restored on exit) and the one-line status display; BUILD.md describes these later but every command needs them from the start.
- Stage 1: `--limit` on `scan` caps the *printed* lines only; the summary still counts the whole scan.

## Blockers / needs owner
- Hardware / admin acceptance criteria (real drives, phone, Administrator terminal, Explorer timings) can't be run in the cloud container. They are listed per stage below as "owner to verify" and are NOT marked passed.

## Owner to verify (hardware-dependent checks)
- **Stage 1:** run `.\bin\drivedex.exe scan E:\ --count-only` on a real drive (ask which letter), paste the summary line into Benchmarks; time an Explorer search of the same drive with a stopwatch and record it too.

## Benchmarks
| Date | Test | Drive (model, FS, port) | Files | Explorer | DriveDex | Mode/Workers | Notes |
|------|------|-------------------------|-------|----------|----------|--------------|-------|
| 2026-10-06 | scan --count-only (fixture fx20k) | cloud container tmpfs (Linux) | 20,171 | n/a | 94 ms (~214k entries/s) | walk | not representative of a USB drive; sanity check only |
