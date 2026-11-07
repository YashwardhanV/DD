// Package copier copies files fast, safely and resumably.
//
// Why not just use Explorer? Explorer copies one file at a time and pays the
// per-file overhead (open, create, close, antivirus scan) serially. With many
// small files that overhead dominates. DriveDex copies with several workers at
// once, starts the biggest files first, and:
//
//   - never leaves a half-written file under its real name (it writes
//     "name.ddxpart" and renames it when complete),
//   - can be stopped (Ctrl+C) and re-run: files that already arrived
//     correctly are skipped, so a failed 40 GB copy does not start from zero,
//   - copies from Android phones with "adb pull" instead of slow MTP.
//
// A copy has two phases. PLAN: list the source, decide for each file whether
// to copy, overwrite or skip it, and check there is room. RUN: do it.
package copier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/manifest"
	"github.com/yashwardhanv/dd/internal/scan"
)

// Conflict policies for files that already exist at the destination.
const (
	OnConflictNewer     = "newer"     // overwrite only if the source is newer (default)
	OnConflictSkip      = "skip"      // never overwrite
	OnConflictOverwrite = "overwrite" // always overwrite
)

// fat32MaxFile is the largest file FAT32 can store (4 GiB minus one byte).
const fat32MaxFile = 4 << 30

// Decision says what the plan will do with one file.
type Decision int

const (
	Copy       Decision = iota // destination doesn't exist: copy
	Overwrite                  // destination exists and is replaced
	SkipSame                   // already there with the same size and time: this is "resume"
	SkipNewer                  // destination is newer than the source: kept
	SkipPolicy                 // --on-conflict skip: existing file left alone
)

func (d Decision) String() string {
	switch d {
	case Copy:
		return "copy"
	case Overwrite:
		return "overwrite"
	case SkipSame:
		return "skip (already there)"
	case SkipNewer:
		return "skip (destination is newer)"
	case SkipPolicy:
		return "skip (exists)"
	}
	return "?"
}

// Skipped reports whether the file will not be written.
func (d Decision) Skipped() bool { return d >= SkipSame }

// Job is one file to copy.
type Job struct {
	// Src is where to read from: a local path, or the phone path ("/sdcard/DCIM/a.jpg").
	Src string
	// Rel is the destination path relative to the destination folder (OS separators).
	Rel string
	// Size in bytes, and ModTime in unix seconds, as reported by the source.
	Size, ModTime int64
	// ModTimeNs is the modification time in nanoseconds since 1970 (UTC). 0
	// means "only ModTime is known" (see MTimeNs).
	ModTimeNs int64
	// TimeCoarse: the source only knows whole seconds (phones, indexes written
	// before exact times existed), so comparisons allow at least one second.
	TimeCoarse bool
	// SrcRel is the original destination-relative path when FixNames had to
	// change it (a name Windows can't store, or a letter-case clash); the
	// copy manifest keeps it so later runs recognise the file.
	SrcRel string
	// SrcFileID is the source's NTFS file reference number when known
	// (plans from an index), 0 otherwise.
	SrcFileID uint64
}

// MTimeNs is the job's modification time in nanoseconds.
func (j Job) MTimeNs() int64 {
	if j.ModTimeNs != 0 {
		return j.ModTimeNs
	}
	return j.ModTime * 1e9
}

// DstState is what the destination held for an item when it was planned
// (and, after a copy, what the copy looks like).
type DstState struct {
	Exists, IsDir bool
	Size, MTimeNs int64
}

func dstStateOf(fi os.FileInfo) DstState {
	if fi == nil {
		return DstState{}
	}
	return DstState{Exists: true, IsDir: fi.IsDir(), Size: fi.Size(), MTimeNs: fi.ModTime().UnixNano()}
}

// SkipWhy says why a file was found up to date (for the report).
type SkipWhy uint8

const (
	WhyNone     SkipWhy = iota
	WhySizeTime         // same size and time (fast / strict)
	WhyJournal          // the manifest and the NTFS change journal prove it unchanged
	WhyChecksum         // same content hash
	WhyMoved            // sync: moved on the destination to follow a source move
)

