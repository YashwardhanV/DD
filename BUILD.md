# DriveDex — BUILD.md

> **DriveDex** is a fast search-and-copy tool for external hard disks, pen drives and Android phones on Windows, written in Go.
> This document is the single source of truth for building it. It is written to be executed stage by stage by **Claude Code**, for an owner who is a beginner and is "vibecoding" the project.

---

## Roadmap at a glance

| Stage | Name | What the user gets | Core concept learned |
|---|---|---|---|
| 0 | Project setup | Repo, build script, `drivedex version` | Go modules, build flags |
| 1 | Basic scanner | `drivedex scan E:\` lists every file | Directory walking, recursion |
| 2 | Index + search | `drivedex index` / `drivedex search` — instant search | Indexing, binary file formats, caching |
| 3 | Drive identity | Drives remembered across plug-ins; search unplugged drives | Volume serial numbers, Windows APIs |
| 4 | Parallel scanner | 3–10× faster indexing on flash/SSD | Goroutines, worker pools, HDD vs SSD |
| 5 | Android phones | Index + search a phone in seconds | ADB vs MTP, parsing external tool output |
| 6 | NTFS turbo mode | Index millions of files in seconds, update in milliseconds | MFT, USN change journal, IOCTLs |
| 7 | Fast copy | `drivedex copy` — parallel, resumable, phone-aware | Parallel I/O, atomic writes |
| 8 | Interactive UI | Type-as-you-search terminal UI | TUI, event loops |
| 9 | Auto-index agent | Drives index themselves when plugged in | Background processes, device detection |
| 10 | Explorer integration | Right-click → "Search with DriveDex" | Windows registry |
| 11 | Doctor, config, power search | `doctor` diagnoses slow USB; `ext:` `size:` filters; JSON output | Diagnostics, query parsing |
| 12 | Packaging & release | One-command install, GitHub release, README | Distribution, CI |

Stages 1–7 are the core tool. Stages 8–12 make it easy to use.

---

## 0. Instructions for Claude Code (read first, follow always)

1. **Read this entire document once before writing any code.**
2. **Execute stages strictly in order** (0 → 12). Every stage has: Goal, Why, Files, Implementation spec, Commands, Acceptance criteria, Commit.
3. **A stage is DONE only when every acceptance criterion passes.** At the end of every stage run:
   ```
   go build ./...
   go vet ./...
   go test ./...
   ```
   All three must be clean.
4. **Maintain `PROGRESS.md`** at the repo root (template in Appendix A). Update it after every stage: checklist, decisions, deviations, blockers, benchmark numbers. If a session restarts, read `PROGRESS.md` first and resume from the first unchecked stage.
5. **Commit after each stage:** `git add -A && git commit -m "stage N: <title>"`.
6. **The owner is a beginner.** After each stage, tell them in plain English:
   - what was built,
   - the exact command to try it,
   - the one concept this stage teaches (2–3 sentences).
7. **Always ask the owner before:**
   - installing any software (Go, Git, adb, Go tools),
   - anything that needs an Administrator terminal,
   - editing the registry, PATH, or Windows startup entries,
   - enabling the USN journal on a drive,
   - writing benchmark files to a user's drive,
   - creating a GitHub repo or pushing.
8. **Safety rules (non-negotiable):**
   - DriveDex is **read-only** on drives it indexes.
   - Only two commands write to user drives: `copy` (to the destination the user named) and `doctor --bench` (one temp file, deleted after).
   - Never delete user files.
   - Never disable Windows Defender or change security settings.
   - `forget` deletes only DriveDex's own index files.
9. **No CGO.** Build with `CGO_ENABLED=0`. Never add a dependency that needs gcc/CGO. The final output must be a single `.exe` that runs on any Windows 10/11 x64 PC.
10. **Shell.** Commands below are written for PowerShell. If your shell is Bash (e.g. Git Bash), either translate them (env vars, quoting, backslashes) or run them via `powershell -NoProfile -Command "..."`.
11. **When the spec is wrong.** If something in this document is technically incorrect or doesn't work on this machine, do not silently hack around it. Write it under "Deviations" in `PROGRESS.md`, pick the closest working approach, and tell the owner.
12. **Hardware-dependent checks.** If an acceptance criterion needs hardware or privileges you don't have (phone, external drive, admin terminal), ask the owner to plug it in or run the command and paste the output. Never mark such a criterion as passed without evidence.
13. **Code style:**
    - Simple and heavily commented — the owner wants to learn from the code.
    - Every package gets a doc comment explaining in plain English what it does and why.
    - Prefer clarity over cleverness, except in code marked `// PERF:`.
    - Errors are wrapped with context: `fmt.Errorf("reading %s: %w", p, err)`.
    - User-facing errors are friendly, one line, with a fix hint. No stack traces unless `--debug`.
14. **Cancellation.** Every long operation takes a `context.Context`. `main` creates it with `signal.NotifyContext(ctx, os.Interrupt)` so Ctrl+C stops cleanly.

---

## 1. Problem statement

When a USB hard disk, a pen drive or an Android phone is connected to a Windows PC:

- **Search is painfully slow.** File Explorer takes minutes to search an external drive and often looks frozen.
- **Copying is slow.** Folders with many small files (photos, WhatsApp media, code projects) copy far below the drive's rated speed.
- **Phones are the worst case.** Browsing lags, search is nearly unusable, and large copies sometimes fail midway.

**Goal:**
- Search on any connected storage feels instant (under 1 second after the first index).
- Search works even for drives that are *not plugged in* ("which pen drive had my resume?").
- Copies run as fast as the hardware allows, resume after interruption, and work the same way for phones.
- All of it through one simple tool: a CLI, an interactive search screen, and a right-click menu.

---

## 2. Root-cause analysis

| Symptom | Real cause |
|---|---|
| Explorer search on an external drive is slow | Windows Search only indexes chosen locations (by default, user folders on internal drives). External drives aren't indexed, so Explorer crawls live: it opens every folder and reads every entry, and if "search file contents" is on, it reads files too. That's thousands to millions of tiny I/O round trips over USB. |
| Even worse on a USB hard disk (HDD) | An HDD has a spinning platter and a moving arm. Jumping to each folder can cost a physical seek of ~5–15 ms. 50,000 folders × 10 ms ≈ 8+ minutes. |
| Slow on pen drives | Cheap flash has poor random-read performance. FAT32/exFAT (the usual pen-drive file systems) have no central file table, so the only way to list files is folder by folder. |
| Phone is extremely slow | Phones connect over **MTP**, not as a disk. Every folder listing and every file is a separate request/response. Explorer's MTP layer handles one request at a time and keeps no index. |
| Copy is slow | Several causes stack up:<br>• per-file overhead dominates when there are many small files;<br>• a USB 2.0 port caps throughput at ~35–40 MB/s;<br>• Windows Defender scans each new file;<br>• the default "Quick removal" policy disables write caching on external drives;<br>• phones go through MTP. |

**Key insight:** the matching algorithm is not the bottleneck — disk I/O is.

- Comparing a search term against 1 million file names **in memory** takes tens of milliseconds.
- Reading those same names from a USB disk **folder by folder** takes minutes.

So the fix is:
1. Read the drive's file list as efficiently as possible, once.
2. Save it (an **index**).
3. Search the saved copy.
4. Keep it fresh cheaply.

---

## 3. The solution: DriveDex

### Five design ideas

1. **Index once, search instantly.** Search never touches the drive; it reads a compact index file kept on the PC.
2. **Remember every drive by identity, not drive letter.** Drive letters change between plug-ins (E: today, F: tomorrow). Every volume has a *serial number* that doesn't. Indexes are keyed by serial, and paths are stored relative to the volume root. Indexes of unplugged drives stay searchable.
3. **Use the fastest scanner for each kind of storage:**

   | Storage | Scanner |
   |---|---|
   | NTFS (most external HDDs) | Read the Master File Table directly via `FSCTL_ENUM_USN_DATA` (seconds for millions of files). Keep it fresh by reading only the USN change journal. |
   | FAT32 / exFAT (pen drives, SD cards) | Parallel directory walk, tuned automatically: few workers for spinning HDDs, many for flash. |
   | Android phone | One ADB command makes the *phone itself* list all files and stream the list back in a single response. MTP is bypassed entirely. |

4. **Fast copy:**
   - parallel workers and large buffers;
   - resumable (skips files already copied);
   - atomic (never leaves half-written files under the real name);
   - phone copies via `adb pull`.
5. **Easy to use:** interactive type-as-you-search UI, auto-indexing on plug-in, an Explorer right-click menu, a `doctor` command that explains why your USB is slow, and a one-line install.

### What makes it worth building
Tools like *Everything* (voidtools) are excellent for NTFS. DriveDex's angle is **one tool, one search box across NTFS drives, FAT/exFAT pen drives and Android phones, including drives that are currently unplugged**, plus a copy engine and diagnostics. It's also a deep, explainable systems project.

---

## 4. Architecture

```
                 ┌────────────── drivedex.exe (CLI + TUI) ───────────────┐
   user  ─────▶  │ search · tui · index · copy · drives · list · doctor  │
                 └──────┬──────────────────┬──────────────────┬──────────┘
                        │                  │                  │
                 ┌──────▼─────┐   ┌────────▼────────┐   ┌─────▼──────┐
                 │   query    │   │ scanner (auto-  │   │   copier   │
                 │ parse+match│   │ selected)       │   │ parallel / │
                 └──────┬─────┘   │ walk | parallel │   │ adb pull   │
                        │         │ | mft | adb     │   └────────────┘
                        │         └────────┬────────┘
                 ┌──────▼──────────────────▼──────────────┐
                 │ index store                            │
                 │ %LOCALAPPDATA%\DriveDex\indexes\*.ddx  │
                 └────────────────────────────────────────┘

   drivedex-agent.exe (background, no window):
     watches for plugged-in drives/phones → refreshes their index → (optional) serves searches from memory
```

**Data flow:**
1. A drive is plugged in.
2. Identify the volume: serial, label, file system, bus type, HDD or flash.
3. Choose the scanner.
4. Build or refresh the index and save the `.ddx` file atomically.
5. `search` loads the `.ddx` files (or asks the agent) and matches in memory.
6. Results are shown with the drive's *current* letter, or marked offline.

---

## 5. Tech stack

