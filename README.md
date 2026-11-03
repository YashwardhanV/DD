# DriveDex

**Instant search and fast copy for external drives, pen drives, memory cards, phones and cameras — even when they're unplugged.**
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

## What's new in 2.0

DriveDex 2.0 is about one complaint: **copying is slow in real use — and phone → PC most of all.** Every new feature has a switch, falls back to the 1.0 behaviour, and `drivedex copy --v1` turns them all off at once. Full list with switches and defaults: [CHANGELOG.md](CHANGELOG.md).

- **Phone → PC as one continuous stream.** Small phone files (photos, WhatsApp media, screenshots) now travel packed together in a `tar` stream over `adb exec-out` — up to 2,000 files per request — instead of `adb pull`'s request-and-wait per file. Big videos keep the resumable pull lane. DriveDex stays **read-only on the phone** (`tar -c` only *creates* an archive, and writes it to the USB link). Only files that were asked for, with safe names, are ever written on the PC.
- **No waiting for a listing.** A phone or drive copy starts from the saved index; the phone is only asked what *changed* since (`find -cmin`), and a drive copy runs a quick sweep alongside that catches new or changed files (*"+37 found while copying"*). Deleted files are reported as *vanished*, not as errors.
- **`copy --trace` shows where the time goes** — listing, creating files, data, renaming, waiting for the phone — with plain-English hints (the USB link is full; antivirus scanning each new file; a hard disk seeking). `drivedex bench copy` times Explorer, robocopy, DriveDex 1.0 and 2.0 on *your* drive, fairly (rotating order, medians, optional unplug/replug between runs), and prints a row for [docs/BENCHMARKS.md](docs/BENCHMARKS.md).
- **Phone USB speed.** `drivedex drives`, `doctor` and every phone copy say how the phone is connected — and whether *the phone's own port* is USB 2.0 (common; then ~40 MB/s is the most any software can get).
- **Fewer round trips per file:** each source file is opened once; `--verify` checks the copy *on the disk*, not in RAM; big files can bypass the RAM cache (`--unbuffered on`); the number of parallel workers adjusts itself while copying; on NTFS hard disks small files are read in the order they sit on the platter.

> **Honest numbers only.** These features are built and tested (in the cloud container, with fake phones and drives), but the speed-ups are **hypotheses until measured on real hardware** — the owner's results go into [docs/BENCHMARKS.md](docs/BENCHMARKS.md), and any feature that doesn't win there stays off by default. The phone target is *≥ 1.5× faster than 1.0 for 5,000 small files*.

## The problem

- **Searching an external drive is slow or impossible.** Windows Explorer re-reads the drive on every search (minutes on a USB hard disk), and finds nothing at all when the drive is in the drawer.
- **Android phones are unusable in Explorer.** Over MTP every folder is a separate round trip to the phone; searching a big camera roll can take forever or hang.
- **Copying many small files is slow and fragile.** One file at a time, no resume after an unplug, no verification.

## What DriveDex does