// Item is a Job plus what the plan decided about it.
type Item struct {
	Job
	Decision Decision
	// Dst is the destination as found when planning.
	Dst DstState
	// Why says which comparison found it up to date (skipped items).
	Why SkipWhy
	// Changed: size and time looked the same, but the comparison mode found
	// different content (checksum) or a change in the journal (journal).
	Changed bool
	// DstModified: the destination copy is not what DriveDex wrote there
	// (it differs from its manifest record): something else changed it.
	DstModified bool
	// MoveFrom (sync): the file is already on the destination at this other
	// path, from before the source moved it; it is renamed instead of copied.
	MoveFrom string
	// Blocked is set when the destination exists but can't be replaced (it is a folder).
	Blocked string
	// DSTShift: the destination differs by exactly one hour on a drive that
	// stores local time (FAT32), the mark of a daylight-saving change, and is
	// treated as unchanged (SkipSame).
	DSTShift bool

	// Filled in when the file is copied (they feed the copy manifest):
	Hash         [16]byte // XXH3-128 of the bytes read from the source
	Hashed       bool     // Hash is valid
	SrcSize      int64    // the source as it was when it was copied
	SrcMTimeNs   int64
	DstMTimeNs   int64 // the modification time given to the copy
	MediaChecked bool  // read back from the destination without the cache and compared
	// Renamed: FixNames gave this file a different destination name (SrcRel
	// holds the original).
	Renamed bool
}

// Plan is everything a copy is going to do.
type Plan struct {
	DstRoot string
	Dirs    []string // destination folders to create (relative, OS separators)
	Items   []Item   // largest files first
	Phone   *PhoneSource

	OnConflict string

	// SrcFS and DstFS describe the two file systems; they decide how
	// modification times are compared (fsinfo.SameTime).
	SrcFS, DstFS fsinfo.Info

	// LinksSkipped counts symbolic links and junctions in the source. They are
	// never followed or copied (that could copy files from somewhere else).
	LinksSkipped int64
	// NewDirs are the destination folders that don't exist yet (filled by
	// finalize); each takes at least one cluster.
	NewDirs int
	// NamesFixed is set once FixNames has run (Run runs it with the defaults
	// if nobody did, so a name Windows can't store is never written as-is).
	NamesFixed bool
	// FromIndex: planned from a saved index (PlanFromIndex), not a listing.
	FromIndex bool
	// Top is the folder the source becomes inside DstRoot (the source
	// folder's own name), "" when single files are copied.
	Top string
	// UnlistedDirs are source folders (destination-relative, like Rel) that
	// could not be listed completely. A sync removes nothing under them.
	UnlistedDirs []string

	// dstNames caches the destination folders read while deciding (one
	// directory read per folder), for FixNames.
	dstNames map[string][]string
}

// DSTShifts counts files treated as unchanged although their times differ by
// exactly one hour (daylight saving on a FAT32 drive).
func (p *Plan) DSTShifts() int64 {
	var n int64
	for i := range p.Items {
		if p.Items[i].DSTShift {
			n++
		}
	}
	return n
}

// Counts summarises the plan.
type Counts struct {
	Files, Copy, Overwrite, Skipped int64
	TotalBytes                      int64 // size of every file in the plan
	WriteBytes                      int64 // size of the files that will actually be written
}

// Counts tallies the plan.
func (p *Plan) Counts() Counts {
	var c Counts
	for _, it := range p.Items {
		c.Files++
		c.TotalBytes += it.Size
		switch it.Decision {
		case Copy:
			c.Copy++
			c.WriteBytes += it.Size
		case Overwrite:
			c.Overwrite++
			c.WriteBytes += it.Size
		default:
			c.Skipped++
		}
	}
	return c
}

// ValidPolicy reports whether s is a known --on-conflict value.
func ValidPolicy(s string) bool {
	switch s {
	case OnConflictNewer, OnConflictSkip, OnConflictOverwrite:
		return true
	}
	return false
}

// decision is what decide concluded about one file.
type decision struct {
	d       Decision
	blocked string
	shift   bool // unchanged except for a daylight-saving hour
}

// decide applies the conflict rules to one file. dst is nil when the
// destination doesn't exist.
//
//	same size and SameTime            -> skip  (this is how resume works)
//	same size, exactly 1 h apart, FAT -> skip, counted as a daylight-saving shift
//	different + newer                 -> overwrite only if the source is newer
//	different + skip                  -> skip
//	different + overwrite             -> overwrite
//
// Times are compared with the coarser of the two file systems' resolutions
// (fsinfo.SameTime), not a fixed 2 seconds: NTFS to NTFS needs no slack at
// all, so an edit within the same second is not missed (INV-1).
func decide(policy string, j Job, dst os.FileInfo, srcFS, dstFS fsinfo.Info) decision {
	return decideWith(policy, j, dst, srcFS, dstFS, true)
}