- **Go:** latest stable release (minimum 1.22). `CGO_ENABLED=0` always.
- **Dependencies** (install the latest versions with `go get`):

  | Package | Purpose | Stage |
  |---|---|---|
  | `github.com/spf13/cobra` | CLI commands, help, shell completion | 0 |
  | `golang.org/x/sys/windows` and `golang.org/x/sys/windows/registry` | Windows APIs | 3 |
  | `github.com/charmbracelet/bubbletea`, `github.com/charmbracelet/bubbles`, `github.com/charmbracelet/lipgloss` | Terminal UI | 8 |
  | `github.com/atotto/clipboard` | Copy path to clipboard | 8 |
  | `github.com/Microsoft/go-winio` | Named pipe for the agent | 9, optional |
  | `fyne.io/systray` | Tray icon | 9, optional — verify CGO-free on Windows first; if not, skip the tray |
  | `github.com/BurntSushi/toml` | Config file | 11 |
  | `github.com/tc-hib/go-winres` | Embed icon + manifest into the exe (as a `go install` tool) | 12, optional |

  Everything else: the standard library.
- **External tool:** `adb.exe` (Android SDK Platform-Tools), for phone features only.

---

## 6. Repository layout (final state)

```
drivedex/
├─ cmd/
│  ├─ drivedex/main.go          # CLI entry point
│  ├─ drivedex-agent/main.go    # background agent (built with -H=windowsgui)
│  └─ genfixture/main.go        # test-data generator (dev tool)
├─ internal/
│  ├─ cli/          # cobra commands, one file per command
│  ├─ scan/         # Scanner interface, single + parallel directory walkers
│  ├─ ntfs/         # MFT enumeration + USN journal (windows only)
│  ├─ volume/       # list drives, serials, FS type, bus type, seek penalty (windows only)
│  ├─ index/        # data model, .ddx file format, store, search, path building
│  ├─ query/        # query parser (terms, ext:, size:, ...)
│  ├─ phone/        # adb discovery, device listing, phone scanner, pull/push
│  ├─ copier/       # parallel resumable copy
│  ├─ tui/          # bubbletea interactive search
│  ├─ agent/        # plug-in watcher, refresh scheduler, optional pipe server
│  ├─ shellint/     # Explorer context menu + startup registration (registry)
│  ├─ doctor/       # diagnostics + USB write benchmark
│  ├─ config/       # config file load/save/defaults
│  └─ ui/           # shared output helpers: colors, human sizes, progress line
├─ scripts/
│  ├─ build.ps1
│  └─ install.ps1
├─ docs/
│  ├─ HOW-IT-WORKS.md
│  └─ BENCHMARKS.md
├─ .github/workflows/ci.yml
├─ BUILD.md        # this file
├─ PROGRESS.md
├─ README.md
├─ LICENSE         # MIT
└─ go.mod
```

**Build tags:**
- Files that call Windows APIs start with `//go:build windows`.
- For each such package, add a `_other.go` stub (`//go:build !windows`) that returns `errors.ErrUnsupported`. This lets the portable packages (`index`, `query`, `scan` walker, `phone` parser, `copier`) compile and unit-test anywhere.

---

## 7. Core data model (shared by every stage)

### 7.1 Entry and IDs

Paths are **not** stored in full. Each entry stores its own name plus the ID of its parent folder — exactly how NTFS stores files internally. Benefits:
- the index is small;
- renaming or moving a folder updates one entry, not thousands;
- paths are rebuilt only for results being displayed.

```go
// package index

const (
    FlagDir      uint8 = 1 << 0
    FlagHidden   uint8 = 1 << 1
    FlagSystem   uint8 = 1 << 2
    FlagNoMeta   uint8 = 1 << 3 // size/mtime unknown (filled lazily on display)
    FlagDeleted  uint8 = 1 << 7 // tombstone, in-memory only; never written to disk
)

type Entry struct {
    ID       uint64 // unique within one index. Walkers: sequential from 1. NTFS: file reference number.
    ParentID uint64 // 0 = direct child of the root. The root itself is never stored as an entry.
    Name     string // single path component, original case, UTF-8
    Size     int64  // bytes; -1 = unknown; 0 for directories
    ModTime  int64  // unix seconds; 0 = unknown
    Flags    uint8
}
```

### 7.2 In-memory index (struct-of-arrays, for cache-friendly fast search)

```go
type Index struct {
    Header  Header
    IDs     []uint64
    Parents []uint64
    Names   []string
    Lower   []string        // lowercase names, computed at load time (parallel), used for matching
    Sizes   []int64
    MTimes  []int64
    Flags   []uint8
    byID    map[uint64]int32 // ID -> slot; built at load
}
```

Methods:

| Method | Notes |
|---|---|
| `Len()` | |
| `Add(Entry)` | |
| `Upsert(Entry)` | Stage 6 |
| `Delete(id)` | Sets the tombstone; stage 6 |
| `Compact()` | Removes tombstones before save |
| `RelPath(slot) string` | Walks parent IDs up to 0 and joins with `\`. Use a per-call `map[uint64]string` cache for parent folders when building many paths. If a parent ID is missing (orphan), return `"?\" + name`. |

### 7.3 Header (stored as JSON inside the file — easy to extend)

```go
type Header struct {
    FormatVersion int       `json:"format_version"` // 1
    SourceKind    string    `json:"source_kind"`    // "folder" | "volume" | "phone"
    SourceID      string    `json:"source_id"`      // "dir-<12 hex>" | "vol-1A2B3C4D" | "adb-<serial>"
    Label         string    `json:"label"`          // volume label, device product name, or phone model
    FileSystem    string    `json:"file_system"`    // "NTFS" | "exFAT" | "FAT32" | "android" | ...
    RootPath      string    `json:"root_path"`      // folder sources: absolute path; phone: "/sdcard"
    LastMount     string    `json:"last_mount"`     // last seen drive root like "E:\" (volume) or adb serial (phone)
    Scanner       string    `json:"scanner"`        // "walk" | "parallel" | "mft" | "adb"
    EntryCount    int       `json:"entry_count"`
    TotalBytes    int64     `json:"total_bytes"`    // volume capacity, if known
    CreatedAt     time.Time `json:"created_at"`
    UpdatedAt     time.Time `json:"updated_at"`
    ScanMillis    int64     `json:"scan_millis"`
    UsnJournalID  uint64    `json:"usn_journal_id,omitempty"` // stage 6
    NextUsn       int64     `json:"next_usn,omitempty"`       // stage 6
}
```

### 7.4 The `.ddx` file format (version 1, little-endian)

| Offset | Field |
|---|---|
| 0 | magic `"DDX1"` (4 bytes) |
| 4 | `uint16` format version = 1 |
| 6 | `uint32` H = header JSON length |
| 10 | header JSON (H bytes, UTF-8) |
| 10+H | `uvarint` N = entry count |
| … | N × entry:<br>`uvarint` id<br>`uvarint` parent_id<br>`byte` flags (never includes `FlagDeleted`)<br>`varint` size (signed)<br>`varint` mtime (signed)<br>`uvarint` name_len<br>name bytes (UTF-8) |
| end−4 | `uint32` CRC-32 (IEEE) of every preceding byte |

Rules:
- **Writing:** use `bufio.Writer` (1 MB) wrapped in `io.MultiWriter` with a `crc32` hasher.
- **Atomic save:** write `<id>.ddx.tmp`, close, then `os.Rename` over `<id>.ddx`. A crash never corrupts an existing index.
- **Reading:** `os.ReadFile` the whole file, verify magic, version and CRC, then parse. Any mismatch returns `ErrCorruptIndex`; the CLI suggests re-indexing.
- **Storage dir:** `filepath.Join(os.UserCacheDir(), "DriveDex", "indexes")`, which resolves to `%LOCALAPPDATA%\DriveDex\indexes`. Create it if missing.
- **File name:** `<SourceID>.ddx`.

### 7.5 Scanner interface

```go
// package scan
type Progress struct{ Entries, Dirs, Errors int64; Current string }

type Scanner interface {
    Name() string // "walk", "parallel", "mft", "adb"
    Scan(ctx context.Context, b *index.Builder, report func(Progress)) error
}
```

- `index.Builder` wraps `Index` for construction and is **not** thread-safe.
- Parallel scanners send batches to one collector goroutine that owns the builder.
- `report` is called at most every 200 ms. The CLI renders it as one updating line: `Indexing E:\ … 182,334 entries (41,200/s)`.

### 7.6 Displaying a result path

| Source state | Display |
|---|---|
| Volume, currently mounted | current drive root (found by serial) + `RelPath`, e.g. `F:\Photos\2024\img.jpg` |
| Volume, not mounted | `[SANDISK 32GB · offline · last seen E:] \Photos\2024\img.jpg` |
| Phone | `phone:Pixel 7/DCIM/Camera/IMG_20240101.jpg` (forward slashes); append `· offline` if not connected |
| Folder source | `RootPath` + `RelPath` |

---

# STAGES

Every stage uses the same structure: **Goal → Why → Files → Spec → Commands → Acceptance → Commit**.

---

## Stage 0 — Project setup

**Goal:** an empty but buildable project with a version command, build script and progress tracking.

**Why:** a clean skeleton makes every later stage a small, testable step.

### Spec

1. **Check prerequisites:** `go version` and `git --version`.
   - If either is missing, **ask the owner**, then install with `winget install --id GoLang.Go -e` / `winget install --id Git.Git -e`. Verify the IDs first with `winget search golang` / `winget search git`.
   - After installing, the terminal may need restarting so PATH updates.
2. **Module path:** ask the owner for their GitHub username. Module path: `github.com/<username>/drivedex`. If they have none, use `drivedex`.
3. **Create the folder:**
   ```powershell
   mkdir drivedex; cd drivedex
   git init
   go mod init github.com/<username>/drivedex
   go get github.com/spf13/cobra@latest
   ```
4. **`cmd/drivedex/main.go`:**
   - `var version = "dev"` (overridden by ldflags);
   - build the root cobra command in `internal/cli/root.go`;
   - `signal.NotifyContext` for Ctrl+C;
   - exit code 1 on error.
5. **`internal/cli/root.go`:**
   - root command `drivedex` with a short description;
   - global flags `--debug` and `--no-color`;
   - a `version` subcommand printing `drivedex <version> (<GOOS>/<GOARCH>, <go version>)`.