- **Indexes once, searches in milliseconds.** The list of every file is saved on your PC. Search never touches the drive, so it also works for drives that are *not plugged in* (results are marked `offline` with the drive's name and last letter).
- **Knows drives by their serial number, not by letter.** The pen drive that is `E:` today and `G:` tomorrow is still the same index.
- **Reads phones over ADB** (one `find` command on the phone instead of thousands of MTP requests), and **any other portable device** — a phone in plain "File transfer" mode, an iPhone, a camera — through Windows, with nothing to set up.
- **NTFS "turbo mode":** reads the drive's Master File Table directly (millions of files in seconds) and then refreshes from the NTFS change journal in milliseconds.
- **Tunes itself to the drive:** few parallel readers for a spinning hard disk, many for flash.
- **Fast, resumable copy** with parallel workers (at most two big files at once on USB, so an unplug loses little; the worker count adjusts itself), space reserved up front, atomic file writes, free-space and FAT32 (4 GiB) pre-checks, optional CRC-32C verification read back from the disk — from drives, folders and phones (small phone files stream as one `tar`). Copies start from the saved index instead of re-listing the source.
- **A background agent** maps out anything you plug in, by itself: USB disks and pen drives, memory cards (also in the laptop's own card reader), eSATA/Thunderbolt/FireWire disks, phones, iPhones and cameras. A locked phone is tried again until you unlock it. Internal disks are left alone unless you list them in `auto_index`.
- **Spots a slow USB port.** A USB 3 pen drive in a USB 2.0 port crawls at ~40 MB/s instead of hundreds, and Windows rarely says so. DriveDex reads the USB hub driver's answers and tells you: *this drive can do 5 Gbps but runs at 480 Mbps — this port is USB 2.0 only* (or: a USB 2.0 hub/adapter is in the way, or the cable fell back), how many USB 3 / USB-C ports your PC has, and `drivedex ports --watch` finds them as you move the drive around. It warns before a long copy, and the agent pops up the moment you plug into a slow port.
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
drivedex drives                       # which drives and devices are connected, how, and which are indexed
drivedex index E:                     # scan the pen drive once (use an Administrator terminal for NTFS turbo mode)
drivedex search invoice ext:pdf       # instant search of everything indexed - plugged in or not
drivedex                              # the interactive type-as-you-search screen
drivedex copy E:\Photos D:\Backup     # fast, resumable copy (add --dry-run to preview, --trace to see where time goes)
drivedex copy phone:/DCIM/Camera D:\Phone   # phone -> PC as a tar stream (2.0), resumable
drivedex ports --watch                # which ports are USB 3? plug the drive in each to see its speed
```

Copy options worth knowing (2.0): `--trace` (where the time went + hints), `--plan walk`/`--fresh` (list the source again instead of using the index), `--phone-mode tar|pull`, `--order size|name|disk`, `--unbuffered on`, `--v1` (copy exactly like 1.0), and `drivedex bench copy <src> <dst-folder> --cold --markdown` to measure it all on your own drives.

More: `drivedex index phone` (Android over USB debugging), `drivedex index device` (any phone, iPhone or camera in Explorer's This PC), `drivedex search IMG --paths-only`, `drivedex doctor --bench E:`, `drivedex agent start`, `drivedex shell install`. Every command has `--help` with examples.

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
| `drive:E` `drive:phone` `drive:device` `drive:"SANDISK 32GB"` | one drive (letter, any phone, any portable device, or label) | `drive:phone IMG` |
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
5. **Phones use ADB**, not MTP: one `find … -exec stat` runs on the phone and streams back the whole list. Devices without USB debugging (and iPhones, cameras) are read through the Windows Shell instead — slower, folder by folder, which is why the agent does it in the background.
6. **What counts as external:** USB, FireWire and SD/MMC drives, anything Windows calls removable, and any disk Windows reports as hot-pluggable (eSATA, Thunderbolt enclosures). Never the Windows drive or a mounted `.vhdx`.

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
| A USB 3 pen drive copies at only ~30–40 MB/s | `drivedex ports` — it is probably on a USB 2.0 port, hub or adapter; it says which, and `--watch` finds the USB 3 ports. No software can make a USB 2.0 port faster. If it's on USB 3 and still slow, `drivedex doctor --bench E:`: many cheap "USB 3" sticks write no faster than USB 2.0. |
| Copy fails on files over 4 GB | The destination is FAT32. Reformat to exFAT/NTFS (back up first). |
| Indexing is slow on a USB hard disk | Expected with a spinning disk. Use NTFS turbo mode (Administrator) and keep workers at 2. |
| Right-click menu missing on Windows 11 | It is under **Show more options** (or hold Shift while right-clicking). |

## Limitations

- **iPhones, cameras and phones without USB debugging** are indexed and searchable, but DriveDex doesn't copy files *off* them — open them in Explorer (This PC → the device). An iPhone shows only its photos (`DCIM`), as in Explorer.
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
internal/phone      ADB: listing, stat, pull, tar streams, capability probes (with a fake phone for tests)
internal/portable   phones, iPhones, cameras through Windows (MTP/PTP): detection and listing
internal/volume     drive identity (serial, label, bus type, HDD/flash, USB link speed and ports)
internal/copier     plan/run copy engine: index planning + sweep, local and phone lanes, autotuner, disk order
internal/copier/trace  where a copy's time goes (--trace) and the hints
internal/benchcopy  fair copy benchmarks (drivedex bench copy)
internal/tui        the interactive screen (Bubble Tea)
internal/agent      background auto-index agent
internal/doctor     diagnostics and the write-speed test
internal/config     config.toml
internal/install    install / uninstall helpers
```

Built stage by stage from [BUILD.md](BUILD.md) (1.0) and [docs/BUILD-V2.pdf](docs/BUILD-V2.pdf) (2.0); [PROGRESS.md](PROGRESS.md) records every decision, every deviation from the plan and every check that still needs real hardware.

## License

[MIT](LICENSE) © Yashwardhan Verma
