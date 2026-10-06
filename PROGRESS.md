# DriveDex — Progress

## Stages
Legend: `[x]` done and verified · `[~]` code complete and tested in the cloud container, but some acceptance checks need the owner's hardware (see "Owner to verify") · `[ ]` not started.

- [x] 0 Project setup
- [~] 1 Basic scanner
- [~] 2 Index + search
- [~] 3 Drive identity & offline search
- [~] 4 Parallel scanner
- [ ] 5 Android via ADB
- [ ] 6 NTFS MFT + USN
- [ ] 7 Fast copy
- [ ] 8 Interactive UI
- [ ] 9 Auto-index agent
- [ ] 10 Explorer integration
- [ ] 11 Doctor, config, power search
- [ ] 12 Packaging & release

## Current status
Stage: 4 code complete (owner checks pending) — next: stage 5

## Decisions
- 2026-10-06: module path = `github.com/yashwardhanv/dd` (matches the actual GitHub repo `YashwardhanV/DD`, so `go install` paths work). BUILD.md suggested `github.com/<user>/drivedex`.
- 2026-10-06: repo layout from BUILD.md section 6 lives at the repo root (not inside a `drivedex/` subfolder).
- 2026-10-06: development happens on a Linux cloud container. Windows-only code is verified with `GOOS=windows go build/vet` (cross-compile); it cannot be *run* here.
- 2026-10-06: `golang.org/x/sys` latest (v0.48.0) requires Go 1.26, so `go.mod` says `go 1.26.0`. Go's automatic toolchain download makes this work on older installs (needs internet once). BUILD.md asked for latest versions; no functional reason to pin lower.
- 2026-10-06: LICENSE copyright holder = "Yashwardhan Verma" (taken from the owner's account email — change if wrong).

## Deviations from BUILD.md
- Stage 1: `internal/index/model.go` and `path.go` were created in stage 1 (BUILD.md lists them under stage 2) because the `Scanner` interface (section 7.5) takes an `*index.Builder`. Stage 2 adds the file format, store and search on top.
- Stage 1: added `internal/fixture` (the generator logic) so `cmd/genfixture` and unit tests share one implementation. `cmd/genfixture` is a thin wrapper.
- Stage 1: `ui` also contains console setup (ANSI colors + UTF-8 code page on Windows, restored on exit) and the one-line status display; BUILD.md describes these later but every command needs them from the start.
- Stage 4: `index` also gained `--mode`/`--workers` (as specified); `scan.AutoWorkers` (portable) holds the HDD/flash/unknown → 2 / min(16, 2×CPUs) / 4 rule, `volume.SeekPenalty` (Windows IOCTL, stub elsewhere) supplies the input. `bench-scan` always uses the *parallel* scanner (1 worker = parallel with one worker), not `walk`.
- Stage 4: the parallel scanner reads folders with `File.ReadDir(-1)` (unsorted) instead of `os.ReadDir` (sorted) because order doesn't matter and sorting every folder is wasted work; `readDir` is a package variable so tests can inject failing folders.
- Stage 4: race test **was run** (`CGO_ENABLED=1 go test -race ./internal/scan/ ./internal/index/`, gcc available in the cloud container): clean. The owner's PC may have no C compiler; CI runs without `-race` (CGO off) — see stage 12.
- Stage 3: hooks `listVolumes` / `volumeForPath` in `internal/cli/volumes.go` let tests simulate drives being plugged in/out/renumbered on Linux. Real Windows calls only run on the owner's PC (`volume_windows_test.go` is checked to compile via `GOOS=windows go vet`).
- Stage 3: a drive with serial `0` (can happen on odd network drives) is indexed as a plain *folder* source instead of colliding on `vol-00000000`.
- Stage 3: `forget` also accepts the last-seen letter of an unplugged drive and can remove an unreadable (corrupt) `.ddx` file; it matches by id, then letter, then label, and refuses ambiguous matches.
- Stage 3: stale-index tips print one dim line per stale *connected* drive (BUILD.md says "one dim line"; with several stale drives each gets its own line).
- Stage 2: search takes a `Matcher` interface (`index.Search(indexes, matcher, opts)`) instead of a plain term list, so stage 11's query language can reuse the parallel search/ranking code. `TermMatcher` is the stage 2 implementation.
- Stage 2: `internal/index/display.go` (BUILD.md section 7.6 display paths + online/offline `Env`) was written in stage 2 rather than 3, since `search` needs it for folder sources; stage 3 only has to fill `Env.Mounts` from the real drive list.
- Stage 2: extras beyond the file list: `index.Store.List()`/`ReadHeaderFile` (header-only reads for stage 3's `list`), `source.go` (folder source IDs), env var `DRIVEDEX_INDEX_DIR` to relocate the index folder (used by tests; handy for the owner too), and SourceIDs are sanitised into safe file names (e.g. wireless-adb serials contain `:`).
- Stage 1: `--limit` on `scan` caps the *printed* lines only; the summary still counts the whole scan.

## Blockers / needs owner
- Hardware / admin acceptance criteria (real drives, phone, Administrator terminal, Explorer timings) can't be run in the cloud container. They are listed per stage below as "owner to verify" and are NOT marked passed.

## Owner to verify (hardware-dependent checks)
- **Stage 4:** (a) `drivedex index E: --mode walk`, replug, `drivedex index E: --mode parallel` (replug between runs — Windows caches folder listings) and record both times in Benchmarks. (b) `drivedex index E:` prints which worker count it chose and why ("flash/SSD detected" / "spinning hard disk detected" / "drive type unknown") — note what it said for each drive you try, especially a USB hard disk (is HDD detected, or unknown?). (c) Optional: `drivedex bench-scan E: --workers 1,4,16`. Also run `go test ./internal/volume/` on Windows (the seek-penalty test just checks it doesn't crash).
- **Stage 3:** (a) `drivedex drives`, then `drivedex index E:` for a pen drive; `drivedex list`. (b) Unplug the drive and run `drivedex search <known file>` → result must show `[LABEL · offline · last seen E:]`. (c) Plug it into another port/letter → result shows the new letter. (d) `drivedex forget E:` removes only the index (check the drive's files are intact). Also run `go test ./internal/volume/` on Windows (checks `C:\` is NTFS with a serial).
- **Stage 2:** `drivedex index <pen drive letter>` then `drivedex search <a file you know is on it>`; record the index time, the search time shown in the footer, and an Explorer search of the same drive (stopwatch).
- **Stage 1:** run `.\bin\drivedex.exe scan E:\ --count-only` on a real drive (ask which letter), paste the summary line into Benchmarks; time an Explorer search of the same drive with a stopwatch and record it too.

## Benchmarks
| Date | Test | Drive (model, FS, port) | Files | Explorer | DriveDex | Mode/Workers | Notes |
|------|------|-------------------------|-------|----------|----------|--------------|-------|
| 2026-10-06 | `go test -bench` Search, 1,000,000 synthetic entries (queries: resume / photo jpg / a / IMG_2024) | cloud container, 4 vCPU Xeon 2.8 GHz | 1,000,000 | n/a | 17.8 / 11.5 / 21.5 / 5.7 ms per search | parallel, limit 50 | target < 100 ms: PASS |
| 2026-10-06 | `go test -bench` Load, 1,000,000-entry .ddx from disk | cloud container, 4 vCPU Xeon 2.8 GHz | 1,000,000 | n/a | 380 ms (≈ 38 MB file) | read + CRC + parse + lowercase + ID map | target < 1.5 s: PASS |
| 2026-10-06 | index + search fixture fx20k | cloud container tmpfs | 20,171 | n/a | index 126 ms; search < 1 ms | walk | footer shows `<1 ms` |
| 2026-10-06 | `bench-scan` on fixture fx20k (workers 1/2/4/8/16) | cloud container tmpfs, 4 vCPU | 20,171 | n/a | 80 / 56 / 30 / 35 / 43 ms (250k → 656k entries/s peak at 4) | parallel | RAM-cached tmpfs, NOT a USB drive; only shows the scanner scales. Owner numbers needed. |
| 2026-10-06 | scan --count-only (fixture fx20k) | cloud container tmpfs (Linux) | 20,171 | n/a | 94 ms (~214k entries/s) | walk | not representative of a USB drive; sanity check only |