6. **`.gitignore`:** `bin/`, `*.exe`, `*.ddx`, `*.tmp`, `testdata/generated/`, `*.syso` (until stage 12 decides).
7. **`scripts/build.ps1`:**
   ```powershell
   $ErrorActionPreference = "Stop"
   $v = (git describe --tags --always --dirty 2>$null); if (-not $v) { $v = "dev" }
   $env:CGO_ENABLED = "0"
   go vet ./...
   go test ./...
   go build -trimpath -ldflags "-s -w -X main.version=$v" -o bin/drivedex.exe ./cmd/drivedex
   Write-Host "Built bin/drivedex.exe ($v)"
   ```
8. **`PROGRESS.md`:** from Appendix A.
9. **`README.md`:** a stub (one paragraph + "work in progress").
10. **`LICENSE`:** MIT, with the owner's name.

### Commands
```powershell
powershell -ExecutionPolicy Bypass -File scripts/build.ps1
.\bin\drivedex.exe version
.\bin\drivedex.exe --help
```

### Acceptance
- [ ] `bin\drivedex.exe version` prints a version line.
- [ ] `go vet ./...` and `go test ./...` are clean.
- [ ] `PROGRESS.md` exists with stage 0 checked.

**Commit:** `stage 0: project setup`

---

## Stage 1 — Basic scanner (walk and print)

**Goal:** `drivedex scan <path>` walks a folder or drive and prints every file, plus a summary.

**Why:** this is the "walk every shelf" baseline. Everything later is measured against it.

### Files
- `internal/scan/walk.go`
- `internal/scan/walk_test.go`
- `internal/cli/scan.go`
- `cmd/genfixture/main.go`

### Spec

**1. `scan.Walk`**
- Uses `filepath.WalkDir`, single-threaded. It implements the `Scanner` interface (7.5) and is also usable standalone.
- **IDs:**
  - keep `map[string]uint64` from directory relative path to ID;
  - the root has ID 0;
  - each new entry gets `nextID++` (starting at 1);
  - its parent is looked up by `filepath.Dir(rel)`.
  - `WalkDir` visits a directory before its children, so the parent is always known.
- **Metadata:** call `d.Info()` for size and mtime. On Windows this data comes from the directory listing itself, so it's cheap. From `info.Sys().(*syscall.Win32FileAttributeData).FileAttributes`:
  - hidden = `0x2`
  - system = `0x4`
- **Default skip list** (case-insensitive, at any depth): `System Volume Information`, `$RECYCLE.BIN`, `$Recycle.Bin`. Make the list configurable later (stage 11).
- **Errors** (permission denied, etc.):
  - count them and remember the first 20 paths;
  - if the error is on a directory, return `filepath.SkipDir`;
  - never abort the whole scan. Only a root-level failure is fatal.
- **Links:** never follow symlinks or junctions (reparse points). `WalkDir` already doesn't.
- **Cancellation:** check `ctx.Err()` every 1,000 entries.

**2. `drivedex scan <path> [--count-only] [--limit N]`**
- Prints relative paths (unless `--count-only`).
- Then prints a summary:
  ```
  Scanned E:\ in 41.2s — 182,334 files, 9,120 folders, 3 errors (4,425 entries/s)
  ```
- Shows the first few errors with a hint ("some folders are protected by Windows; this is normal").

**3. `cmd/genfixture` — deterministic test-tree generator**

Flags:
- `-out <dir>`
- `-files N` (default 20000)
- `-depth D` (default 4)
- `-fanout F` (default 8)
- `-seed S` (default 1)

Each file contains a few random bytes (size 0–4 KB). The generated tree must include:
- names with spaces, Unicode (`résumé.pdf`, `फोटो.jpg`, `写真.png`) and mixed case;
- at least 5 files with a full path longer than 260 characters;
- some empty folders.

It prints the exact file and folder counts it created.

**4. Tests** (in `t.TempDir()` with a small generated tree):
- counts match exactly;
- skip-list folders are skipped;
- long-path files are found;
- a cancelled context stops the walk.

### Commands
```powershell
go run ./cmd/genfixture -out testdata/generated/fx20k -files 20000
.\bin\drivedex.exe scan testdata\generated\fx20k --count-only
.\bin\drivedex.exe scan E:\ --count-only      # ask the owner which drive letter to use
```

### Acceptance
- [ ] The fixture scan count equals the genfixture output.
- [ ] A real drive scan completes, and the summary is recorded in `PROGRESS.md` → Benchmarks.
- [ ] The owner times an Explorer search on the same drive (stopwatch) and that number is recorded too.

**Commit:** `stage 1: basic directory scanner`

---

## Stage 2 — Index + search

**Goal:**
- `drivedex index <path>` saves an index.
- `drivedex search <terms…>` searches all saved indexes in well under a second.

**Why:** this is the core idea: read the drive once, search the saved list forever.

### Files
- `internal/index/model.go` — Entry, Index, Builder, flags
- `internal/index/format.go` — Write/Read
- `internal/index/store.go` — Dir, Save, Load, LoadAll, Delete
- `internal/index/search.go`
- `internal/index/path.go`
- tests for each
- `internal/cli/index.go`
- `internal/cli/search.go`
- `internal/ui/format.go` — human sizes `1.4 MB`, dates `2024-03-07 18:22`, thousands separators

### Spec

**1. Data model and file format** exactly as section 7.

For folder sources:
- `SourceKind="folder"`;
- `SourceID = "dir-" + first 12 hex chars of sha1(lowercase(absPath))`;
- `RootPath = absPath`.

**2. `LoadAll`**
- Loads every `.ddx` in the store dir in parallel (one goroutine per file).
- Computes `Lower` in parallel chunks.
- Skips corrupt files with a warning.

**3. Search (basic syntax for now; stage 11 extends it)**
- **Query:** split on whitespace; `"double quoted phrases"` stay together.
- **Matching:** an entry matches if **every** term is a substring of its lowercase name (case-insensitive via `strings.ToLower` on terms).
- **Tombstones:** skip entries with `FlagDeleted`.
- **PERF:** split each index's slots into `runtime.NumCPU()` chunks, match in goroutines, collect matching slot numbers, then merge.
- **Ranking** (lower score is better):

  | Score | Condition |
  |---|---|
  | 0 | name exactly equals the whole query |
  | 1 | name starts with the first term |
  | 2 | first term starts at a word boundary (after `space _ - . (`) |
  | 3 | anything else |

  Tie-breaks, in order: shorter name, then files before folders, then path alphabetical.
- **Results:** return the top `--limit` (default 50) plus the total match count. Partial-sort top-K; don't fully sort millions.

**4. `drivedex index <path>`**
- Scans with `scan.Walk`, saves, and prints:
  ```
  Indexed D:\Projects — 52,114 entries in 3.1s → saved (4.2 MB)
  ```

**5. `drivedex search <terms…> [--limit N] [--source <id>] [--files|--dirs]`**
- Output: one line per result — display path (7.6), size, modified date.
- Footer:
  ```
  37 of 1,203 matches · 21 ms · searched 512,331 entries in 2 indexes
  ```
- If no index exists, print a friendly hint: "Nothing indexed yet. Try: drivedex index E:\".

**6. Tests**
- File-format round-trip: write then read gives identical entries.
- Flipping one byte gives `ErrCorruptIndex`.
- Truncated file gives an error, not a panic.
- `RelPath` with nested parents and with an orphan.
- Search covers: AND semantics, case-insensitivity with Unicode (`RÉSUMÉ` finds `résumé.pdf`), ranking order, tombstones skipped, limit and total.
- **Benchmarks** in `search_bench_test.go`, with 1,000,000 synthetic entries generated in memory:
  - `BenchmarkSearch` — target < 100 ms/op on a typical laptop;
  - `BenchmarkLoad` — target < 1.5 s for a 1M-entry file.

### Commands
```powershell
.\bin\drivedex.exe index testdata\generated\fx20k
.\bin\drivedex.exe search resume
.\bin\drivedex.exe search "foto" jpg --limit 10
go test ./internal/index/ -bench . -benchmem
```

### Acceptance
- [ ] Search on the fixture index returns correct results in < 100 ms (shown in the footer).
- [ ] The benchmarks run, and their numbers are recorded in `PROGRESS.md`.
- [ ] The owner indexes a real pen drive and searches it. The time is recorded next to the Explorer time.

**Commit:** `stage 2: index file format and instant search`

---

## Stage 3 — Drive identity, multiple drives, offline search

**Goal:**
- Drives are recognised by serial, so drive-letter changes don't matter.
- `drivedex drives`, `list` and `forget` work.
- Unplugged drives stay searchable.

**Why:** a pen drive might be E: today and G: tomorrow. Identity must come from the drive itself.

### Files
- `internal/volume/volume_windows.go`
- `internal/volume/volume_other.go`
- `internal/cli/drives.go`
- `internal/cli/list.go`
- `internal/cli/forget.go`
- updates to `index`, `search`, `cli/index.go`

### Spec

**1. Dependencies:** `go get golang.org/x/sys/windows`.

**2. At program start (Windows):**
```go
windows.SetErrorMode(windows.SEM_FAILCRITICALERRORS | windows.SEM_NOOPENFILEERRORBOX)
```
This stops "There is no disk in the drive" popups from empty card readers. Define the constants if `x/sys/windows` lacks them (`0x0001`, `0x8000`).

**3. `volume.List() ([]Volume, error)`**
- Use `windows.GetLogicalDrives()` and loop over bits 0..25 (`A:`..`Z:`).
- For each root `X:\`:

  | Field | API |
  |---|---|
  | `DriveType` | `windows.GetDriveType`. Keep `DRIVE_REMOVABLE`, `DRIVE_FIXED`, `DRIVE_REMOTE`; skip `DRIVE_CDROM` and `DRIVE_NO_ROOT_DIR`. |
  | `Label`, `Serial` (uint32), `FileSystem` | `windows.GetVolumeInformation` |
  | `TotalBytes`, `FreeBytes` | `windows.GetDiskFreeSpaceEx` |

- A drive that errors (not ready, empty reader) is listed with `Ready=false`. It is not an error.
- Struct:
  ```go
  type Volume struct {
      Root string // "E:\"
      Label, FileSystem string
      Serial uint32
      DriveType uint32
      TotalBytes, FreeBytes uint64
      Ready bool
  }
  ```
- `volume.ForPath(path)` returns the Volume that contains a path, via `filepath.VolumeName`.
- `SourceID(v) = fmt.Sprintf("vol-%08X", v.Serial)`.
- **Known limitation** (document it in README): two cloned drives share a serial, and reformatting a drive changes its serial (it then gets a new index).

**4. `drivedex index <path>`**
- If `<path>` is a drive root (`E:`, `E:\`), the source is a **volume**:
  - `SourceKind="volume"`, the serial-based ID, label, FS, total bytes;
  - `LastMount = "E:\"`;
  - `RelPath` is relative to the volume root.
