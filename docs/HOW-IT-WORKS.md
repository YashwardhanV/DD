# How DriveDex works

A beginner-friendly tour. You don't need to know any of this to *use* DriveDex — but if you read it once, you'll be able to explain the project (and the ideas behind it) to anyone, including in an interview. Each section ends with a one-sentence "in short".

Contents: [1. Why Explorer is slow](#1-why-explorer-is-slow) · [2. What an index is](#2-what-an-index-is) · [3. File systems](#3-file-systems-ntfs-vs-fatexfat) · [4. The MFT and the USN journal](#4-the-mft-and-the-usn-journal-the-library-catalogue-and-the-diary) · [5. HDD vs flash](#5-hdd-vs-flash-and-why-the-number-of-workers-matters) · [6. MTP vs ADB](#6-phones-mtp-vs-adb) · [7. Paths from parent IDs](#7-how-the-index-stores-paths-parent-ids-and-why-renames-are-cheap) · [8. Search internals](#8-how-search-is-so-fast) · [9. Knowing a drive](#9-recognising-a-drive-serial-numbers-not-letters) · [10. Copy, agent, safety](#10-copying-the-agent-and-staying-safe) · [11. USB 2 vs USB 3](#11-usb-20-vs-usb-3-why-software-cant-speed-up-a-port) · [12. Interview cheat sheet](#12-interview-cheat-sheet)

---

## 1. Why Explorer is slow

When you type in Explorer's search box on an external drive, Windows has to **look at the drive**: it opens every folder, asks for the list of what is inside, then opens each sub-folder, and so on. Each of those is a small request to the drive, one after another.

- A **spinning hard disk** has to physically move its read head for each folder. That's several milliseconds per request, and a drive with 500,000 folders needs 500,000 of them.
- A **pen drive** has no moving parts, but cheap ones answer small requests slowly, and USB itself adds delay to every request.
- If the drive **isn't plugged in**, there is nothing to look at, so there's nothing to find.

Windows does have a search index for your internal drives, but it doesn't cover drives you plug in and out.

**In short:** Explorer re-reads the whole drive for every search, and a drive is slow at being asked many small questions.

## 2. What an index is

An **index** is a saved list of every file and folder on a drive: its name, size, modification date and where it sits in the folder tree. Think of the card catalogue of a library: finding a book in the catalogue takes seconds; walking the shelves takes hours.

DriveDex does the slow walk **once** (`drivedex index E:`) and saves the list on your PC as one small file per drive (a `.ddx` file in `%LOCALAPPDATA%\DriveDex\indexes`). From then on `drivedex search` only looks at those files in your computer's memory — it never touches the drive. That is why you can search a drive that is in a drawer.

The catch: an index is a *snapshot*. If you add files to the drive on another PC, the index doesn't know until you refresh it (`drivedex index E:` again — fast on NTFS, see section 4 — or let the background agent do it when you plug the drive in).

**In short:** walk the drive once, remember the result, search the memory.

## 3. File systems: NTFS vs FAT/exFAT

A **file system** is the set of rules a drive uses to organise files — the "filing system" inside the drive.

| | NTFS | exFAT / FAT32 |
|---|---|---|
| Typical use | Windows' own disks, many external hard disks | Pen drives, SD cards, cameras, phones |
| Where it keeps the file list | In one big central table (the *MFT*, next section) | Spread across the folders themselves |
| Change diary | Yes — the *USN journal* (next section) | No |
| Biggest single file | Practically unlimited | FAT32: **4 GiB** (exFAT: practically unlimited) |

Two consequences for DriveDex:

- **Only NTFS drives can use "turbo mode"** (reading the central table directly). Pen drives are usually exFAT/FAT32, so they use the normal scanner — which is fine, because they have no moving parts.
- **Copying a movie bigger than 4 GiB to a FAT32 pen drive fails** in a confusing way in Explorer. `drivedex copy` checks first and tells you ("the destination is FAT32… use exFAT or NTFS").

**In short:** NTFS keeps a central file table and a change diary; FAT/exFAT don't.

## 4. The MFT and the USN journal: the library catalogue and the diary

### The MFT — the library catalogue

On an NTFS drive, the **Master File Table (MFT)** is one big table with one row ("record") for every file and folder: its name, its parent folder and its attributes. Windows keeps it for its own use, but it is exactly the list DriveDex wants.

Walking the folders is like walking the shelves of the library, shelf by shelf. Reading the MFT is like reading the catalogue from the first card to the last: one long, straight read instead of hundreds of thousands of small jumps. DriveDex asks Windows for it with a single low-level call (`FSCTL_ENUM_USN_DATA`, sent through `DeviceIoControl`) and receives the records in big batches. That's the difference between **minutes** and **seconds** for millions of files.

Windows only allows administrators to open a drive for raw reading, which is why "turbo mode" needs an **Administrator terminal**. Without one, DriveDex quietly falls back to the normal scanner and says so.

The catalogue doesn't hold sizes and dates in the form DriveDex reads them, so turbo mode normally indexes **names only**; `--with-metadata` makes it also read every size and date (slower). DriveDex fills in sizes and dates for the few results you actually look at.

### The USN journal — the librarian's diary

NTFS can also keep a **change journal** (the "USN journal"): a diary where every change on the drive ("file created", "renamed", "deleted") is written down with a sequence number (the USN). DriveDex remembers the journal's ID and the last sequence number it saw. Next time, instead of reading the whole catalogue again, it asks: *"what has happened since entry #1,234,567?"* and applies just those changes to its index — typically in **milliseconds**.

If the diary can't be trusted (it was switched off and on again, or the part we need has already been thrown away to save space), DriveDex notices and does a full read instead. It never guesses.

**In short:** the MFT is the drive's own catalogue (fast to read in one go); the USN journal is its diary of changes (fast to catch up with).

## 5. HDD vs flash, and why the number of workers matters

To scan quickly, DriveDex reads several folders **at the same time** using a pool of worker threads (in Go, *goroutines* — very cheap threads) that share a queue of folders still to read. How many workers is best depends on the drive:

- **Flash storage (SSD, pen drive, SD card)** has no moving parts. It happily answers many requests at once — more workers means more throughput, up to a point. DriveDex uses `min(16, 2 × your CPU cores)`.
- A **spinning hard disk** has one read head. Asking it for ten different places at once makes the head jump back and forth ("seek thrashing") and is *slower* than asking politely, one or two at a time. DriveDex uses **2**.
- If Windows can't tell what kind of drive it is, DriveDex takes a middle value (**4**).

DriveDex asks Windows ("does this drive have a *seek penalty*?" — `IOCTL_STORAGE_QUERY_PROPERTY`) and prints what it chose and why. You can override it (`--workers 8`) and measure with `drivedex bench-scan E: --workers 1,2,4,8,16`. The same logic picks the number of files copied at once by `drivedex copy`.

**In short:** parallelism helps flash and hurts spinning disks, so DriveDex checks which one it's dealing with.

## 6. Phones: MTP vs ADB

When you browse an Android phone in Explorer, Windows talks **MTP** (Media Transfer Protocol). MTP is a polite, one-question-at-a-time protocol designed for syncing music: "list this folder" → wait for the answer → "list that folder" → wait… A camera roll with thousands of folders turns into thousands of slow round trips over USB, and Explorer's search over MTP is notoriously slow or hangs.

**ADB** (Android Debug Bridge) is a developer tool that lets the PC run a *command on the phone itself*. DriveDex sends **one** command — roughly "find every file under /sdcard and print its size, date and path" — the phone runs it locally at full speed, and the whole list streams back in a single response. The same trick is used to copy: `adb pull` moves many files in one go.

What you need: turn on *USB debugging* on the phone and install Google's Platform-Tools (`drivedex index phone` prints the exact steps if something is missing, and `drivedex doctor` checks them). iPhones have no equivalent, so they aren't supported. DriveDex only ever **reads** from the phone (find, stat, pull); it never writes to it.

**In short:** MTP asks the phone thousands of small questions; ADB gives it one big job.

## 7. How the index stores paths: parent IDs, and why renames are cheap

You might expect an index to store full paths like `E:\Photos\2024\Trip\IMG_001.jpg` for every file. DriveDex doesn't. Each entry stores only:

- its own **ID** (on NTFS: the file's number in the MFT; elsewhere: just a counter),
- its **parent's ID**,
- its **name** (one piece — `IMG_001.jpg`), plus size, date and a few flags.

A path is **rebuilt on demand** by following parent IDs up to the top: `IMG_001.jpg` → parent `Trip` → parent `2024` → parent `Photos`. This is only done for the handful of results you see, never for the millions of entries that were merely examined.

Why this is clever:

1. **Small.** A folder name that 50,000 files live in is stored once, not 50,000 times. A million-entry index is about 38 MB on disk.
2. **Renaming a folder is one change.** Rename `Trip` to `Italy 2024` and one entry changes; every file below it automatically has the new path, because their paths are computed from the chain. With stored full paths you would have to rewrite every file under it. This is exactly what the USN journal reports ("folder X renamed to Y"), so applying journal entries stays cheap.
3. **Moving a folder** is likewise one change: its parent ID.

On disk, each entry is written with *variable-length integers* (small numbers take one byte) and the file ends with a **CRC-32 checksum**; it is saved by writing a temporary file and then renaming it, so a crash or power cut can never leave a half-written index — you get the old file or the new one.

**In short:** store "name + parent" and compute paths when needed; that's small, fast, and makes renames and moves free.

## 8. How search is so fast

Everything is arranged so that scanning a million names is just walking through memory:

- **Flat arrays ("struct of arrays").** Instead of a million small objects, an index keeps separate arrays: one of names, one of sizes, one of dates, one of flags… A search that only looks at names streams through one contiguous array, which a CPU is very good at. Names are also kept in a pre-computed lowercase copy, so "not case sensitive" costs nothing at search time.
- **All cores at once.** Each index is cut into chunks and every chunk is searched on its own goroutine. Each one keeps only its own best handful of results in a small heap, and the heaps are merged at the end — so a query that matches 900,000 names never sorts 900,000 results.
- **Cheap filters first.** In the query language (`ext:`, `size:`, `modified:`, `path:`, `re:`…) the cheap tests (file/folder, extension, name words) run first; the expensive ones (building a full path, running a regular expression) only run on entries that already passed the cheap ones.
- **Ranking.** Results are ordered by how good the match is: exact name (best) → name starts with your first word → your first word starts a word inside the name (after a space, `_`, `-`, `.` or `(`) → anywhere else; ties go to the shorter name, then files before folders, then alphabetical order of the path.
- **Offline drives cost nothing extra** — they're just another array in memory. DriveDex only checks which drives are connected to decide how to display each result (and for `drive:` filters); it never needs the drive to search it.

Measured on 1,000,000 entries in a 4-core cloud container: a few milliseconds to ~20 ms for plain words, up to ~70 ms for a regex or path filter (see [BENCHMARKS.md](BENCHMARKS.md)).

**In short:** flat arrays + every CPU core + cheap checks first = a million names in milliseconds.

## 9. Recognising a drive: serial numbers, not letters

`E:` isn't the drive's identity — it's just whichever letter Windows handed out today. A pen drive can be `E:` this morning and `G:` this afternoon. When a drive is formatted, Windows writes a **volume serial number** into it, and that never changes. DriveDex names each saved index by that serial (`vol-1A2B3C4D.ddx`) and only looks up the *current* letter when it needs to show a path.

Results from a drive that's plugged in show its real path (`G:\Photos\IMG_001.jpg`). Results from an unplugged drive show where it was last seen: `[SANDISK 32GB · offline · last seen E:] \Photos\IMG_001.jpg`. Known limits: two *cloned* drives have the same serial, and reformatting creates a new serial (so a new index).

**In short:** a drive is its serial number; the letter is just today's nickname.

## 10. Copying, the agent, and staying safe

- **Copy** (`drivedex copy`): first **plan** (list what would be copied, skipped or overwritten — that's `--dry-run`), then **run** with a pool of workers. Each file is written under a temporary name (`.ddxpart`) and renamed only when complete, so an unplug never leaves a half-file that looks real. Run the same command again and it **resumes**: files already there (same size and time) are skipped. Free space and the FAT32 4 GiB limit are checked *before* starting. `--verify` compares CRC-32 checksums afterwards. For phones, files are pulled into a hidden staging folder and renamed into place once they arrive.
- **The agent** (`drivedex-agent.exe`) is a tiny program with no window. It checks for new USB drives and phones every few seconds and runs `drivedex index` for them, so your indexes stay fresh without you remembering. A lock file stops it and a hand-run `index` from rebuilding the same index at once.
- **Safety rules.** DriveDex is **read-only** on the drives it indexes. The only commands that write to a drive are `copy` (what you asked for) and `doctor --bench` (one temporary file, always deleted). It never writes to a phone. `forget`/`uninstall` only ever delete DriveDex's own files, by exact name. Installing asks y/N before every change to your system, and nothing needs administrator rights unless you choose NTFS turbo mode.

## 11. USB 2.0 vs USB 3: why software can't speed up a port

**The wires.** Look inside a USB-A port. A USB 2.0 port has **4 contacts** (power, ground, and one pair of data wires). A USB 3 port has those 4 *plus 5 more* set further back: two extra data pairs (one to send, one to receive) and a ground. USB 3's speed lives entirely on those extra wires. Put a USB 3 pen drive in a USB 2.0 port and the extra contacts on the plug touch nothing, so the drive and the PC agree to talk over the old pair at **480 Mbit/s — about 40 MB/s in practice**. No program can send data over wires that aren't there.

**The names are a mess** (this trips everyone up): *USB 3.0*, *USB 3.1 Gen 1* and *USB 3.2 Gen 1* are the **same thing**, 5 Gbit/s — it was renamed twice. *USB 3.2 Gen 2* is 10 Gbit/s, *Gen 2x2* is 20. So a "USB 3.2" pen drive is usually a 5 Gbit/s one. And the port's colour (blue inside, or an "SS" logo) is a convention, not a rule.

**The drive is often the real limit.** 5 Gbit/s allows ~450 MB/s, but many cheap "USB 3" sticks *write* at 20–40 MB/s — no faster than USB 2.0. `drivedex doctor --bench E:` measures the write speed so you know which one is holding you back.

**What DriveDex does about it.** Windows' USB hub driver knows, for every port: which USB versions the port speaks, how fast the device in it is running, and whether that device *could* run faster (it's how Windows decides to show "This device can perform faster if connected to a USB 3.0 port"). DriveDex follows a drive letter down to its port — volume → physical disk → disk device → up the device tree to the USB hub, whose *address* for the device is the port number — and asks the hub (`IOCTL_USB_GET_NODE_CONNECTION_INFORMATION_EX_V2`). Then it can say *which* thing is in the way:

- **the port** is USB 2.0 only → move to another port (`drivedex ports` counts the PC's USB 3 ports, including USB-C ones, which work through a USB-C→A adapter *marked USB 3*);
- **a hub, dock or adapter** in between is USB 2.0 → plug straight in;
- **the port is USB 3 but the link fell back** → a USB 2.0 extension cable or adapter, or a plug not pushed all the way in. A USB 3 socket is really *two* ports to Windows (its USB 3 half and its USB 2.0 half, "companions"); a drive that lands on the USB 2.0 half of a USB 3 socket gives this away.

Windows usually can't tell *where* a port is on the case, so `drivedex ports --watch` lets you find out: move the drive from port to port and it prints each port's speed as you plug in.

**And within the limit, the copy wastes less.** On USB, at most two big files are copied at once (the link moves one stream as fast as eight; finished files are what survive an unplug); a file's space is reserved before writing it (so files written side by side don't interleave on the drive); and the file's date is set through the already-open file instead of opening it a second time (on a pen drive, every extra little write to a folder entry is slow).

**In short:** speed lives in the wires; DriveDex can't add wires, but it tells you exactly which port, hub or cable is missing them.

## 12. Interview cheat sheet

**"What is it, in one sentence?"** A Windows tool that indexes external drives and Android phones once, then searches them instantly — even when unplugged — and copies files fast and resumably.

**"Why is it fast?"** Three reasons: it searches an in-memory index instead of the drive; it reads NTFS drives by reading the Master File Table in one pass and refreshes from the change journal; and it tunes its parallelism to the drive type (few workers for HDD, many for flash).

**"What's the hardest bug / most interesting design decision?"** Pick one: (a) storing parent IDs instead of paths so renames are cheap and the index is small; (b) the Explorer right-click menu command line — `"E:\"` is read by Windows as an *escaped quote*, so the spec's form silently corrupted arguments; the fix is a trailing space inside the quotes, proved by a test that re-implements Windows' command-line parsing rules; (c) making all the Windows-only parts (raw disk access, registry, ADB) testable on any machine by hiding them behind small interfaces and writing in-memory fakes (a fake NTFS volume, a fake phone).

**"How did you test things you can't run in CI?"** Interfaces + fakes for everything hardware-shaped (`ntfs.Device`, `phone.Runner`, the registry), and a checklist of hardware checks the owner runs on real drives and records in `PROGRESS.md` — with nothing marked "passed" without evidence.

**"Anything you're proud of that isn't search?"** The USB link check: following a drive letter through the Windows device tree to its hub port and asking the hub driver whether the device could run faster — so a user learns *the port is USB 2.0 only* instead of blaming the drive.

**"What would you do next?"** Duplicate finder (group by size, then hash), disk-usage view from the index, content search, iPhone support, a GUI on top of the same packages.

**"What are the limits?"** Android only (no iPhone); turbo mode needs administrator rights; cloned drives share a serial number; the index is a snapshot until refreshed.

**Words worth knowing:** index · file system · NTFS / exFAT · MFT · USN journal · `DeviceIoControl` · seek penalty · goroutine / worker pool · MTP · ADB · CRC-32 · USB 2.0 (480 Mbit/s) vs USB 3 (5 Gbit/s+), companion ports · atomic write (write a temp file, then rename) · struct-of-arrays · CGO (avoided: the program is one `.exe` with no extra DLLs).
