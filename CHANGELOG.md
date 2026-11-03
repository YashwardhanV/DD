# Changelog

## 2.0.0 — not released yet (waiting for the owner's hardware benchmarks)

Built from [docs/BUILD-V2.pdf](docs/BUILD-V2.pdf). The goal: copying faster in real use, phone → PC most of all — and proving it with numbers. **Every feature has a switch and falls back to the 1.0 behaviour; `drivedex copy --v1` turns all of them off.** Defaults marked *pending* are set from the owner's benchmarks ([docs/BENCHMARKS.md](docs/BENCHMARKS.md) section 2a) before the release.

| Feature (stage) | What it does | Switch | Default |
|---|---|---|---|
| Copy tracer (13) | Times every phase of a copy (listing, opening, creating, data, renaming, phone waits) and prints up to 3 hints | `copy --trace`, `--trace-file t.json` | off (opt-in) |
| `bench copy` (13) | Times explorer, robocopy-1, robocopy-mt, drivedex-v1 and drivedex (or adb-pull for phones) fairly; Markdown rows | `drivedex bench copy <src> <dst> [--rounds 3] [--cold] [--markdown] [--engines ...] [--v2-flags ...]` | — |
| 1.0 mode (13) | Every 2.0 copy feature off | `copy --v1` | off |
| Phone USB link (14) | The phone's USB speed in `drives`, `doctor`, before a phone copy and in the tracer; says when the phone itself is USB 2.0 | — | always on |
| Phone plan from index + delta (15) | Phone copies start from the phone's saved index plus `find -cmin` (only what changed); deleted files reported as *vanished*; the index is refreshed afterwards | `copy_plan_from_index`, `copy --plan auto\|index\|walk`, `--fresh` | auto (index when ≤ 7 days old and the phone supports `-cmin`) |
| Phone tar stream lane (16) | Files < 32 MB stream as one `tar` (≤ 2,000 per request) over `adb exec-out`; big files and unsafe names use `adb pull`; only requested regular files with safe names are written | `phone_copy_mode`, `copy --phone-mode auto\|tar\|pull` | auto (tar when the phone's tar works) — *pending bench (target ≥ 1.5×)* |
| Parallel tar streams (16) | More than one tar stream at once | `phone_tar_streams` | 1 — *pending bench (keep 2 only if ≥ 10 % faster)* |
| Compressed pulls (16) | `adb pull -z` for big .txt .log .json .db .csv .xml .html | `phone_pull_compress` | auto = off — *pending bench* |
| Local plan from index + sweep (17) | Drive copies start from a fresh index (≤ `index_plan_max_age`, ≥ 95 % with size/date); a 2-worker sweep adds new/changed files (“+N found while copying”); NTFS + Administrator refreshes from the journal instead; hard disks list first; the index is refreshed afterwards | `copy_plan_from_index`, `index_plan_max_age`, `copy --plan`, `--fresh` | auto, 15 min — *pending bench* |
| Single-open source (18a) | One open per source file; size/date from the handle | `--v1` | on |
| Unbuffered big files (18b) | Files ≥ 256 MB bypass the RAM cache (aligned 8 MB blocks) | `copy_unbuffered`, `copy --unbuffered auto\|on\|off` | auto = off — *pending bench (≥ 10 % or no stalls)* |
| Media verify (18c) | `--verify` tees the source checksum and re-reads only the copy, unbuffered | `--v1` restores 1.0's double re-read | on (with `--verify`) |
| Copy-speed hints in doctor (18d) | `doctor` explains the last traced copy's hints, incl. the manual antivirus-exclusion steps | — | on when a traced copy exists |
| Direct writes (18e, experimental) | Small files into a brand-new folder go straight to their names; `.drivedex-resume` journal | `copy --experimental-direct` | off — *ships only if ≥ 15 % faster* |
| Worker autotuner (19) | Hill-climbs the worker count every 2 s (1–16, 1–4 with a hard disk) | `copy_autotune`; off with `--workers N` or `--v1` | on — *pending bench (keep only if never slower)* |
| Disk-order reads (20) | NTFS hard-disk sources: small files read in on-disk (LCN) order, ≤ 2 workers; abandoned if the lookup costs > 10 % | `copy_order`, `copy --order auto\|size\|name\|disk` | auto = disk for NTFS hard disks — *pending bench* |

Also: `search --copy-to` uses the same lanes; all new settings are in `config.toml` with comments (`drivedex config edit`); `uninstall --purge` also removes the last trace report.

Safety rules added in 2.0: `bench copy` deletes only inside the `.drivedex-bench-<time>` folder it created (every delete checks the path); DriveDex stays read-only on phones (`find`, `stat`, `tar -c` to stdout, `adb pull` only).

## 1.0.0 — 2026-10-10

First release: index + instant search (also of unplugged drives), NTFS turbo mode, Android phones over ADB, portable devices through Windows, fast resumable copy, interactive search screen, background agent, Explorer right-click menu, doctor, USB port speed check. See [BUILD.md](BUILD.md) and [PROGRESS.md](PROGRESS.md).
