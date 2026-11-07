package copier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/ui"
)

// PartSuffix marks a file that is still being written. The real name only ever
// appears when the whole file is on disk.
const PartSuffix = ".ddxpart"

const (
	bufferSize     = 1 << 20 // 1 MB copy buffer: big reads/writes keep USB drives busy
	defaultRetries = 2
	defaultDelay   = 500 * time.Millisecond
	progressEvery  = 200 * time.Millisecond
	maxFailures    = 20
)

// bufPool shares 1 MB buffers between workers instead of allocating one per file.
var bufPool = sync.Pool{New: func() any { b := make([]byte, bufferSize); return &b }}

// alignedPool shares buffers for unbuffered reads (VerifyMedia). Windows only
// reads without the cache into memory aligned to the disk's sector size, in
// whole sectors; 1 MiB is a multiple of every sector size in use.
var alignedPool = sync.Pool{New: func() any { b := alignedBuffer(bufferSize, maxSectorAlign); return &b }}

// maxSectorAlign is the alignment used for unbuffered reads. 4096 covers both
// 512-byte and 4 KB sector drives (a 4096-aligned address is 512-aligned too).
const maxSectorAlign = 4096

// alignedBuffer returns a size-byte slice whose first byte sits at an address
// that is a multiple of align: over-allocate by one alignment and slice.
func alignedBuffer(size, align int) []byte {
	raw := make([]byte, size+align)
	off := 0
	if r := int(uintptr(unsafe.Pointer(&raw[0])) % uintptr(align)); r != 0 {
		off = align - r
	}
	return raw[off : off+size : off+size]
}

// VerifyMode says how (and whether) each copied file is checked.
type VerifyMode int

const (
	// VerifyOff: no check after writing (the default without --verify).
	VerifyOff VerifyMode = iota
	// VerifyMedia: the source is hashed while it is copied (read once), the
	// copy is flushed to the device, then read back WITHOUT the Windows
	// cache (unbuffered) and its hash compared. This proves the bytes came
	// back from the drive itself, not from the PC's memory.
	VerifyMedia
	// VerifyCache: the copy is read back through the Windows cache. Cheaper,
	// but it only proves DriveDex wrote the right bytes, not that the drive
	// stored them (the read is usually served from RAM).
	VerifyCache
)

// ParseVerifyMode understands the values of --verify.
func ParseVerifyMode(s string) (VerifyMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "false", "no", "none":
		return VerifyOff, true
	case "media", "true", "yes", "on":
		return VerifyMedia, true
	case "cache":
		return VerifyCache, true
	}
	return VerifyOff, false
}

func (v VerifyMode) String() string {
	switch v {
	case VerifyMedia:
		return "media"
	case VerifyCache:
		return "cache"
	}
	return "off"
}

// Options controls a copy.
type Options struct {
	// Workers is how many files are copied at once (local copies). <= 0 means 4.
	Workers int
	// Verify says how each copied file is checked (see VerifyMode).
	Verify VerifyMode
	// Hash computes the XXH3-128 hash of every copied file even without
	// verification (the copy manifest records it).
	Hash bool
	// Progress, if set, is called about every 200 ms and once at the end.
	Progress func(ui.CopyProgress)
	// Retries is how many extra attempts a failing file gets (default 2);
	// RetryDelay is the wait between attempts (default 500 ms).
	Retries    int
	RetryDelay time.Duration
	// ReadOrder is the order small files are read in: ReadOrderSize (biggest
	// first, the default) or ReadOrderDisk (where they lie on the disk; for
	// spinning hard disks, experimental). Big files keep their own lanes.
	ReadOrder string
	// CaseSensitiveDirs: folders the copy creates get NTFS per-folder case
	// sensitivity (see NameOptions.CaseSensitiveDest).
	CaseSensitiveDirs bool
	// BigLanes, when > 0, is how many BIG files (BigFileSize and up) are
	// copied at once; the other workers keep copying small files meanwhile.
	// For USB drives: the link moves one stream as fast as eight, so eight
	// half-finished 4 GB films only mean more to lose if the drive is
	// unplugged (resume restarts unfinished files) and more fragmentation on
	// the destination. 0 = big files share all workers (the old behaviour).
	BigLanes int
	// Trash, when set, receives the old version of every file an Overwrite
	// replaces (moved there just before the new one is renamed in), instead
	// of it being lost (keep_replaced, INV-3).
	Trash Trasher
	// Record, when set, is called for each file once it is in place (and
	// verified, when verifying), from several goroutines at once. The item's
	// Dst then describes the copy, and Hash its content when hashing.
	Record func(it *Item)
}

// Trasher moves a destination file (relative to the plan's DstRoot) to the
// trash. *trash.Run is one.
type Trasher interface {
	Move(rel, why string) error
}

// TrashReplaced is the reason given to the Trasher for an overwritten file.
const TrashReplaced = "replaced"

