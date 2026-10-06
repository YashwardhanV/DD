# Benchmarks

Every number here was measured; where a number could **not** be measured (it needs a real drive, a phone or an Administrator terminal), the table says so and the cell is empty rather than guessed. The same data, with the day-to-day decisions around it, lives in [PROGRESS.md](../PROGRESS.md).

## 1. Measured so far — cloud container (no USB hardware)

**Machine:** cloud container, 4 vCPU (Intel Xeon @ 2.80 GHz), Linux, Go 1.26, files on a RAM-backed or page-cached disk. 2026-10-06.

> ⚠️ These measure DriveDex's **CPU and memory work** (search, loading, scanner scaling). They are **not** USB results: there is no spinning disk, no USB bridge, no phone. They show the algorithms scale; they do not show the speed-up over Explorer on a real drive.

| Test | Files / entries | Result | Target | Verdict |
|---|---|---|---|---|
| Search, in memory — query `resume` | 1,000,000 | 17.8 ms | < 100 ms | pass |
| Search — `photo jpg` | 1,000,000 | 11.5 ms | < 100 ms | pass |
| Search — `a` (matches almost everything) | 1,000,000 | 21.5 ms | < 100 ms | pass |
| Search — `IMG_2024` | 1,000,000 | 5.7 ms | < 100 ms | pass |
| Load a saved index from disk (read + CRC + parse + lowercase + ID map), ≈ 38 MB file | 1,000,000 | 380 ms | < 1.5 s | pass |
| Query syntax — `resume` | 1,000,000 | 22 ms | < 100 ms | pass |
| Query — `resume ext:pdf` | 1,000,000 | 21 ms | < 100 ms | pass |
| Query — `photo ext:jpg,png size:>2mb` | 1,000,000 | 27 ms | < 100 ms | pass |
| Query — `photo ext:jpg modified:<3650d size:>1mb` | 1,000,000 | 20 ms | < 100 ms | pass |
| Query — `invoice !draft ext:pdf path:folder` (builds paths for candidates) | 1,000,000 | 58 ms | < 100 ms | pass |
| Query — regex `re:^IMG_\w+_\d{6}\.jpg$` | 1,000,000 | 72 ms | < 100 ms | pass |
| Index a folder (single-threaded walk), fixture fx20k | 20,171 | 126 ms (search then `<1 ms`) | — | — |
| `scan --count-only`, fixture fx20k | 20,171 | 94 ms (≈ 214k entries/s) | — | sanity check |
| `bench-scan` scaling, fixture fx20k, workers 1 / 2 / 4 / 8 / 16 | 20,171 | 80 / 56 / 30 / 35 / 43 ms (peak ≈ 656k entries/s at 4 workers) | — | scales; tmpfs, not USB |
| Copy fixture fx20k (20,000 small files, 38.8 MB), 3 alternating rounds | 20,000 | `cp -r`: 7980 / 920 / 662 ms · `drivedex copy --workers 8`: 1496 / 622 / 592 ms | comparable to `robocopy /MT` | very noisy (page cache); shows parity or better, **not** a USB result |

### How to reproduce these

```powershell
# search and load (synthetic 1,000,000-entry index)
go test ./internal/index -run xxx -bench "BenchmarkSearch|BenchmarkLoad" -benchtime 5x
# the query language
go test ./internal/query -run xxx -bench BenchmarkQuery -benchtime 5x
# a repeatable test tree (same --seed = same tree)
go run ./cmd/genfixture --out .\fx20k --files 20000
drivedex index .\fx20k ; drivedex scan .\fx20k --count-only
drivedex bench-scan .\fx20k --workers 1,2,4,8,16
```

## 2. Still to measure — real hardware

These are the numbers that matter for the project's claim ("faster than Explorer on external drives and phones"). Each needs the owner's PC and drives; **none is claimed until it is filled in with the drive, the port and the evidence.** (The checklists for taking them are in [PROGRESS.md](../PROGRESS.md#owner-to-verify-hardware-dependent-checks).)

| Test | Drive (model, file system, USB port) | Files | Explorer | DriveDex | Mode / workers | Notes |
|---|---|---|---|---|---|---|
| Index a pen drive (`index E:`) | | | n/a | | | |
| Search a drive, Explorer's box vs `drivedex search` (stopwatch) | | | | | | |
| Walk vs parallel scan, replug between runs (stage 4) | | | | walk: / parallel: | | |
| USB hard disk: workers 1 / 2 / 4 / 8 (stage 4) | | | | | | confirms "2 is best on HDD" |
| `index phone` — 50k-file phone (stage 5) | | | "gave up after N min" | | adb | target: under ~1 minute |
| NTFS turbo mode, `index C: --mode mft` (Administrator, stage 6) | | | | | mft | target: < ~15 s per million files |
| Turbo mode vs parallel entry counts on the same NTFS drive (±2 %) | | | | | | explain differences |
| USN delta refresh after a few changes | | | n/a | | mft | target: < 1 s |
| Copy 5,000+ small files: Explorer vs `robocopy /E /MT:16` vs `drivedex copy` | | 5,000+ | | | | all three times |
| Copy resume after Ctrl+C / replug | | | n/a | | | must skip finished files |
| `doctor --bench` write speed on a USB 2 port vs a USB 3 port | | | n/a | MB/s: | | verdicts should differ |

**Hardware notes to record with every result:** laptop/PC model, Windows version, drive make/model/capacity, file system, which USB port (blue/"SS" = USB 3), cable, whether the drive had been unplugged since the previous run (Windows caches folder listings, so only a *first* run after a replug is an honest cold-cache number).

## 3. Performance targets (from the project plan)

| Operation | Target | Status |
|---|---|---|
| Search 1M entries (in memory) | < 100 ms | **met** (≤ 72 ms worst case measured) |
| Load a 1M-entry index from disk | < 1.5 s | **met** (380 ms) |
| MFT enumeration | < ~15 s per million files | needs an NTFS drive + Administrator |
| USN delta refresh | < 1 s for typical changes | needs an NTFS drive + Administrator |
| Phone index (50k files) | under ~1 minute | needs a phone |
| Copy of many small files | faster than Explorer; comparable to `robocopy /MT` | needs real drives |
