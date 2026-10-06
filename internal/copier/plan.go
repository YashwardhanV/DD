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

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/scan"
)

// Conflict policies for files that already exist at the destination.
const (
	OnConflictNewer     = "newer"     // overwrite only if the source is newer (default)
	OnConflictSkip      = "skip"      // never overwrite
	OnConflictOverwrite = "overwrite" // always overwrite
)

// mtimeTolerance: FAT stores modification times in 2-second steps, so two
// copies of the "same" file can differ by up to 2 s. Within that, times are equal.
const mtimeTolerance = 2 // seconds

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
}

// Item is a Job plus what the plan decided about it.
type Item struct {
	Job
	Decision Decision
	// Blocked is set when the destination exists but can't be replaced (it is a folder).
	Blocked string
}

// Plan is everything a copy is going to do.
type Plan struct {
	DstRoot string
	Dirs    []string // destination folders to create (relative, OS separators)
	Items   []Item   // largest files first
	Phone   *PhoneSource

	OnConflict string
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

// decide applies the conflict rules to one file. dst is nil when the
// destination doesn't exist.
//
//	same size and times within 2 s  -> skip  (this is how resume works)
//	different + newer               -> overwrite only if the source is newer
//	different + skip                -> skip
//	different + overwrite           -> overwrite
func decide(policy string, srcSize, srcMTime int64, dst os.FileInfo) (Decision, string) {
	if dst == nil {
		return Copy, ""
	}
	if dst.IsDir() {
		return Copy, "the destination is a folder, not a file"
	}
	dstMTime := dst.ModTime().Unix()
	if dst.Size() == srcSize && abs64(dstMTime-srcMTime) <= mtimeTolerance {
		return SkipSame, ""
	}
	switch policy {
	case OnConflictSkip:
		return SkipPolicy, ""
	case OnConflictOverwrite:
		return Overwrite, ""
	default: // newer
		if srcMTime > dstMTime {
			return Overwrite, ""
		}
		return SkipNewer, ""
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// finalize resolves conflicts for jobs (statting destinations in parallel),
// sorts so big files go first, and assembles the Plan.
func finalize(dstRoot string, dirs []string, jobs []Job, policy string, workers int) *Plan {
	if policy == "" {
		policy = OnConflictNewer
	}
	items := make([]Item, len(jobs))
	parallelFor(len(jobs), workers, func(i int) {
		j := jobs[i]
		items[i].Job = j
		st, err := os.Lstat(filepath.Join(dstRoot, j.Rel))
		if err != nil {
			st = nil // not there (or unreadable: copying will report the real problem)
		}
		items[i].Decision, items[i].Blocked = decide(policy, j.Size, j.ModTime, st)
	})

	// Largest first: big files start early instead of ending as one lonely
	// straggler while every other worker sits idle. Ties by name for a
	// predictable order.
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].Size != items[b].Size {
			return items[a].Size > items[b].Size
		}
		return items[a].Rel < items[b].Rel
	})
	sort.Strings(dirs)
	return &Plan{DstRoot: dstRoot, Dirs: dirs, Items: items, OnConflict: policy}
}

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

	if !st.IsDir() {
		job := Job{Src: src, Rel: filepath.Base(src), Size: st.Size(), ModTime: st.ModTime().Unix()}
		return finalize(dstRoot, nil, []Job{job}, policy, 1), nil
	}

	if inside(src, dstRoot) {
		return nil, fmt.Errorf("%w: %s is inside %s", ErrDestinationInsideSource, dstRoot, src)
	}

	name := srcName(src)
	sc := scan.NewParallel(src, workers)
	b := index.NewBuilder(index.Header{})
	if err := sc.Scan(ctx, b, nil); err != nil {
		return nil, fmt.Errorf("listing %s: %w", src, err)
	}
	ix := b.Index()

	dirs := []string{name} // the top folder itself, even if the source is empty
	var jobs []Job
	cache := map[uint64]string{}
	for slot := 0; slot < ix.Len(); slot++ {
		rel := filepath.Join(name, strings.ReplaceAll(ix.RelPathCached(slot, cache), `\`, string(filepath.Separator)))
		if ix.Flags[slot]&index.FlagDir != 0 {
			dirs = append(dirs, rel)
			continue
		}
		jobs = append(jobs, Job{
			Src:     index.JoinRoot(src, ix.RelPathCached(slot, cache)),
			Rel:     rel,
			Size:    max(ix.Sizes[slot], 0),
			ModTime: ix.MTimes[slot],
		})
	}
	return finalize(dstRoot, dirs, jobs, policy, workers), nil
}

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
// a search) into dstRoot.
func PlanJobs(dstRoot string, jobs []Job, policy string, workers int) *Plan {
	return finalize(dstRoot, nil, append([]Job(nil), jobs...), policy, workers)
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
		if free, err := c.FreeBytes(probe); err == nil && uint64(counts.WriteBytes) > free {
			return fmt.Errorf("%w: need %d bytes but only %d are free", ErrNotEnoughSpace, counts.WriteBytes, free)
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