// BigFileSize is where BigLanes starts to apply.
const BigFileSize = 32 << 20

// bigFileSize is BigFileSize (a variable so tests can use small files).
var bigFileSize int64 = BigFileSize

func (o Options) hashing() bool { return o.Hash || o.Verify != VerifyOff }

func (o Options) retries() int {
	if o.Retries <= 0 {
		return defaultRetries
	}
	return o.Retries
}

func (o Options) retryDelay() time.Duration {
	if o.RetryDelay <= 0 {
		return defaultDelay
	}
	return o.RetryDelay
}

// Failure is a file that could not be copied.
type Failure struct {
	Rel string
	Err string
}

// Result is the outcome of Run.
type Result struct {
	Written          int64 // files written (copied or overwritten)
	Skipped          int64 // files not written (already there, kept newer, not regular files)
	Failed           int64
	BytesWritten     int64
	VerifyMismatches int64
	Failures         []Failure // the first 20
	Elapsed          time.Duration
	Cancelled        bool
	NotRegular       int64 // links and other special files that were skipped
}

// state is the live, shared counters of a running copy.
type state struct {
	total                int64
	bytesTotal           int64
	filesDone, bytesDone atomic.Int64
	skipped, failed      atomic.Int64
	written, notRegular  atomic.Int64
	mismatches           atomic.Int64

	mu       sync.Mutex
	failures []Failure
}

func (s *state) fail(rel string, err error) {
	s.failed.Add(1)
	s.filesDone.Add(1)
	s.mu.Lock()
	if len(s.failures) < maxFailures {
		s.failures = append(s.failures, Failure{Rel: rel, Err: err.Error()})
	}
	s.mu.Unlock()
}

func (s *state) snapshot(speed float64) ui.CopyProgress {
	return ui.CopyProgress{
		FilesDone: s.filesDone.Load(), FilesTotal: s.total,
		BytesDone: s.bytesDone.Load(), BytesTotal: s.bytesTotal,
		Skipped: s.skipped.Load(), Failed: s.failed.Load(), BytesPerSec: speed,
	}
}

// Test hooks (never set in real use, except the defaults).
var (
	// openSource opens a source file. locked=true opens it so that no other
	// program can write to it while it is read (Windows: share-read only); a
	// file open for writing elsewhere then fails with errInUse. locked=false
	// is a normal open (other programs may keep writing; the before/after
	// check catches that).
	openSource = defaultOpenSource
	// openUnbuffered opens a copy for reading without the Windows cache
	// (VerifyMedia); unbufferedOpens counts its calls in tests.
	openUnbuffered = unbufferedOpen
	afterCopy      func(dst string) // runs after a file is renamed into place, before verification
	// duringCopy runs after the source has been read and before it is
	// checked again (tests use it to change the source mid-copy).
	duringCopy func(src string)
)

// Run carries out the plan. It stops promptly when ctx is cancelled (Result.Cancelled).
// An error is returned only for problems that stop the whole copy (such as not
// being able to create the destination folder); failures of individual files
// are reported in the Result and the copy carries on.
func Run(ctx context.Context, p *Plan, opt Options) (Result, error) {
	start := time.Now()
	counts := p.Counts()
	st := &state{total: counts.Files, bytesTotal: counts.WriteBytes}

	// Files that need no copying count as done right away.
	for _, it := range p.Items {
		if it.Decision.Skipped() {
			st.skipped.Add(1)
			st.filesDone.Add(1)
		}
	}

	if !p.NamesFixed {
		// Nobody checked the names (a library caller): apply the safe defaults
		// rather than write a name Windows can't store.
		p.FixNames(NameOptions{})
	}
	if err := makeDirs(p, opt.CaseSensitiveDirs); err != nil {
		return Result{}, err
	}

	// Progress reporter: smooths the speed over a few seconds.
	stopReport := make(chan struct{})
	var reportWG sync.WaitGroup
	if opt.Progress != nil {
		reportWG.Add(1)
		go func() {
			defer reportWG.Done()
			tick := time.NewTicker(progressEvery)
			defer tick.Stop()
			var speed float64
			last, lastT := int64(0), time.Now()
			for {
				select {
				case <-stopReport:
					return
				case now := <-tick.C:
					done := st.bytesDone.Load()
					inst := float64(done-last) / now.Sub(lastT).Seconds()
					if speed == 0 {
						speed = inst
					} else {
						speed = 0.7*speed + 0.3*inst // exponential smoothing: steady, but reacts within seconds
					}
					last, lastT = done, now
					opt.Progress(st.snapshot(speed))
				}
			}
		}()
	}

	var err error
	if p.Phone != nil {
		err = runPhone(ctx, p, opt, st)
	} else {
		runLocal(ctx, p, opt, st)
	}

	close(stopReport)
	reportWG.Wait()
	elapsed := time.Since(start)
	if opt.Progress != nil {
		speed := 0.0
		if elapsed > 0 {
			speed = float64(st.bytesDone.Load()) / elapsed.Seconds()
		}
		opt.Progress(st.snapshot(speed))
	}

	st.mu.Lock()
	res := Result{
		Written: st.written.Load(), Skipped: st.skipped.Load(), Failed: st.failed.Load(),
		BytesWritten: st.bytesDone.Load(), VerifyMismatches: st.mismatches.Load(),
		Failures: append([]Failure(nil), st.failures...), Elapsed: elapsed,
		Cancelled: ctx.Err() != nil, NotRegular: st.notRegular.Load(),
	}
	st.mu.Unlock()
	return res, err
}

