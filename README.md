# DriveDex

**Instant search and fast copy for external drives, pen drives and Android phones — even when they're unplugged.**
A single Windows program written in Go (no CGO, no extra DLLs).

<!--
  DEMO GIF GOES HERE. Record it with ScreenToGif (https://www.screentogif.com/):
  1. Open ScreenToGif -> Recorder, drag the frame over a PowerShell / Windows Terminal window.
  2. Press Record, run:  drivedex   then type a few letters, scroll with the arrow keys, press Esc.
     Also worth showing:  drivedex search "ext:jpg size:>2mb"   and   drivedex copy ...
  3. Stop, trim, save as docs/demo.gif (keep it under ~5 MB: 10-15 s, 800 px wide), then
     replace this comment with:   ![DriveDex demo](docs/demo.gif)
-->

> 📽️ *Demo GIF to be added — see the comment in this file's source for how to record it.*

## The problem

- **Searching an external drive is slow or impossible.** Windows Explorer re-reads the drive on every search (minutes on a USB hard disk), and finds nothing at all when the drive is in the drawer.
- **Android phones are unusable in Explorer.** Over MTP every folder is a separate round trip to the phone; searching a big camera roll can take forever or hang.
- **Copying many small files is slow and fragile.** One file at a time, no resume after an unplug, no verification.

## What DriveDex does

- **Indexes once, searches in milliseconds.** The list of every file is saved on your PC. Search never touches the drive, so it also works for drives that are *not plugged in* (results are marked `offline` with the drive's name and last letter).
- **Knows drives by their serial number, not by letter.** The pen drive that is `E:` today and `G:` tomorrow is still the same index.
- **Reads phones over ADB** (one `find` command on the phone instead of thousands of MTP requests).
- **NTFS "turbo mode":** reads the drive's Master File Table directly (millions of files in seconds) and then refreshes from the NTFS change journal in milliseconds.
- **Tunes itself to the drive:** few parallel readers for a spinning hard disk, many for flash.
- **Fast, resumable copy** with parallel workers, atomic file writes, free-space and FAT32 (4 GiB) pre-checks, optional CRC-32 verification — from drives, folders and phones.
- **A background agent** re-indexes USB drives and phones the moment you plug them in.
- **Explorer right-click menu**, an interactive type-as-you-search screen, a power query language, `--json` output for scripts, and a `doctor` that explains why things are slow.

## Performance

Measured numbers (full details and honest caveats in [docs/BENCHMARKS.md](docs/BENCHMARKS.md)):

| What | Result | Target | Where measured |
|---|---|---|---|
| Search 1,000,000 entries, ranked, in memory | 6 – 22 ms (simple words) · 20 – 72 ms (with `ext:`/`size:`/`path:`/regex) | < 100 ms | 4-vCPU cloud container |
| Load a 1,000,000-entry index from disk (≈ 38 MB file) | 380 ms | < 1.5 s | 4-vCPU cloud container |
| Index a 20,000-file folder | 126 ms | — | cloud container (RAM-backed disk) |

> ⚠️ Those are CPU/memory numbers from a cloud container. **The numbers that matter — DriveDex vs Explorer on a real USB drive and phone — are being collected on real hardware** and will be added to [docs/BENCHMARKS.md](docs/BENCHMARKS.md) with the exact drive, port and PC used. Nothing in this README claims a speed-up on real drives that hasn't been measured.

## Install

Requires Windows 10/11 (x64 or ARM64). No administrator rights needed.

**From a release** — download `drivedex-<version>-windows-<arch>.zip` from the [Releases](../../releases) page, unzip it, open PowerShell in that folder and run:

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

(or just `.\drivedex.exe install`). It asks **y/N before every change**: copy the programs to `%LOCALAPPDATA%\Programs\DriveDex`, add that folder to your user `PATH`, add the right-click menu (optional), start the background agent at log-in (optional), then run `drivedex doctor`. Open a **new** terminal afterwards and type `drivedex`. `drivedex uninstall` reverses everything and asks whether to keep your indexes and settings.

> Windows SmartScreen may warn about the programs because they aren't code-signed: click **More info → Run anyway**, or build from source below.

**From source** — needs [Go](https://go.dev/dl/) 1.26 or newer (Go downloads the exact toolchain by itself if yours is older):

```powershell
git clone https://github.com/YashwardhanV/DD.git
cd DD
powershell -ExecutionPolicy Bypass -File scripts\build.ps1   # vet + test + build; zips for amd64 and arm64 in bin\
.\bin\drivedex.exe install
```

## Quick start

```powershell
drivedex drives                       # which drives are connected, and which are indexed
drivedex index E:                     # scan the pen drive once (use an Administrator terminal for NTFS turbo mode)
drivedex search invoice ext:pdf       # instant search of everything indexed - plugged in or not
drivedex                              # the interactive type-as-you-search screen
drivedex copy E:\Photos D:\Backup     # fast, resumable copy (add --dry-run to preview)
```

More: `drivedex index phone` (Android), `drivedex search IMG --paths-only`, `drivedex doctor --bench E:`, `drivedex agent start`, `drivedex shell install`. Every command has `--help` with examples.

## Search syntax

Words must all appear in the file name (not case sensitive). Combine anything:

| Syntax | Meaning | Example |
|---|---|---|
| `word` | name contains it (all words must match) | `resume pdf` |
| `"two words"` | phrase | `"final report"` |
| `!word` | leave out names containing it | `invoice !draft` |
| `ext:a,b` | extension is one of | `ext:jpg,png` |
| `type:file` / `type:dir` | only files / only folders | `type:dir photos` |
| `size:>100mb` `size:<1kb` `size:10mb..1gb` | size (b/kb/mb/gb, 1024-based) | `size:>1gb ext:mp4` |
| `modified:>2025-01-01` `modified:<30d` | date, or "within the last N days" | `modified:<7d` |
| `drive:E` `drive:phone` `drive:"SANDISK 32GB"` | one drive (letter, any phone, or label) | `drive:phone IMG` |
| `path:word` | match the whole path, not just the name | `path:whatsapp` |
| `re:<regex>` | Go regular expression on the name | `re:^IMG_\d{8}` |

For scripts: `--json` prints an array of `{path, name, size, modified, source_id, label, online}`, `--paths-only` prints one path per line, and `--copy-to <folder>` copies **every** match (asks first if there are more than 100).

```powershell
drivedex search ext:jpg size:>2mb --json | ConvertFrom-Json | Select-Object -First 5 path, size
```

## How it works

1. **An index is a saved file list.** Each drive/phone gets one compact `.ddx` file (CRC-protected, written atomically). All indexes are loaded into memory as flat arrays and searched in parallel.
2. **Paths are rebuilt from parent IDs.** An entry stores only its name and its parent's ID, so the index stays small and renaming a folder changes one entry, not a million paths.
3. **NTFS turbo mode** enumerates the Master File Table (`FSCTL_ENUM_USN_DATA`), then replays the change journal (`FSCTL_READ_USN_JOURNAL`) to refresh — "read the library catalogue, then only the diary entries since last time".
4. **The scanner adapts to the drive:** it asks Windows whether the drive has a seek penalty (spinning disk) and picks 2 / many / 4 workers for HDD / flash / unknown.
5. **Phones use ADB**, not MTP: one `find … -exec stat` runs on the phone and streams back the whole list.

The long, beginner-friendly version (file systems, MFT, USN journal, MTP vs ADB, HDD vs flash…) is in [docs/HOW-IT-WORKS.md](docs/HOW-IT-WORKS.md).

## Settings

`drivedex config edit` opens `%APPDATA%\DriveDex\config.toml` (created on first run, every option explained): `default_limit`, `show_hidden`, `workers`, `skip_dirs` / `skip_dirs_extra` (e.g. `["node_modules", ".git"]`), `stale_after`, `notifications`, `auto_index`, `adb_path`, `phone_root`, `color`. `drivedex config show` prints what is in force.

## PowerShell tab completion

```powershell
drivedex completion powershell | Out-String | Invoke-Expression      # this session only
```

To enable it in every session, run `notepad $PROFILE` and add that same line to the file (create the file if PowerShell says it doesn't exist).

## Troubleshooting

Start with `drivedex doctor` — it checks everything below and prints the fix.

| Problem | Fix |
|---|---|
| `adb` not recognized / phone not found | Install Platform-Tools (`drivedex index phone` prints the steps) and open a new terminal, or set `adb_path` in the config. |
| Phone shows `unauthorized` | Unlock the phone, tap **Allow** on the USB-debugging prompt, run `adb kill-server`, replug. |
| "Access is denied" opening `\\.\E:` | NTFS turbo mode needs an Administrator terminal. Without it DriveDex falls back to the normal scanner automatically. |
| `ERROR_JOURNAL_NOT_ACTIVE` / "no change journal" | `drivedex journal enable E:` (Administrator), or just use `--mode parallel`. |
| Results show an old drive letter | Run `drivedex drives`; letters are resolved by serial number at search time. A reformatted drive is a new drive (new index). |
| SmartScreen warns about the exe | Unsigned binaries trigger this: **More info → Run anyway**, or build it yourself. |
| Copy fails on files over 4 GB | The destination is FAT32. Reformat to exFAT/NTFS (back up first). |
| Indexing is slow on a USB hard disk | Expected with a spinning disk. Use NTFS turbo mode (Administrator) and keep workers at 2. |
| Right-click menu missing on Windows 11 | It is under **Show more options** (or hold Shift while right-clicking). |

## Limitations

- **iPhone is not supported** (no ADB equivalent); Android only.
- **NTFS turbo mode needs an Administrator terminal** (Windows only lets administrators read a drive's file table). Everything else works without.
- **Cloned drives share a serial number**, so they share one index. Reformatting a drive gives it a new serial (and a new index).
- **Only files on a phone are listed** by `find -type f`; folders that contain no files don't appear. Android also hides some folders (such as `Android/data`) from ADB.
- **Windows 11:** the right-click entries live under *Show more options*; the compact menu can only be extended by packaged apps.
- DriveDex is **read-only on your drives** — only `copy` and `doctor --bench` ever write (and bench removes its own temp file). It never writes to a phone.

## Project layout

```
cmd/drivedex, cmd/drivedex-agent   the two programs (agent has no console window)
internal/index      index model, .ddx file format, store, parallel ranked search
internal/query      the search language (parser + matcher)
internal/scan       walk and parallel directory scanners
internal/ntfs       MFT enumeration and USN-journal refresh (with an in-memory fake NTFS for tests)
internal/phone      ADB: listing, stat, pull (with a fake phone for tests)
internal/volume     drive identity (serial, label, bus type, HDD/flash)
internal/copier     plan/run copy engine
internal/tui        the interactive screen (Bubble Tea)
internal/agent      background auto-index agent
internal/doctor     diagnostics and the write-speed test
internal/config     config.toml
internal/install    install / uninstall helpers
```

Built stage by stage from [BUILD.md](BUILD.md); [PROGRESS.md](PROGRESS.md) records every decision, every deviation from the plan and every check that still needs real hardware.

## License

[MIT](LICENSE) © Yashwardhan Verma