// decideWith is decide; allowShift=false is --compare strict: a daylight-
// saving hour is a difference like any other.
func decideWith(policy string, j Job, dst os.FileInfo, srcFS, dstFS fsinfo.Info, allowShift bool) decision {
	if dst == nil {
		return decision{d: Copy}
	}
	if dst.IsDir() {
		return decision{d: Copy, blocked: "the destination is a folder, not a file"}
	}
	srcNs, dstNs := j.MTimeNs(), dst.ModTime().UnixNano()
	if dst.Size() == j.Size {
		same, shift := fsinfo.SameTime(srcNs, dstNs, srcFS, dstFS, j.TimeCoarse)
		if same {
			return decision{d: SkipSame}
		}
		if shift && allowShift {
			return decision{d: SkipSame, shift: true}
		}
	}
	switch policy {
	case OnConflictSkip:
		return decision{d: SkipPolicy}
	case OnConflictOverwrite:
		return decision{d: Overwrite}
	default: // newer
		if srcNs > dstNs {
			return decision{d: Overwrite}
		}
		return decision{d: SkipNewer}
	}
}

// finalize resolves conflicts for jobs (statting destinations in parallel),
// sorts so big files go first, and assembles the Plan. srcFS describes where
// the jobs are read from.
func finalize(dstRoot string, dirs []string, jobs []Job, policy string, workers int, srcFS fsinfo.Info) *Plan {
	if policy == "" {
		policy = OnConflictNewer
	}
	dstFS := fsinfo.OrDefault(dstRoot)
	listing := checkDestination(dstRoot, jobs, workers, dstFS)

	// Largest first: big files start early instead of ending as one lonely
	// straggler while every other worker sits idle. Ties by name for a
	// predictable order.
	// PERF: sort positions, not Items: an Item is ~200 bytes, and moving
	// them around was a sixth of planning a 200,000-file copy.
	order := make([]int32, len(jobs))
	for i := range order {
		order[i] = int32(i)
	}
	sort.Slice(order, func(a, b int) bool {
		ja, jb := &jobs[order[a]], &jobs[order[b]]
		if ja.Size != jb.Size {
			return ja.Size > jb.Size
		}
		return ja.Rel < jb.Rel
	})
	items := make([]Item, len(jobs))
	parallelFor(len(jobs), workers, func(i int) {
		j := jobs[order[i]]
		items[i].Job = j
		st := listing.lookup(j.Rel)
		dec := decide(policy, j, st, srcFS, dstFS)
		items[i].Decision, items[i].Blocked, items[i].DSTShift = dec.d, dec.blocked, dec.shift
		items[i].Dst = dstStateOf(st)
		if dec.d == SkipSame {
			items[i].Why = WhySizeTime
		}
	})
	sort.Strings(dirs)
	p := &Plan{DstRoot: dstRoot, Dirs: dirs, Items: items, OnConflict: policy, SrcFS: srcFS, DstFS: dstFS, dstNames: listing.names}
	p.countNewDirsWith(listing)
	return p
}

// dstListing is what the destination folders hold, read once per folder.
type dstListing struct {
	infos   map[string]os.FileInfo // "<folder>\x00<folded name>" -> info
	names   map[string][]string    // folder -> every name in it (for case collisions)
	missing map[string]bool        // folders known not to exist
	allNew  bool                   // the destination itself doesn't exist yet
	fold    func(string) string
}

func (d *dstListing) lookup(rel string) os.FileInfo {
	if d.allNew || len(d.infos) == 0 {
		return nil
	}
	dir, name := splitDirBase(rel)
	return d.infos[dir+"\x00"+d.fold(name)]
}

// splitDirBase splits a relative path at its last separator, without the
// cleaning (and allocations) filepath.Dir and Base do.
func splitDirBase(rel string) (dir, name string) {
	i := strings.LastIndexByte(rel, filepath.Separator)
	if filepath.Separator != '/' {
		if j := strings.LastIndexByte(rel, '/'); j > i {
			i = j
		}
	}
	if i < 0 {
		return "", rel
	}
	return rel[:i], rel[i+1:]
}