func defaultOpenSource(name string, locked bool) (*os.File, error) {
	if locked {
		return lockedOpen(name)
	}
	return os.Open(name)
}

// joinDst is the full destination path of an item.
func joinDst(root, rel string) string { return filepath.Join(root, rel) }

// makeDirs creates every destination folder up front (including empty ones).
// With caseSensitive, each folder it creates gets NTFS per-folder case
// sensitivity (copy --case-sensitive-dest).
func makeDirs(p *Plan, caseSensitive bool) error {
	if err := os.MkdirAll(p.DstRoot, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", p.DstRoot, err)
	}
	made := map[string]bool{}
	var mk func(rel string) error
	mk = func(rel string) error {
		if rel == "" || rel == "." || made[rel] {
			return nil
		}
		made[rel] = true
		full := filepath.Join(p.DstRoot, rel)
		if _, err := os.Lstat(full); err == nil {
			return nil
		}
		if err := mk(filepath.Dir(rel)); err != nil {
			return err
		}
		if err := os.Mkdir(full, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("creating folder %s: %w", full, err)
		}
		if caseSensitive {
			fsinfo.SetCaseSensitiveDir(full) // best effort: needs NTFS and Windows 10 1803+
		}
		return nil
	}
	for _, d := range p.Dirs {
		if err := mk(d); err != nil {
			return err
		}
	}
	for _, it := range p.Items {
		if !it.Decision.Skipped() {
			if err := mk(filepath.Dir(it.Rel)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Local copies
// ---------------------------------------------------------------------------

func runLocal(ctx context.Context, p *Plan, opt Options, st *state) {
	workers := opt.Workers
	if workers <= 0 {
		workers = 4
	}
	bigLanes := opt.BigLanes
	if bigLanes < 0 || bigLanes >= workers {
		bigLanes = 0 // no point in a separate lane
	}
	isBig := func(it *Item) bool { return bigLanes > 0 && it.Size >= bigFileSize }

	smallQ := make(chan *Item)
	bigQ := make(chan *Item)
	var wg sync.WaitGroup
	work := func(queues ...chan *Item) {
		defer wg.Done()
		bufp := bufPool.Get().(*[]byte)
		defer bufPool.Put(bufp)
		var abuf []byte
		if opt.Verify == VerifyMedia {
			abufp := alignedPool.Get().(*[]byte)
			defer alignedPool.Put(abufp)
			abuf = *abufp
		}
		for _, q := range queues {
			for it := range q {
				copyWithRetries(ctx, p.DstRoot, it, *bufp, abuf, opt, st)
			}
		}
	}
	// Big-file workers help with the small files once the big ones are done.
	for w := 0; w < bigLanes; w++ {
		wg.Add(1)
		go work(bigQ, smallQ)
	}
	for w := 0; w < workers-bigLanes; w++ {
		wg.Add(1)
		go work(smallQ)
	}

	// Dispatch largest-first (the plan is already sorted), and stop handing out
	// work as soon as the user cancels. Big and small files have their own
	// dispatcher, so small files flow while the big lanes are busy.
	var dwg sync.WaitGroup
	dispatch := func(q chan *Item, mine func(*Item) bool) {
		defer dwg.Done()
		defer close(q)
		for i := range p.Items {
			it := &p.Items[i]
			if it.Decision.Skipped() || !mine(it) {
				continue
			}
			if it.Blocked != "" {
				st.fail(it.Rel, errors.New(it.Blocked))
				continue
			}
			select {
			case q <- it:
			case <-ctx.Done():
				return
			}
		}
	}
	dwg.Add(2)
	if opt.ReadOrder == ReadOrderDisk {
		ordered := diskOrder(p.Items, isBig)
		go func() {
			defer dwg.Done()
			defer close(smallQ)
			for _, it := range ordered {
				if it.Blocked != "" {
					st.fail(it.Rel, errors.New(it.Blocked))
					continue
				}
				select {
				case smallQ <- it:
				case <-ctx.Done():
					return
				}
			}
		}()
	} else {
		go dispatch(smallQ, func(it *Item) bool { return !isBig(it) })
	}
	go dispatch(bigQ, isBig)
	dwg.Wait()
	wg.Wait()
}