- Any other path stays a **folder** source (stage 2 behaviour).

**5. Search display**
- Build `map[serial]currentRoot` from `volume.List()` once per search.
- Display paths per 7.6 (online vs offline).
- `--drive E:` filters to that volume's index (resolve the letter → serial).
- **Staleness warning:** if a mounted volume's index is older than 24 h, print one dim line after results: `tip: E:\ was indexed 3 days ago — run 'drivedex index E:' to refresh`.

**6. `drivedex drives`** — table of connected drives:

| Column | Example |
|---|---|
| Letter | `E:` |
| Label | |
| FS | |
| Type | Removable / Fixed / Network |
| Size | |
| Free | |
| Indexed? | yes (2h ago, 182k entries) / no |

**7. `drivedex list`** — all saved indexes: source ID, label, kind, FS, entries, last updated, online/offline, last mount, file size on disk.

**8. `drivedex forget <id|letter|label> [--yes]`**
- Deletes that index file after a `y/N` confirmation (skipped with `--yes`).
- Never touches the drive.

**9. Tests**
- **Portable:** display-path logic with a fake mount map (online, offline, phone, folder).
- **Windows-only** (`//go:build windows`): `volume.List()` returns at least `C:\` with NTFS and a non-zero serial.

### Commands
```powershell
.\bin\drivedex.exe drives
.\bin\drivedex.exe index E:
.\bin\drivedex.exe list
.\bin\drivedex.exe search invoice --drive E:
```

### Acceptance
- [ ] Owner: index a pen drive, unplug it, and run `drivedex search <a known file>`. The result shows as offline with the drive label.
- [ ] Owner: plug it into a different USB port (or a different PC drive letter, if it changes). The result shows the correct current letter.
- [ ] `forget` removes only the index file.

**Commit:** `stage 3: volume identity and offline search`

---

## Stage 4 — Parallel scanner + automatic HDD/flash tuning

**Goal:** much faster indexing on flash and SSDs using many goroutines, without hurting spinning HDDs.

**Why:**
- Flash drives and SSDs can serve many requests at once, so parallel reads speed them up.
- An HDD has one arm. Parallel random reads make it seek back and forth and get *slower*.
- So the tool must detect the drive type.

### Files
- `internal/scan/parallel.go`
- `internal/scan/parallel_test.go`
- `internal/volume/seek_windows.go`
- `internal/cli/benchscan.go`
- `index` CLI flags

### Spec

**1. Parallel walker**

Data types:
```go
type dirJob struct { abs, rel string; id uint64 }
```

- **Work queue:** an **unbounded** queue (slice + `sync.Mutex` + `sync.Cond`) plus a `pending` counter.
  - Do **not** use a bounded channel for the queue. Workers that block while pushing children can deadlock.
- `push(job)`: `pending++`, append, `cond.Signal()`.
- **Worker loop:**
  1. Pop a job, waiting while the queue is empty and `pending > 0`. Exit when `pending == 0`.
  2. `os.ReadDir(job.abs)`.
  3. For each entry: `id := atomic.AddUint64(&nextID, 1)`. Build the `Entry` with `ParentID: job.id`, add it to a local batch, and for subdirectories (not in the skip list, not reparse points) push `dirJob{…, id}`.
  4. Send batches of 256 entries to `out chan []index.Entry` (buffer 64).
  5. After processing: `pending--`. If it reaches 0, `cond.Broadcast()` so idle workers exit.
- A **single collector** goroutine reads `out` and calls `builder.Add`.
- A closer goroutine waits for all workers, then closes `out`.
- **Errors:** same policy as stage 1 (count, keep the first 20, continue).
- **Cancellation:** workers check `ctx` before each `ReadDir`.
- Entry order differs between runs. That's fine.

**2. Seek-penalty detection** — `volume.SeekPenalty(root string) (hasPenalty bool, known bool)`

- Open `\\.\E:` with `windows.CreateFile(path, 0 /*no access needed*/, FILE_SHARE_READ|FILE_SHARE_WRITE, nil, OPEN_EXISTING, 0, 0)`. This works without admin.
- Call `DeviceIoControl` with `IOCTL_STORAGE_QUERY_PROPERTY = 0x002D1400`.
- Input — `STORAGE_PROPERTY_QUERY` (12 bytes):
  ```
  PropertyId uint32 = 7   // StorageDeviceSeekPenaltyProperty
  QueryType  uint32 = 0   // PropertyStandardQuery
  AdditionalParameters [4]byte
  ```
- Output — `DEVICE_SEEK_PENALTY_DESCRIPTOR` (12 bytes): `Version uint32`, `Size uint32`, `IncursSeekPenalty byte` (+ padding).
- If the call fails (common with some USB enclosures), return `known=false`.

**3. Worker count (`--workers auto`)**

| Seek penalty | Workers |
|---|---|
| yes (HDD) | 2 |
| no (flash/SSD) | `min(16, 2*runtime.NumCPU())` |
| unknown | 4 |

`--workers N` overrides. Print the choice: `Using 16 workers (flash/SSD detected)`.

**4. Mode flag:** `--mode auto|walk|parallel` (stage 6 adds `mft`). `auto` = `parallel` for now.

**5. `drivedex bench-scan <drive> --workers 1,2,4,8,16`**
- Runs a count-only scan per setting and prints a table: workers, time, entries/s.
- **Warn before running:** Windows caches directory data in RAM, so the 2nd+ runs look faster than reality. For honest numbers, unplug and replug between runs, or compare only first runs.

**6. Tests**
- The parallel scanner over the fixture yields the **same set** of relative paths as `scan.Walk` (compare sorted slices).
- Run with workers = 1, 4 and 16.
- A cancelled context terminates without a goroutine leak (test finishes under a timeout).
- If a C compiler is available, also run `go test -race ./internal/scan/` (the race detector needs CGO). Otherwise note "race test skipped: no C toolchain" in `PROGRESS.md`.

### Commands
```powershell
.\bin\drivedex.exe index E: --workers auto
.\bin\drivedex.exe bench-scan E: --workers 1,4,16
```

### Acceptance
- [ ] Parallel and single scans produce identical path sets (test passes).
- [ ] Owner: index a pen drive with `--mode walk` and with `--mode parallel` (replug between runs). Both times go in the Benchmarks table.
- [ ] HDD detection was tried on at least one drive, with the result noted (detected / unknown).

**Commit:** `stage 4: parallel scanner with HDD/flash tuning`

---

## Stage 5 — Android phones via ADB (bypass MTP)

**Goal:**
- `drivedex index phone` indexes an Android phone in seconds.
- Phone files appear in search results, online or offline.

**Why:**
- MTP makes the PC ask the phone for every folder one at a time.
- With ADB, the phone itself runs `find` over its own storage (fast, local) and streams the whole list back in one response.

### Owner setup (show these steps to the owner; Claude Code can't do them)
1. On the phone: Settings → About phone → tap **Build number** 7 times (enables Developer options).
2. Settings → System → Developer options → turn on **USB debugging**.
3. **Install Platform-Tools** (ask the owner first):
   - `winget install --id Google.PlatformTools -e` — verify the ID with `winget search platform-tools`;
   - or download from https://developer.android.com/tools/releases/platform-tools and unzip.
4. Plug in the phone, unlock it, and tap **Allow** on the "Allow USB debugging?" prompt (tick "Always allow").
5. `adb devices` should show `<serial>    device`.

**iPhone is not supported** (no ADB). Document this in the README.

### Files
- `internal/phone/adb.go` — find adb, run commands
- `internal/phone/devices.go`
- `internal/phone/scanner.go`
- `internal/phone/parse.go`
- tests
- `internal/cli/index.go` updates

### Spec

**1. `phone.FindADB()`** — search in this order:
1. config path (stage 11);
2. `exec.LookPath("adb")`;
3. `%LOCALAPPDATA%\Android\Sdk\platform-tools\adb.exe`;
4. `%LOCALAPPDATA%\Microsoft\WinGet\Links\adb.exe`.

If none exist, return a friendly error with the setup steps above.

**2. `phone.Devices()`**
- Run `adb devices -l` and parse each line after the header:
  ```
  <serial>\s+<state> [key:value ...]
  ```
- States: `device`, `unauthorized`, `offline`, `no permissions`.
- Read `model:` from the key/values (replace `_` with space).
- `unauthorized` → hint: "Unlock your phone and tap Allow on the USB debugging prompt."

**3. Target selection**
- `drivedex index phone` uses the single connected device.
- If there are several, require `phone:<serial>` and list them.

**4. Phone scanner (`Name()="adb"`)**
- Run one remote command. Pass the **whole remote command as one argument** so Windows quoting doesn't mangle it:
  ```go
  exec.CommandContext(ctx, adb, "-s", serial, "shell",
      `find /sdcard/ -type f -exec stat -c '%s|%Y|%n' {} + 2>/dev/null`)
  ```
  The trailing slash on `/sdcard/` makes `find` follow the `/sdcard` symlink into the real storage.
- **Stream** stdout with `bufio.Scanner`. Raise the buffer to 1 MB max token. Never buffer the whole output.
- **Parse each line** with `strings.SplitN(line, "|", 3)` → size, mtime, path. The path is last, so names containing `|` still work.
  - Malformed lines are counted and skipped.
  - Strip the `/sdcard/` prefix to get the relative path.
- **Build folders:** `find -type f` returns only files, so create folder entries on demand. Keep `map[relDir]id` and create each missing ancestor with `FlagDir`. Empty folders are not indexed — that's acceptable; document it.
- **Fallback:** if the command exits non-zero and produced 0 lines, retry with `find /sdcard/ -type f 2>/dev/null` (names only). Set `Size=-1`, `ModTime=0`, `FlagNoMeta`.
- `Android/data` and `Android/obb` are usually blocked on Android 11+. Errors there are silenced by `2>/dev/null`. Document it.
- **Header:**
  - `SourceKind="phone"`, `SourceID="adb-"+serial`;
  - `Label=model`, `FileSystem="android"`;
  - `RootPath="/sdcard"`, `LastMount=serial`.

**5. Search integration**
- Phone indexes appear in all searches.
- Online status = the serial is in the `adb devices` output with state `device`. Only check if adb is available, and cache it for the duration of the command.

**6. Tests**
- Parser tests with a fixture text file (`testdata/adb_find_sample.txt`). Include:
  - normal lines;
  - names with spaces, `|`, Unicode and deep nesting;
  - a malformed line;
  - an empty line.
- Assert the folder synthesis.

### Commands
```powershell
adb devices -l
.\bin\drivedex.exe index phone
.\bin\drivedex.exe search IMG_2024 --limit 5
```

### Acceptance
- [ ] Owner: index their phone. Record the time and entry count.
- [ ] Owner compares with Explorer search on the phone (stopwatch, or "gave up after N minutes"), and both go in the Benchmarks table.
- [ ] Unplug the phone: results show `· offline`.

**Commit:** `stage 5: android phone indexing via adb`

---

## Stage 6 — NTFS turbo mode: MFT enumeration + USN journal

**Goal:**
- Index an NTFS drive by reading its master file list directly (millions of files in seconds).
- Refresh an index in milliseconds by reading only what changed.

**Why:**
- NTFS keeps a table of every file — the **MFT (Master File Table)** — and a diary of changes — the **USN journal**.
- Reading the table in big chunks avoids visiting folders one by one.
- Reading the diary avoids rescanning at all.
- This is how tools like *Everything* are instant.

**Requires Administrator.** Opening a volume handle with read access needs an elevated process. When not elevated, DriveDex falls back to the parallel walker and prints a tip.

**Safe testing:** enumerating `C:` is read-only and safe, and it's a great large benchmark.

### Files
- `internal/ntfs/mft_windows.go`
- `internal/ntfs/usn_windows.go`
- `internal/ntfs/records.go` — portable USN record parser
- `internal/ntfs/records_test.go`
- `internal/ntfs/apply.go` — portable delta logic
- `internal/ntfs/apply_test.go`
- `internal/ntfs/ntfs_other.go`
- `internal/cli/journal.go`
- `index` CLI mode updates

### Constants (define locally if `x/sys/windows` lacks them)
```go
const (
    FSCTL_QUERY_USN_JOURNAL  = 0x000900F4
    FSCTL_ENUM_USN_DATA      = 0x000900B3
    FSCTL_READ_USN_JOURNAL   = 0x000900BB
    FSCTL_CREATE_USN_JOURNAL = 0x000900E7

    ERROR_HANDLE_EOF                 = 38
    ERROR_JOURNAL_DELETE_IN_PROGRESS = 1178
    ERROR_JOURNAL_NOT_ACTIVE         = 1179
    ERROR_JOURNAL_ENTRY_DELETED      = 1181

    USN_REASON_FILE_DELETE = 0x00000200
    FILE_ATTRIBUTE_DIRECTORY = 0x10
    FILE_ATTRIBUTE_HIDDEN    = 0x02
    FILE_ATTRIBUTE_SYSTEM    = 0x04
)
```

### Structures (little-endian; parse records from bytes with `encoding/binary`, not unsafe casts)

**`USN_JOURNAL_DATA_V0`** (56 bytes; allocate 80 and parse the first 56):
```
UsnJournalID uint64 | FirstUsn int64 | NextUsn int64 | LowestValidUsn int64
MaxUsn int64 | MaximumSize uint64 | AllocationDelta uint64
```

**`MFT_ENUM_DATA_V0`** (24 bytes, input):
```
StartFileReferenceNumber uint64 | LowUsn int64 | HighUsn int64
```

**`READ_USN_JOURNAL_DATA_V0`** (40 bytes, input):
```
StartUsn int64 | ReasonMask uint32 | ReturnOnlyOnClose uint32 | Timeout uint64 | BytesToWaitFor uint64 | UsnJournalID uint64
```

**`CREATE_USN_JOURNAL_DATA`** (16 bytes, input):
```
MaximumSize uint64 | AllocationDelta uint64
```

**`USN_RECORD_V2`** (variable length; field offsets in bytes):

| Offset | Field |
|---|---|
| 0 | `RecordLength uint32` |
| 4 | `MajorVersion uint16` (must be 2; skip others) |
| 6 | `MinorVersion uint16` |
| 8 | `FileReferenceNumber uint64` |
| 16 | `ParentFileReferenceNumber uint64` |
| 24 | `Usn int64` |
| 32 | `TimeStamp int64` |
| 40 | `Reason uint32` |
| 44 | `SourceInfo uint32` |
| 48 | `SecurityId uint32` |
| 52 | `FileAttributes uint32` |
| 56 | `FileNameLength uint16` (bytes) |
| 58 | `FileNameOffset uint16` |

The file name is UTF-16LE at `FileNameOffset`, `FileNameLength` bytes long. Decode it with `windows.UTF16ToString` or `unicode/utf16`.

### Spec

**1. `ntfs.IsElevated()`** → `windows.GetCurrentProcessToken().IsElevated()`.

**2. Open the volume**
- `windows.CreateFile(\\.\E:, GENERIC_READ, FILE_SHARE_READ|FILE_SHARE_WRITE, nil, OPEN_EXISTING, 0, 0)`.
- "Access is denied" → `ErrNeedAdmin`.

**3. Root FRN**
- Open `E:\` with `CreateFile(…, FILE_READ_ATTRIBUTES, share all, OPEN_EXISTING, FILE_FLAG_BACKUP_SEMANTICS)`.
- Call `windows.GetFileInformationByHandle`.
- `rootFRN = uint64(FileIndexHigh)<<32 | uint64(FileIndexLow)`.

**4. Query the journal:** `FSCTL_QUERY_USN_JOURNAL`.
- On `ERROR_JOURNAL_NOT_ACTIVE`, continue without a journal: `HighUsn = math.MaxInt64` and no bookmark.

**5. Enumerate (`FSCTL_ENUM_USN_DATA`)**
- Input: `{Start: 0, LowUsn: 0, HighUsn: journal.NextUsn or MaxInt64}`.
- Output buffer: 1 MB.
- **Loop:**
  1. Call `DeviceIoControl`.
  2. On `ERROR_HANDLE_EOF` → done.
  3. Otherwise, the first 8 bytes of the output are the next start FRN.
  4. Records run from offset 8 to `bytesReturned`; step by `RecordLength`.
  5. Set `Start = next` and repeat.
- If enumeration itself fails with a journal error → return `ErrNoJournal`. The CLI then falls back to parallel and suggests `drivedex journal enable E:`.
- **Collect** into `map[uint64]rec{parent uint64; name string; attrs uint32}`.
- **Report progress** every 200 ms.

**6. Build the index from records**
- **Record number** = `frn & 0x0000FFFFFFFFFFFF`.
- **Skip** records with record number < 24 (NTFS metafiles like `$MFT`). Their children (e.g. everything under `$Extend`) become orphans and are dropped.
- **Map** `rootFRN` → parent ID 0.
- **Drop orphans:** walk each entry's parent chain with memoization (`map[frn]bool reachesRoot`) and drop any entry whose chain doesn't reach root.
- **Apply the stage 1 skip list** by name at root level (`System Volume Information`, `$RECYCLE.BIN`).
- **Entry fields:**
  - `ID = frn`;
  - `ParentID` = parent frn, or 0 if it's the root;
  - `Flags` from attributes, plus `FlagNoMeta`;
  - `Size = -1`, `ModTime = 0`.
  - USN records don't carry size or real mtime.
- **Header:** `Scanner="mft"`, `UsnJournalID`, `NextUsn` (if the journal is active).

**7. Lazy metadata**
- When displaying results that have `FlagNoMeta`, `os.Lstat` the full path of **only the displayed results** (≤ limit) to fill size and date.
- `--with-metadata` on `index` stats every file after enumeration using a parallel worker pool (slower, but complete; needed for `size:` filters in stage 11).

**8. Incremental refresh (USN deltas)** — on `drivedex index E:` when an MFT-built index exists. Conditions:
- elevated;
- `header.UsnJournalID == current.UsnJournalID`;
- `header.NextUsn >= current.LowestValidUsn`.

Then:
1. Call `FSCTL_READ_USN_JOURNAL` with `{StartUsn: header.NextUsn, ReasonMask: 0xFFFFFFFF, ReturnOnlyOnClose: 0, Timeout: 0, BytesToWaitFor: 0, UsnJournalID: id}`.
2. The first 8 bytes of the output are the next USN; records follow.
3. Loop until `bytesReturned <= 8`.
4. **Apply each record in order** (`ntfs.ApplyDelta(idx, rec, rootFRN)`):
   - `Reason & USN_REASON_FILE_DELETE != 0` → `idx.Delete(frn)`;
   - otherwise → `idx.Upsert(Entry{ID: frn, ParentID: parent (root→0), Name, Flags, Size:-1, FlagNoMeta})`.

   Upsert is idempotent and later records win. This one rule correctly handles create, rename and move. A moved folder updates one entry, and all children's paths follow automatically because paths are computed from parents (explain this to the owner — it's the elegant part).
5. Skip records with record number < 24.
6. **Fall back to a full rebuild if:**
   - `ERROR_JOURNAL_ENTRY_DELETED` (bookmark too old), or
   - the journal ID changed.
7. Save with the new `NextUsn`.
8. Print: `Refreshed E:\ from change journal — 132 changes applied in 18 ms`.

**9. `drivedex journal status E:`** prints active/inactive, ID, size, first/next USN.

**`drivedex journal enable E:`**
- Ask "This adds a small change log (32 MB) to the drive's file system. Continue? y/N".
- Then `FSCTL_CREATE_USN_JOURNAL{MaximumSize: 32<<20, AllocationDelta: 4<<20}`.
- Requires admin.

**10. Mode selection (`--mode auto`)**

| Drive | Elevated | Mode |
|---|---|---|
| NTFS | yes | `mft` |
| NTFS | no | `parallel`, plus one tip: "Run in an Administrator terminal for NTFS turbo mode (often 10× faster)." |
| FAT32 / exFAT / other | — | `parallel` |
| phone | — | `adb` |

**11. Tests (portable)**
- `ParseRecords`: build byte slices for 3 crafted V2 records (including a Unicode name), parse them, and assert the fields. A truncated buffer gives an error, not a panic.
- `ApplyDelta`, applied to an index:
  1. create file;
  2. rename it;
  3. move its parent folder into another folder;
  4. delete the file.

  Assert `RelPath` and existence after each step.
- Orphan and metafile filtering.

### Commands (owner runs these in an **Administrator** PowerShell)
```powershell
.\bin\drivedex.exe journal status C:
.\bin\drivedex.exe index C: --mode mft          # read-only, safe; big benchmark
.\bin\drivedex.exe index E: --mode mft
# create/rename a few files on E:, then:
.\bin\drivedex.exe index E:                     # should say "refreshed from change journal"
```

### Acceptance
- [ ] The `C:` MFT index completes. Record entries + time; target: under ~15 s per million files.
- [ ] On an NTFS external drive, MFT entry count vs parallel-walk entry count are within ±2%. Explain the differences: hidden/system items, protected folders the walker can't read.
- [ ] 20 random entries from the MFT index exist on disk (`os.Lstat` succeeds).
- [ ] The incremental refresh reflects a created, a renamed and a deleted file.
- [ ] A non-elevated run falls back to parallel with the tip.

**Commit:** `stage 6: NTFS MFT enumeration and USN journal refresh`

---

## Stage 7 — Fast, resumable copy (drives and phones)

**Goal:** `drivedex copy <src> <dst>` copies faster than Explorer for many-file folders, resumes after interruption, and works for phone → PC.

**Why:**
- Explorer copies one file at a time and pays the per-file overhead serially.
- Parallel workers hide that latency on flash and SSD.
- Resume means a failed 40 GB copy doesn't restart from zero.

### Files
- `internal/copier/plan.go`
- `internal/copier/copy.go`
- `internal/copier/phone.go`
- `internal/ui/progress.go`
- tests
- `internal/cli/copy.go`

### Spec

**1. Syntax**
```
drivedex copy <src> <dst> [--workers auto|N] [--on-conflict newer|skip|overwrite] [--verify] [--dry-run]
```
- `src` can be a file, a folder, or a phone path: `phone:/DCIM/Camera` or `phone:<serial>:/DCIM/Camera`.
- `dst` is a local folder. Create it if missing.
- A folder `src` is copied *into* `dst` as `dst\<srcName>\…`, like Explorer does.

**2. Plan phase**
- Walk `src` with the stage 4 parallel walker to get a list of jobs: rel path, size, mtime.
- Create every destination directory first.
- **Pre-checks**, each aborting with a clear message:
  - Total bytes > free space on `dst`.
  - `dst` is FAT32 and any file is ≥ 4 GiB (FAT32 can't store it). Suggest exFAT/NTFS.
- Print the plan: `Copying 12,408 files (18.3 GB) with 16 workers…`
- `--dry-run` stops here and lists what would be copied, skipped or overwritten.

**3. Conflict policy** (default `newer`) for each job whose destination exists:

| Situation | Action |
|---|---|
| Same size, and mtime within 2 s | **skip** (this is resume). The 2-second tolerance exists because FAT stores times in 2-second steps. |
| Different, `newer` | overwrite only if the source mtime is newer; else skip and report |
| Different, `skip` | always skip |
| Different, `overwrite` | always overwrite |

**4. Workers**
- Sort jobs **largest first**, so big files start early and don't end as a lone straggler.
- Auto worker count:

  | Condition | Workers |
  |---|---|
  | `src` or `dst` volume has a seek penalty | 2 |
  | both are flash/SSD | 8 |
  | unknown | 4 |

- **Per-file steps:**
  1. Open `src`.
  2. Create `dst + ".ddxpart"`.
  3. `io.CopyBuffer` with a 1 MB buffer from a `sync.Pool`.
  4. Close.
  5. `os.Chtimes` to the source mtime.
  6. `os.Rename(part, final)`.

  The real name never points at a half-written file.
- Do **not** fsync per file (it destroys USB throughput). Tell the owner to always use "Safely Remove".
- Retry each failed file 2 times with a 500 ms backoff, then record the failure and continue.

**5. Progress line** — updates every 200 ms on one line with `\r`:
```
[ 8,412/12,408 files ]  12.1/18.3 GB  ·  86 MB/s  ·  ETA 1m12s  ·  3 skipped  ·  0 failed
```
End with a summary. The exit code is non-zero if any file failed. Then list the failures (first 20 + a count).

**6. Ctrl+C**
- Stop dispatching new jobs and let in-flight files finish (or cancel them; leftover `.ddxpart` files get overwritten next run).
- Print `Stopped. Run the same command again to resume.`

**7. `--verify`**
- After copying, compute CRC-32 (Castagnoli, `crc32.MakeTable(crc32.Castagnoli)`, hardware-accelerated) of the source and destination for each copied file. Report mismatches.
- Note in the help text: the destination may be read back from RAM cache, so this verifies the copy logic, not the physical media.

**8. Phone source (`copier/phone.go`)**
1. List the remote files with the stage 5 `find … stat` command, scoped to the given path.
2. Apply the conflict policy against local files (size + 2 s mtime rule).
3. Group pending files by **remote parent folder**.
4. For each group, run `adb -s S pull -a <file1> <file2> … <localDir>`. `-a` preserves timestamps. Batch at most 100 files per call to stay under the Windows command-line length limit.
5. Run up to 2 pull processes in parallel. ADB is the bottleneck; more doesn't help.
6. Progress comes from file completion between batches. Show `files done/total`. Byte counts are approximate.

**9. Tests**
- Copy the fixture to `t.TempDir()`: all files are present, and sizes and mtimes match.
- Run the copy again: everything is skipped.
- Cancel midway, then rerun: the copy completes, and no `.ddxpart` files remain.
- The FAT32 4 GiB pre-check logic, using a fake FS name.
- The free-space check, using a fake value.

### Commands
```powershell
.\bin\drivedex.exe copy E:\Photos D:\Backup --dry-run
.\bin\drivedex.exe copy E:\Photos D:\Backup
.\bin\drivedex.exe copy phone:/DCIM/Camera D:\PhoneBackup
```

### Acceptance
- [ ] The tests pass.
- [ ] Owner benchmark: copy one folder of ~5,000+ small files three ways — Explorer, `robocopy src dst /E /MT:16`, and `drivedex copy`. Record all three times.
- [ ] A phone folder copy works and resumes after unplug/replug.

**Commit:** `stage 7: parallel resumable copy`

---

## Stage 8 — Interactive search UI (type-as-you-search)

**Goal:** running `drivedex` with no arguments opens a full-screen search. Type and results update live. Enter opens the file.

**Why:** this is the most convenient way to use the tool day to day.

### Files
- `internal/tui/model.go`
- `internal/tui/view.go`
- `internal/tui/actions_windows.go`
- `internal/cli/tui.go`
- root command default action

### Spec

**1. Dependencies:** `go get github.com/charmbracelet/bubbletea github.com/charmbracelet/bubbles github.com/charmbracelet/lipgloss github.com/atotto/clipboard`

**2. Entry points**
- `drivedex` (no args) and `drivedex tui`.
- Flags: `[--drive E:] [--scope <folder>] [--query "<initial>"]`.

**3. Startup**
- Load all indexes in a background `tea.Cmd`, showing a spinner: `Loading 3 indexes…`.
- Keep them in memory for the session.

**4. Layout**
```
 🔍 resume pdf_                                              
 ────────────────────────────────────────────────────────────
 ▶ Resume_Yash_2026.pdf     E:\Docs\Jobs\                  212 KB  2026-08-14
   resume_old.pdf           [SANDISK 32GB · offline] \Old\  180 KB  2024-02-01
   Resume.pdf               phone:Pixel 7/Download/         198 KB  2026-07-30
 ────────────────────────────────────────────────────────────
 1,203 matches · 18 ms · 3 sources (1 offline)
 ↑↓ move · Enter open · Ctrl+O show in folder · Ctrl+Y copy path · Ctrl+P get from phone · Esc quit