// checkDestination reads every destination folder the jobs go to, once, in
// parallel (instead of one Lstat per file). A folder that doesn't exist means
// every file below it is new, and nothing below it is read. On Windows the
// sizes and times come with the folder listing itself (FindFirstFile data),
// so this costs one call per folder. Names are matched the way the
// destination matches them: ignoring case unless it is case-sensitive.
func checkDestination(dstRoot string, jobs []Job, workers int, dstFS fsinfo.Info) *dstListing {
	d := &dstListing{infos: map[string]os.FileInfo{}, names: map[string][]string{}, missing: map[string]bool{}, fold: func(s string) string { return s }}
	if !dstFS.CaseSensitive {
		d.fold = strings.ToUpper
	}
	if _, err := os.Lstat(dstRoot); err != nil {
		d.allNew = true // nothing there yet: every file is new, nothing to read
		return d
	}
	seen := map[string]bool{}
	var folders []string
	for _, j := range jobs {
		dir, _ := splitDirBase(j.Rel)
		if !seen[dir] {
			seen[dir] = true
			folders = append(folders, dir)
		}
	}
	sort.Strings(folders) // parents before children
	missing := d.missing
	var mu sync.Mutex
	// Walk folders top-down in "waves" by depth, so a missing parent is known
	// before its children are tried.
	byDepth := map[int][]string{}
	maxDepth := 0
	for _, f := range folders {
		depth := 0
		if f != "" {
			depth = strings.Count(f, string(filepath.Separator)) + 1
		}
		byDepth[depth] = append(byDepth[depth], f)
		maxDepth = max(maxDepth, depth)
	}
	parentMissing := func(f string) bool {
		for p := filepath.Dir(f); p != "." && p != f; p = filepath.Dir(p) {
			if missing[p] {
				return true
			}
			f = p
		}
		return false
	}
	for depth := 0; depth <= maxDepth; depth++ {
		wave := byDepth[depth]
		parallelFor(len(wave), workers, func(i int) {
			f := wave[i]
			mu.Lock()
			skip := parentMissing(f)
			mu.Unlock()
			if skip {
				mu.Lock()
				missing[f] = true
				mu.Unlock()
				return
			}
			entries, err := os.ReadDir(filepath.Join(dstRoot, f))
			if err != nil {
				mu.Lock()
				missing[f] = true
				mu.Unlock()
				return
			}
			names := make([]string, 0, len(entries))
			infos := make(map[string]os.FileInfo, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
				if info, err := e.Info(); err == nil {
					infos[f+"\x00"+d.fold(e.Name())] = info
				}
			}
			mu.Lock()
			d.names[f] = names
			for k, v := range infos {
				d.infos[k] = v
			}
			mu.Unlock()
		})
	}
	return d
}

// countNewDirs counts the destination folders the copy will create.
func (p *Plan) countNewDirs() { p.countNewDirsWith(nil) }

// countNewDirsWith is countNewDirs using what checkDestination already knows,
// so planning a big copy to a new folder makes no extra system calls.
func (p *Plan) countNewDirsWith(known *dstListing) {
	seen := map[string]bool{}
	n := 0
	exists := func(rel string) bool {
		if known != nil {
			if known.allNew || known.missing[rel] {
				return false
			}
			if _, ok := known.names[rel]; ok {
				return true
			}
		}
		_, err := os.Lstat(filepath.Join(p.DstRoot, rel))
		return err == nil
	}
	var walk func(rel string)
	walk = func(rel string) {
		for rel != "" && rel != "." && !seen[rel] {
			seen[rel] = true
			if exists(rel) {
				return // it exists, and so do its parents
			}
			n++
			rel, _ = splitDirBase(rel)
		}
	}
	for _, d := range p.Dirs {
		walk(d)
	}
	for i := range p.Items {
		if !p.Items[i].Decision.Skipped() {
			dir, _ := splitDirBase(p.Items[i].Rel)
			walk(dir)
		}
	}
	p.NewDirs = n
}