```
- Name in bold, folder dimmed.
- Matched substring highlighted with lipgloss.
- Offline rows dimmed.

**5. Search loop**
- On each keystroke, start a 60 ms debounce timer. When it fires, run the search in a `tea.Cmd` tagged with a sequence number. Ignore results whose sequence number is older than the latest.
- Show up to the terminal height; Page Up and Page Down scroll.

**6. Actions** (Windows, `actions_windows.go`)

| Key | Action |
|---|---|
| **Enter** | Open the file with its default app: `windows.ShellExecute(0, "open", path, "", dir, SW_SHOWNORMAL)`. On a folder, open it in Explorer. On an offline row, show a status message: `Drive "SANDISK 32GB" isn't connected — plug it in.` |
| **Ctrl+O** | Show in folder. Run Explorer with an **exact** command line, because Go's default argument quoting breaks `/select` (see the snippet below the table). Ignore its exit code: explorer.exe returns 1 even on success. |
| **Ctrl+Y** | Copy the full path to the clipboard and show `Copied!` in the status bar. |
| **Ctrl+P** | Phone row: `adb pull -a` into `%USERPROFILE%\Downloads\DriveDex\`, then show it in the folder. |

Ctrl+O snippet — set the raw command line yourself:
```go
cmd := exec.Command("explorer.exe")
cmd.SysProcAttr = &syscall.SysProcAttr{
    CmdLine: `explorer.exe /select,"` + path + `"`,
}
_ = cmd.Run() // explorer.exe exits with 1 even on success
```

**7. `--scope <folder>`** restricts results to paths under that folder, on the volume that contains it.

### Acceptance
- [ ] Owner: open the TUI, type, see results update with no visible lag on all indexes.
- [ ] Enter opens a PDF or image.
- [ ] Ctrl+O selects the file in Explorer (path with spaces tested).
- [ ] Ctrl+Y works.
- [ ] Ctrl+P on a phone result works.

**Commit:** `stage 8: interactive search UI`

---

## Stage 9 — Auto-index agent (drives index themselves when plugged in)

**Goal:** a background program with no console window that notices new drives and phones and refreshes their indexes automatically.

**Why:** the owner should never have to remember to run `index`.

### Files
- `cmd/drivedex-agent/main.go`
- `internal/agent/watch.go`
- `internal/agent/lock.go`
- `internal/volume/bus_windows.go`
- `internal/cli/agent.go`
- optional: `internal/agent/pipe_windows.go`

### Spec

**1. Build** the agent as a GUI-subsystem exe so no console window appears:
```powershell
go build -trimpath -ldflags "-s -w -H=windowsgui -X main.version=$v" -o bin/drivedex-agent.exe ./cmd/drivedex-agent
```
Add this to `build.ps1`.

**2. Single instance**
- `windows.CreateMutex(nil, false, "Local\\DriveDexAgent")`.
- If the error is `ERROR_ALREADY_EXISTS`, exit silently.
- Write the PID to `%LOCALAPPDATA%\DriveDex\agent.pid`.

**3. Logging**
- `%LOCALAPPDATA%\DriveDex\agent.log`.
- Rotate to `agent.log.1` at 5 MB.
- One line per event.

**4. Bus type** — `volume.BusType(root)`. Only **USB** drives are auto-indexed by default; internal drives are indexed only if listed in config `auto_index`.
- Same handle as the seek-penalty query, with `PropertyId = 0` (`StorageDeviceProperty`).
- Output: `STORAGE_DEVICE_DESCRIPTOR`, using a 1 KB buffer:

  | Offset | Field |
  |---|---|
  | 0 | Version |
  | 4 | Size |
  | 8 | DeviceType (byte) |
  | 9 | DeviceTypeModifier |
  | 10 | RemovableMedia |
  | 11 | CommandQueueing |
  | 12 | VendorIdOffset (u32) |
  | 16 | ProductIdOffset (u32) |
  | 20 | ProductRevisionOffset |
  | 24 | SerialNumberOffset |
  | 28 | **BusType (u32)** |

- `BusTypeUsb = 7`.
- Also read the ProductId string (NUL-terminated ASCII at that offset, if non-zero) for nicer labels like "SanDisk Ultra".

**5. Watch loop**
- Every 2 s: compare the `GetLogicalDrives()` bitmask with the previous one.
- Every 5 s, if adb exists: compare `adb devices` serials.
- **On a newly arrived USB volume or phone:**
  1. Wait 3 s for it to settle.
  2. Pick the refresh method:
     - MFT index + elevated + journal valid → USN delta;
     - otherwise → full rescan with the auto scanner.
  3. Save.
- **Notification** (if `notifications=true` in config): a Windows toast, e.g. `"SANDISK 32GB indexed — 182,334 files"`. Simplest way: a hidden `powershell.exe -NoProfile -WindowStyle Hidden` running a small toast script via `Windows.UI.Notifications`. Wrap it in a function; if it fails, just log.
- **Concurrency guard:** acquire `<id>.lock` via `os.OpenFile(O_CREATE|O_EXCL)` before re-indexing, and remove it after. Treat a lock older than 10 min as stale. The CLI `index` command uses the same lock.

**6. Startup without admin**
- Registry value `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` → `DriveDexAgent = "\"<path>\drivedex-agent.exe\""` (via `golang.org/x/sys/windows/registry`).
- **Ask the owner before writing.**

**Optional elevated startup** (enables MFT turbo in the background):
```
schtasks /Create /TN "DriveDex Agent" /TR "\"<path>\drivedex-agent.exe\"" /SC ONLOGON /RL HIGHEST /F
```
Creating this task requires an Administrator terminal once. **Ask first**, and explain the trade-off: the agent then runs with admin rights.

**7. CLI commands**

| Command | Behaviour |
|---|---|
| `drivedex agent start` | Launch the agent detached. |
| `drivedex agent stop` | Read the PID and kill the process. |
| `drivedex agent status` | Running? PID, uptime, last 10 log lines. |
| `drivedex startup enable [--elevated]` / `disable` | Manage the startup entry. |

**8. Optional stretch — do only after everything above passes**
- The agent keeps all indexes in memory and serves searches over the named pipe `\\.\pipe\drivedex`, using `github.com/Microsoft/go-winio` with JSON request/response.
- CLI `search` and the TUI try the pipe first (50 ms connect timeout) and fall back to loading files.
- Result: zero load time.

**9. Optional:** tray icon with "Search…", "Re-index all" and "Quit" — only if `fyne.io/systray` builds with `CGO_ENABLED=0` on Windows. Otherwise skip it and note that in `PROGRESS.md`.

### Acceptance
- [ ] Owner: start the agent and plug in a pen drive. Within ~10 s, the log shows the indexing, and `drivedex list` shows an updated time.
- [ ] Plug in the phone (authorized): same result.
- [ ] Starting a second agent exits immediately.
- [ ] Startup enable/disable creates and removes the Run value (verify with `reg query HKCU\Software\Microsoft\Windows\CurrentVersion\Run`).
- [ ] No console window ever appears for the agent.

**Commit:** `stage 9: background auto-index agent`

---

## Stage 10 — Explorer integration (right-click menu)

**Goal:** right-click a drive, a folder, or empty space in a folder → **"Search with DriveDex"** opens the TUI scoped there. Right-click a drive → **"Index with DriveDex"**.

**Why:** the owner gets the speed without opening a terminal first.

### Files
- `internal/shellint/menu_windows.go`
- `internal/cli/shell.go`

### Spec

**1. All keys go under `HKCU\Software\Classes`**, so no admin is needed. Use the absolute path of the running exe (`os.Executable()`).

| Key | (Default) | Extra values | `\command` (Default) |
|---|---|---|---|
| `Directory\shell\DriveDex` | `Search with DriveDex` | `Icon` = `"<exe>",0` | `"<exe>" tui --scope "%1"` |
| `Directory\Background\shell\DriveDex` | `Search with DriveDex` | `Icon` | `"<exe>" tui --scope "%V"` |
| `Drive\shell\DriveDex` | `Search with DriveDex` | `Icon` | `"<exe>" tui --scope "%1"` |
| `Drive\shell\DriveDexIndex` | `Index with DriveDex` | `Icon` | `"<exe>" index "%1" --pause` |

**2. `--pause`** (a global flag): after the command finishes, print `Press Enter to close…` and wait, so the window doesn't vanish.

**3. Scope not indexed:** if the TUI `--scope` path's volume has no index, show a prompt inside the TUI: `This drive isn't indexed yet. Press I to index now (takes a moment), Esc to cancel.`

**4. Commands**
- `drivedex shell install` — **ask the owner first**, then write the keys.
- `drivedex shell uninstall` — delete exactly these keys, recursively.
- `drivedex shell status`.

**5. Windows 11 note** (document in README): these entries appear under **"Show more options"** (or Shift+Right-click). The new compact menu requires a packaged app, which is out of scope.

### Acceptance
- [ ] Owner: right-click a pen drive → "Search with DriveDex" → the TUI opens scoped to it.
- [ ] Right-click inside a folder's empty space → it works.
- [ ] "Index with DriveDex" shows progress and waits for Enter.
- [ ] `shell uninstall` removes every entry.

**Commit:** `stage 10: Explorer right-click integration`

---

## Stage 11 — Doctor, config, power search, polish

**Goal:**
- A `doctor` that explains *why* things are slow.
- A config file.
- A richer query syntax.
- Script-friendly output.
- Polished help.

### Files
- `internal/doctor/*.go`
- `internal/config/config.go`
- `internal/query/parser.go`
- `internal/query/parser_test.go`
- `internal/query/match.go`
- `search` and `tui` updates
- `internal/cli/doctor.go`
- `internal/cli/config.go`

### Spec

**1. Config:** `%APPDATA%\DriveDex\config.toml` (`os.UserConfigDir()`). Created with defaults on first run; `drivedex config path|show|edit` (`edit` opens it in Notepad).
```toml
# DriveDex configuration
default_limit   = 50
show_hidden     = true
workers         = "auto"          # or a number
skip_dirs       = ["System Volume Information", "$RECYCLE.BIN", "$Recycle.Bin"]
skip_dirs_extra = []              # e.g. ["node_modules", ".git"]
stale_after     = "24h"
notifications   = true
auto_index      = []              # extra non-USB drives, e.g. ["D:"]
adb_path        = ""              # leave empty to auto-detect
phone_root      = "/sdcard/"
color           = "auto"          # auto | always | never (NO_COLOR env var is respected)
```