// SpaceNeeded is how much free space writing the plan really takes: every
// written file rounded up to whole clusters (allocation units), plus one
// cluster per new folder, plus 1 percent margin. On NTFS, files under about
// 700 bytes live inside their MFT record and take no cluster at all.
//
// The difference matters for many small files on exFAT drives, whose default
// clusters are 32 to 128 KB: 10,000 files of 1 KB take 1.25 GB at 128 KB each.
func (p *Plan) SpaceNeeded() (files, onDisk int64) {
	cl := p.DstFS.ClusterBytes
	if cl <= 0 {
		cl = 4096
	}
	for i := range p.Items {
		it := &p.Items[i]
		if it.Decision.Skipped() {
			continue
		}
		files += it.Size
		if p.DstFS.IsNTFS() && it.Size < ntfsResidentMax {
			continue
		}
		onDisk += (it.Size + cl - 1) / cl * cl
	}
	onDisk += int64(p.NewDirs) * cl
	onDisk += onDisk / 100
	return files, onDisk
}

// ntfsResidentMax: NTFS stores files smaller than this inside their MFT
// record ("resident"), using no cluster. The real limit depends on the length
// of the name and other attributes; ~700 bytes is the usual figure for a
// 1 KB record. A starting value to calibrate on real hardware.
const ntfsResidentMax = 700