**2. Query syntax** — the `query` package parses into a struct; table-driven tests cover **every** example below.

| Syntax | Meaning | Example |
|---|---|---|
| `word` | name contains (all terms must match) | `resume pdf` |
| `"two words"` | phrase | `"final report"` |
| `!word` | exclude | `invoice !draft` |
| `ext:a,b` | extension is one of | `ext:jpg,png` |
| `type:file` / `type:dir` | entry type | `type:dir photos` |
| `size:>100mb`, `size:<1kb`, `size:10mb..1gb` | size filter (units b/kb/mb/gb, 1024-based) | `size:>1gb ext:mp4` |
| `modified:>2025-01-01`, `modified:<30d` | date filter (absolute date or relative days) | `modified:<7d` |
| `drive:E` / `drive:phone` / `drive:"SANDISK 32GB"` | source filter (letter, phone, or label) | `drive:phone IMG` |
| `path:word` | match against the full relative path instead of the name | `path:whatsapp` |
| `re:<regex>` | Go regexp, case-insensitive, on the name | `re:^IMG_\d{8}` |

- `size:` and `modified:` on entries with `FlagNoMeta` → exclude them, and print one tip: `some NTFS turbo-mode entries have no size/date; run 'drivedex index E: --with-metadata' to include them.`
- `path:` builds paths only for candidates that already passed the cheap filters. **PERF:** apply cheap filters first (type, ext, name terms), then path/regex/size/date.

**3. Output flags** on `search`:
- `--json` → an array of `{path, name, size, modified, source_id, label, online}`.
- `--paths-only` → one path per line, for piping.
- `--copy-to <dir>` → copies every match (not just the displayed ones) using the stage 7 copier. Confirm if more than 100 files.

**4. `drivedex doctor [--bench E:] [--yes]`** — prints ✅ / ⚠️ / ❌ lines, each with a one-line fix tip.

| Area | Checks |
|---|---|
| DriveDex | version; index folder writable; config valid; agent running?; startup enabled?; right-click menu installed? |
| Privileges | Elevated? (explain MFT turbo mode) |
| Phone | adb found? (version); devices and their states (`unauthorized` → tip) |
| Each ready drive | file system; bus type (USB/SATA/NVMe…); HDD/flash/unknown; index present + age + entries; NTFS: journal active? (if elevated) |

**`--bench E:`** (asks y/N unless `--yes`):
1. Requires 600 MB free.
2. Writes a 256 MB file `E:\.drivedex-bench.tmp` in 4 MB chunks, calls `f.Sync()`, and measures MB/s.
3. **Always deletes the file**, even on error (`defer`).
4. Interprets the result:

   | Speed | Message |
   |---|---|
   | < 25 MB/s | ❌ "very slow — likely a USB 2.0 port/cable or a low-end drive" |
   | 25–60 MB/s | ⚠️ "USB 2.0-class speed — try a blue/'SS' USB 3 port and a different cable" |
   | > 60 MB/s | ✅ |

Final **"Tips"** section, printed as information only — doctor changes nothing:
- **Write caching:** Device Manager → Disk drives → your drive → Policies → "Better performance" makes copies faster, but you must then always use "Safely Remove".
- **Windows Defender** scans every copied file. This is normal. Don't disable it.
- **Many tiny files** copy much faster if zipped first.

**5. Help polish**
- Every cobra command gets `Short`, `Long` (plain English, 2–4 lines) and `Example` (2–3 real examples).
- Add `drivedex completion powershell`, and document in README how to enable it.

### Acceptance
- [ ] The parser tests cover every example in the table.
- [ ] `doctor` runs on the owner's PC with sensible output.
- [ ] `doctor --bench` on a pen drive leaves no file behind.
- [ ] `search ext:jpg size:>2mb --json` produces valid JSON (`ConvertFrom-Json` parses it).

**Commit:** `stage 11: doctor, config, power search syntax`

---

## Stage 12 — Packaging, install, release, docs

**Goal:** one-command install on any Windows PC, a GitHub release, and a README good enough for a resume link.

### Files
- `scripts/build.ps1` (final)
- `scripts/install.ps1`
- `internal/cli/install.go`
- `.github/workflows/ci.yml`
- `README.md`
- `docs/HOW-IT-WORKS.md`
- `docs/BENCHMARKS.md`
- optional `winres/`

### Spec

**1. `build.ps1` (final)**
- Runs vet and tests.
- Builds `drivedex.exe` and `drivedex-agent.exe` (agent with `-H=windowsgui`) for `windows/amd64` and `windows/arm64`.
- Version from `git describe`.
- Writes `bin/drivedex-<version>-windows-<arch>.zip` containing both exes + README + LICENSE.

**2. Optional icon & manifest** via `go-winres`:
- `go install github.com/tc-hib/go-winres@latest`;
- `go-winres init` creates `winres/`. Set the icon, the product name, and in the manifest `longPathAware: true`.
- `go-winres make` generates `.syso` files that `go build` picks up automatically.
- **Ask the owner** before installing the tool.

**3. `drivedex install`** — asks y/N before **each** system change:
1. Copy both exes to `%LOCALAPPDATA%\Programs\DriveDex\`.
2. Add that folder to the **user** PATH if absent:
   ```powershell
   powershell -NoProfile -Command "[Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path','User') + ';<dir>', 'User')"
   ```
   This also notifies Windows of the change. Tell the owner to open a new terminal.
3. Offer: right-click menu (stage 10).
4. Offer: start the agent at login (stage 9).
5. Run `doctor`.

`scripts/install.ps1` does the same for someone who just downloaded the zip.

**4. `drivedex uninstall`** reverses everything:
- menu keys, Run value / scheduled task, PATH entry, program folder;
- then asks whether to also delete the indexes and config.

**5. CI (`.github/workflows/ci.yml`)**
- On push/PR, on `windows-latest`: `actions/setup-go` (go-version: stable), `go vet ./...`, `go test ./...`, `scripts/build.ps1`.
- On tags `v*`: also build the zips and attach them to a GitHub Release (`softprops/action-gh-release`).
- **Ask the owner** before creating the GitHub repo or pushing.

**6. `README.md`** — written for a recruiter *and* a user:
- one-line pitch;
- a demo GIF placeholder (tell the owner how to record one with ScreenToGif);
- the problem in 3 bullets;
- benchmark table (from `docs/BENCHMARKS.md`);
- install;
- quick start (5 commands);
- query syntax table;
- how it works (5 bullets, linking to `HOW-IT-WORKS.md`);
- limitations: iPhone unsupported, admin needed for turbo mode, cloned-drive serials, empty phone folders not indexed, Windows 11 "Show more options";
- license.

**7. `docs/HOW-IT-WORKS.md`** — beginner-friendly explanations:
- why Explorer is slow;
- what an index is;
- file systems (NTFS vs FAT/exFAT);
- MFT and USN journal (the library catalogue and diary analogy);
- HDD vs flash and why worker count matters;
- MTP vs ADB;
- parent-ID path storage, and why renames are cheap.

The owner should be able to explain the project in an interview after reading this.

**8. `docs/BENCHMARKS.md`** — all measured numbers from `PROGRESS.md`, with hardware notes (laptop model, USB port type, drive model).

### Acceptance
- [ ] A fresh clone builds with `scripts/build.ps1`.
- [ ] `drivedex install` on the owner's PC works end to end (new terminal → `drivedex` opens the TUI).
- [ ] `drivedex uninstall` leaves no traces except optionally-kept indexes.
- [ ] CI passes on GitHub (if the owner agreed to push).
- [ ] README renders correctly on GitHub.

**Commit:** `stage 12: packaging, install and release` → then tag `v1.0.0` (ask first).

---

# Reference

## Testing strategy summary

| Layer | How |
|---|---|
| Pure logic (format, search, query, parsers, delta apply, copier) | Unit tests with table-driven cases; run anywhere |
| Windows APIs (volume, seek, bus, MFT) | `//go:build windows` tests that use `C:` read-only |
| Hardware (pen drive, HDD, phone, admin) | Manual acceptance steps run by the owner, with evidence pasted into `PROGRESS.md` |
| Performance | `go test -bench` + `bench-scan` + manual stopwatch vs Explorer, recorded in Benchmarks |

## Performance targets

| Operation | Target |
|---|---|
| Search 1M entries (in memory) | < 100 ms |
| Load 1M-entry index from disk | < 1.5 s |
| MFT enumeration | < ~15 s per million files |
| USN delta refresh | < 1 s for typical changes |
| Phone index (50k files) | under ~1 minute (vs. Explorer: often unusable) |
| Copy of many small files | faster than Explorer; comparable to `robocopy /MT` |

## Troubleshooting (put in README too)

| Problem | Fix |
|---|---|
| `adb` not recognized | Install Platform-Tools (stage 5) and open a new terminal, or set `adb_path` in config. |
| Phone shows `unauthorized` | Unlock the phone, tap Allow on the USB debugging prompt, run `adb kill-server`, replug. |
| "Access is denied" opening `\\.\E:` | NTFS turbo mode needs an Administrator terminal. Without it, DriveDex falls back automatically. |
| `ERROR_JOURNAL_NOT_ACTIVE` | `drivedex journal enable E:` (admin), or just use parallel mode. |
| Results show an old drive letter | Run `drivedex drives`; letters are resolved by serial at search time. If the drive was reformatted, it's a new index. |
| SmartScreen warns about the exe | Unsigned binaries trigger this. Click "More info → Run anyway", or build it yourself. |
| Copy fails on files over 4 GB | The destination is FAT32. Reformat to exFAT/NTFS (back up first). |
| Indexing is slow on a USB HDD | Expected with seek penalties. Use NTFS turbo mode (admin), and keep workers at 2. |
| Explorer `/select` opens the wrong folder | Path quoting. See the stage 8 exact command-line note. |

## Future ideas (after v1.0)

- **Duplicate finder:** group by size, then hash, across all drives ("this photo exists on 3 drives").
- **Disk-usage view:** largest folders per drive, from the index (like WizTree).
- **Content search:** `content:"text"` using parallel reads on mounted drives.
- **iPhone support** via libimobiledevice/AFC.
- **Desktop GUI** (Wails) on top of the same `internal/` packages.
- **Search history and pinned searches** in the TUI.

---

## Appendix A — `PROGRESS.md` template

```markdown
# DriveDex — Progress

## Stages
- [ ] 0 Project setup
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
Stage: 0 — not started

## Decisions
- (date) module path = ...

## Deviations from BUILD.md
- (none yet)

## Blockers / needs owner
- (none)

## Benchmarks
| Date | Test | Drive (model, FS, port) | Files | Explorer | DriveDex | Mode/Workers | Notes |
|------|------|-------------------------|-------|----------|----------|--------------|-------|
```

## Appendix B — Glossary (for the owner)

| Term | Meaning |
|---|---|
| **Index** | A saved list of every file on a drive, so search doesn't have to touch the drive. |
| **File system** | The rules a drive uses to organize files. **NTFS** = Windows' main one, with a central file table. **FAT32/exFAT** = simpler, used on pen drives, no central table. |
| **MFT** | NTFS's Master File Table: one big table of every file. Reading it is like reading the library catalogue instead of walking the shelves. |
| **USN journal** | NTFS's change diary. DriveDex reads only new diary entries to stay up to date. |
| **Volume serial** | A unique ID written when a drive is formatted. It's how DriveDex recognizes a drive regardless of letter. |
| **Seek penalty** | The delay a spinning hard disk pays to move its arm. Flash/SSD have none. |
| **Goroutine / worker pool** | Go's lightweight threads; a fixed group of them sharing a queue of work. |
| **MTP** | The slow request-by-request protocol Windows uses to talk to phones. |
| **ADB** | Android Debug Bridge: a developer tool for fast, direct commands on an Android phone. |
| **IOCTL / FSCTL** | Low-level "ask the driver directly" calls in Windows (`DeviceIoControl`). |
| **TUI** | Text User Interface: an interactive full-screen app inside the terminal. |
| **Atomic write** | Write to a temp file, then rename. Readers see either the old or the new file, never a broken one. |
| **CGO** | Go calling C code. Avoided here, so the exe needs no extra DLLs or compilers. |

*End of BUILD.md*