// parallelFor runs fn(0..n-1) on up to `workers` goroutines.
func parallelFor(n, workers int, fn func(i int)) {
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}
	var wg sync.WaitGroup
	next := make(chan int, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Local sources
// ---------------------------------------------------------------------------

// srcName is the name a source folder gets inside the destination. For a
// drive root such as `E:\` (no name of its own) it is the drive letter.
func srcName(path string) string {
	clean := filepath.Clean(path)
	if v := filepath.VolumeName(clean); v != "" && strings.TrimPrefix(clean, v) == string(filepath.Separator) {
		return strings.TrimRight(v, ":")
	}
	if base := filepath.Base(clean); base != "." && base != string(filepath.Separator) {
		return base
	}
	return "copy"
}

// PlanLocal plans copying a local file or folder into dstRoot.
//
// A folder is copied INTO dstRoot as dstRoot\<folder name>\..., like Explorer
// does; a single file lands directly in dstRoot. workers is used for listing
// the source and checking existing destination files.
func PlanLocal(ctx context.Context, src, dstRoot, policy string, workers int) (*Plan, error) {
	st, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", src, err)
	}

	srcFS := fsinfo.OrDefault(src)
	if !st.IsDir() {
		job := Job{Src: src, Rel: filepath.Base(src), Size: st.Size(), ModTime: st.ModTime().Unix(), ModTimeNs: st.ModTime().UnixNano()}
		return finalize(dstRoot, nil, []Job{job}, policy, 1, srcFS), nil
	}

	if inside(src, dstRoot) {
		return nil, fmt.Errorf("%w: %s is inside %s", ErrDestinationInsideSource, dstRoot, src)
	}

	name := srcName(src)
	sc := scan.NewParallel(src, workers)
	// A copy manifest folder in the source (it was once a destination) is
	// DriveDex's own bookkeeping, not data: never copied.
	sc.SkipDirs = append(append([]string(nil), scan.DefaultSkipDirs...), ManifestDirName)
	b := index.NewBuilder(index.Header{})
	if err := sc.Scan(ctx, b, nil); err != nil {
		return nil, fmt.Errorf("listing %s: %w", src, err)
	}
	ix := b.Index()

	dirs := []string{name} // the top folder itself, even if the source is empty
	var jobs []Job
	var links int64
	cache := map[uint64]string{}
	for slot := 0; slot < ix.Len(); slot++ {
		rel := filepath.Join(name, strings.ReplaceAll(ix.RelPathCached(slot, cache), `\`, string(filepath.Separator)))
		if ix.Flags[slot]&index.FlagDir != 0 {
			dirs = append(dirs, rel)
			continue
		}
		if ix.Flags[slot]&index.FlagReparse != 0 {
			links++
			continue
		}
		jobs = append(jobs, Job{
			Src:        index.JoinRoot(src, ix.RelPathCached(slot, cache)),
			Rel:        rel,
			Size:       max(ix.Sizes[slot], 0),
			ModTime:    ix.MTimes[slot],
			ModTimeNs:  ix.ModTimeNs(slot),
			TimeCoarse: !ix.Header.ExactTimes(),
		})
	}
	p := finalize(dstRoot, dirs, jobs, policy, workers, srcFS)
	p.LinksSkipped, p.Top = links, name
	for _, d := range sc.Stats().FailedDirs {
		p.UnlistedDirs = append(p.UnlistedDirs, filepath.Join(name, d))
	}
	return p, nil
}

// ManifestDirName is the copy manifest's folder (manifest.DirName), skipped
// in every source.
const ManifestDirName = manifest.DirName

// ErrDestinationInsideSource means a folder would be copied into itself (or
// one of its own sub-folders). Explorer refuses that too: every run would copy
// the previous copy again, nesting one level deeper each time.
var ErrDestinationInsideSource = errors.New("the destination is inside the folder being copied")

// inside reports whether path is dir itself or somewhere below it. (On Windows
// filepath.Rel ignores case, as the file system does.)
func inside(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// PlanJobs plans copying an explicit list of files (for example every match of
// a search) into dstRoot. srcFS describes the files' file system (they may
// come from several drives; pass the most tolerant one).
func PlanJobs(dstRoot string, jobs []Job, policy string, workers int, srcFS fsinfo.Info) *Plan {
	return finalize(dstRoot, nil, append([]Job(nil), jobs...), policy, workers, srcFS)
}

// ---------------------------------------------------------------------------
// Pre-checks
// ---------------------------------------------------------------------------

// ErrNotEnoughSpace means the destination can't hold what is about to be copied.
var ErrNotEnoughSpace = errors.New("not enough free space at the destination")

// ErrFileTooBigForFAT32 means a file is 4 GiB or larger and the destination is FAT32.
var ErrFileTooBigForFAT32 = errors.New("a file is too big for a FAT32 destination")

// Checks are the system queries the pre-checks need. They are fields so tests
// can supply fake answers.
type Checks struct {
	// FreeBytes returns the free space on the drive holding path.
	FreeBytes func(path string) (uint64, error)
	// FileSystem returns the file system name ("NTFS", "FAT32", "exFAT") of the drive holding path.
	FileSystem func(path string) (string, error)
}

// DefaultChecks queries the real system.
func DefaultChecks() Checks { return Checks{FreeBytes: diskFree, FileSystem: fileSystemOf} }

// Check runs the two safety checks before anything is written. Each failure
// carries a clear explanation:
//
//   - the files to write must fit in the free space;
//   - FAT32 cannot store a file of 4 GiB or more.
func (p *Plan) Check(c Checks) error {
	counts := p.Counts()
	if counts.WriteBytes == 0 && counts.Copy+counts.Overwrite == 0 {
		return nil
	}

	probe := nearestExisting(p.DstRoot)
	if c.FreeBytes != nil {
		_, need := p.SpaceNeeded()
		if free, err := c.FreeBytes(probe); err == nil && uint64(need) > free {
			return fmt.Errorf("%w: need %d bytes but only %d are free", ErrNotEnoughSpace, need, free)
		}
	}
	if c.FileSystem != nil {
		if fsName, err := c.FileSystem(probe); err == nil && isFAT32(fsName) {
			for _, it := range p.Items {
				if !it.Decision.Skipped() && it.Size >= fat32MaxFile {
					return fmt.Errorf("%w: %s is %d bytes (FAT32 stores at most 4 GiB minus 1 byte)", ErrFileTooBigForFAT32, it.Rel, it.Size)
				}
			}
		}
	}
	return nil
}

func isFAT32(name string) bool {
	return strings.EqualFold(name, "FAT32") || strings.EqualFold(name, "FAT")
}

// nearestExisting returns path, or its closest ancestor that exists (the
// destination may not have been created yet).
func nearestExisting(path string) string {
	p := filepath.Clean(path)
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// ---------------------------------------------------------------------------
// Choosing the number of workers
// ---------------------------------------------------------------------------

// AutoWorkers picks how many files to copy at once. A spinning disk on either
// end makes parallel copying counter-productive (the arm thrashes), so then we
// use 2; if both ends are flash/SSD we use 8; if we can't tell, 4.
func AutoWorkers(srcPenalty, srcKnown, dstPenalty, dstKnown bool) int {
	switch {
	case (srcKnown && srcPenalty) || (dstKnown && dstPenalty):
		return 2
	case srcKnown && dstKnown:
		return 8
	default:
		return 4
	}
}
