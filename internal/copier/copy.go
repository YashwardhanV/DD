package copier

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yashwardhanv/dd/internal/copier/trace"
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

// castagnoli is the CRC-32C table; modern CPUs compute it in hardware.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Options controls a copy.
type Options struct {
	// Workers is how many files are copied at once (local copies). <= 0 means 4.
	Workers int
	// Verify re-reads source and destination after each file and compares
	// CRC-32C checksums.
	Verify bool
	// Progress, if set, is called about every 200 ms and once at the end.
	Progress func(ui.CopyProgress)
	// Retries is how many extra attempts a failing file gets (default 2);
	// RetryDelay is the wait between attempts (default 500 ms).
	Retries    int
	RetryDelay time.Duration
	// BigLanes, when > 0, is how many BIG files (BigFileSize and up) are
	// copied at once; the other workers keep copying small files meanwhile.
	// For USB drives: the link moves one stream as fast as eight, so eight
	// half-finished 4 GB films only mean more to lose if the drive is
	// unplugged (resume restarts unfinished files) and more fragmentation on
	// the destination. 0 = big files share all workers (the old behaviour).
	BigLanes int
	// Trace, if set, records where the time goes (drivedex copy --trace).
	// nil costs nothing.
	Trace *trace.Recorder

	// Phone copies (BUILD-V2 stage 16).
	// PhoneMode: "auto" or "" (tar stream when the phone supports it), "tar",
	// or "pull" (exactly v1).
	PhoneMode string
	// TarStreams is how many tar streams run at once (<= 0 means 1).
	TarStreams int
	// PullCompress: "auto"/"" or "off" (no compression), or "lz4", "zstd",
	// "brotli" to use that 'adb pull -z' algorithm for big compressible
	// files when the phone supports it.
	PullCompress string

	// LateChecks are the free-space and FAT32 checks applied to files the
	// reconciling sweep adds during the copy (the plan's own files were
	// checked by Plan.Check before the copy started).
	LateChecks Checks

	// Fast turns on the local fast paths of BUILD-V2 stage 18 (zero = v1).
	Fast FastPaths

	// Tune, when set, lets the number of workers adjust itself during a
	// local copy (stage 19), between Min and Max, starting at Workers.
	Tune *TuneOptions

	journal *resumeJournal // set by Run (18e)
	direct  bool           // small files go straight to their real names
}

// BigFileSize is where BigLanes starts to apply.
const BigFileSize = 32 << 20

// bigFileSize is BigFileSize (a variable so tests can use small files).
var bigFileSize int64 = BigFileSize

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
	// Vanished counts planned files that no longer existed when their turn
	// came (deleted since the index the plan came from was made). They are
	// not failures.
	Vanished int64
	// PhoneLane says how phone files were moved: "tar", "pull", or "pull
	// (...)" with the reason tar wasn't used. "" for local copies.
	PhoneLane string
	// Found counts files the reconciling sweep found while copying (not in
	// the plan, or changed since it was made).
	Found int64
	// StreamRejected counts entries in a phone's tar stream that were
	// refused (not requested, not a regular file, or a name that would
	// escape the destination). They are drained and ignored.
	StreamRejected int64
}

// state is the live, shared counters of a running copy.
type state struct {
	total, bytesTotal    atomic.Int64 // grow when the sweep finds files the plan missed
	found                atomic.Int64 // files the sweep added
	filesDone, bytesDone atomic.Int64
	skipped, failed      atomic.Int64
	written, notRegular  atomic.Int64
	mismatches           atomic.Int64
	vanished             atomic.Int64
	rejected             atomic.Int64

	laneMu    sync.Mutex
	phoneLane string

	mu       sync.Mutex
	failures []Failure
}

func (s *state) setPhoneLane(l string) {
	s.laneMu.Lock()
	s.phoneLane = l
	s.laneMu.Unlock()
}

// vanish records a planned file that no longer exists at the source.
func (s *state) vanish() {
	s.vanished.Add(1)
	s.skipped.Add(1)
	s.filesDone.Add(1)
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
		FilesDone: s.filesDone.Load(), FilesTotal: s.total.Load(),
		BytesDone: s.bytesDone.Load(), BytesTotal: s.bytesTotal.Load(),
		Skipped: s.skipped.Load(), Failed: s.failed.Load(), BytesPerSec: speed,
		Found: s.found.Load(),
	}
}

// Test hooks (never set in real use).
var (
	openSource = os.Open
	afterCopy  func(dst string) // runs after a file is renamed into place, before verification
)

// Run carries out the plan. It stops promptly when ctx is cancelled (Result.Cancelled).
// An error is returned only for problems that stop the whole copy (such as not
// being able to create the destination folder); failures of individual files
// are reported in the Result and the copy carries on.
func Run(ctx context.Context, p *Plan, opt Options) (Result, error) {
	start := time.Now()
	if opt.Trace == nil {
		opt.Trace = trace.From(ctx)
	}
	journal, direct, err := setupJournal(p, opt) // 18e; must come before the counts
	if err != nil {
		return Result{}, err
	}
	opt.journal, opt.direct = journal, direct
	counts := p.Counts()
	st := &state{}
	st.total.Store(counts.Files)
	st.bytesTotal.Store(counts.WriteBytes)

	// Files that need no copying count as done right away.
	for _, it := range p.Items {
		if it.Decision.Skipped() {
			st.skipped.Add(1)
			st.filesDone.Add(1)
		}
	}

	if err := makeDirs(p); err != nil {
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

	if p.Phone != nil {
		err = runPhone(ctx, p, opt, st)
	} else {
		runLocal(ctx, p, opt, st)
	}
	if journal != nil {
		journal.finish(ctx.Err() == nil && st.failed.Load() == 0)
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
		Vanished: st.vanished.Load(), StreamRejected: st.rejected.Load(),
		Found: st.found.Load(),
	}
	st.laneMu.Lock()
	res.PhoneLane = st.phoneLane
	st.laneMu.Unlock()
	st.mu.Unlock()
	return res, err
}

// makeDirs creates every destination folder up front (including empty ones).
func makeDirs(p *Plan) error {
	if err := os.MkdirAll(p.DstRoot, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", p.DstRoot, err)
	}
	made := map[string]bool{}
	mk := func(rel string) error {
		if rel == "" || rel == "." || made[rel] {
			return nil
		}
		made[rel] = true
		if err := os.MkdirAll(filepath.Join(p.DstRoot, rel), 0o755); err != nil {
			return fmt.Errorf("creating folder %s: %w", filepath.Join(p.DstRoot, rel), err)
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

	// The reconciling sweep (stage 17) runs alongside the copy and queues
	// what the plan missed.
	var lateQ *workQueue[LateJob]
	if p.Reconciler != nil {
		lateQ = newWorkQueue[LateJob]()
		go func() {
			defer lateQ.close()
			p.Reconciler.Sweep(ctx, func(j Job, c Change) { lateQ.push(LateJob{Job: j, Change: c}) })
		}()
	}
	inflight := newInflight()

	// The autotuner (stage 19): small-file workers take a slot from a
	// resizable limit before each file; the tuner moves the limit. The big
	// lanes are not tuned. Without it every worker always runs (v1).
	smallWorkers := workers - bigLanes
	var limit *Limit
	if t := opt.Tune; t != nil {
		smallWorkers = max(t.Max-bigLanes, 1)
		limit = NewLimit(workers - bigLanes)
		tctx, stopTuner := context.WithCancel(ctx)
		defer stopTuner()
		tuner := &Tuner{Min: max(t.Min, 1), Max: max(t.Max-bigLanes, 1), Start: workers - bigLanes, Window: t.Window}
		go tuner.Run(tctx, limit, func() (int64, int64) { return st.written.Load(), st.bytesDone.Load() }, opt.Trace.Note)
	}

	smallQ := make(chan *Item)
	bigQ := make(chan *Item)
	var wg sync.WaitGroup
	process := func(it *Item, buf []byte) {
		if it.late {
			copyLate(ctx, p, it, buf, opt, st)
		} else {
			copyWithRetries(ctx, p, it, buf, opt, st)
		}
		inflight.finish(it)
	}
	work := func(queues ...chan *Item) {
		defer wg.Done()
		bufp := bufPool.Get().(*[]byte)
		defer bufPool.Put(bufp)
		for _, q := range queues {
			tuned := limit != nil && q == smallQ
			for {
				if tuned && limit.Acquire(ctx) != nil {
					return // cancelled
				}
				it, ok := <-q
				if !ok {
					if tuned {
						limit.Release()
					}
					break
				}
				process(it, *bufp)
				if tuned {
					limit.Release()
				}
			}
		}
	}
	// Big-file workers help with the small files once the big ones are done.
	for w := 0; w < bigLanes; w++ {
		wg.Add(1)
		go work(bigQ, smallQ)
	}
	for w := 0; w < smallWorkers; w++ {
		wg.Add(1)
		go work(smallQ)
	}

	// Dispatch largest-first (the plan is already sorted), and stop handing out
	// work as soon as the user cancels. Big and small files have their own
	// dispatcher, so small files flow while the big lanes are busy.
	var dwg sync.WaitGroup
	dispatch := func(q chan *Item, mine func(*Item) bool, closeWhenDone bool) {
		defer dwg.Done()
		if closeWhenDone {
			defer close(q)
		}
		for i := range p.Items {
			it := &p.Items[i]
			if it.Decision.Skipped() || !mine(it) {
				continue
			}
			if it.Blocked != "" {
				st.fail(it.Rel, errors.New(it.Blocked))
				continue
			}
			inflight.start(it)
			select {
			case q <- it:
			case <-ctx.Done():
				inflight.finish(it)
				return
			}
		}
	}
	dwg.Add(2)
	go dispatch(smallQ, func(it *Item) bool { return !isBig(it) }, lateQ == nil)
	go dispatch(bigQ, isBig, true)
	dwg.Wait()
	if lateQ != nil {
		dispatchLate(ctx, p, opt, st, lateQ, smallQ, inflight)
		close(smallQ)
	}
	wg.Wait()
}

// inflight remembers, per destination file, the copy most recently handed
// to a worker, so a later copy of the same file (found by the sweep) waits
// for it instead of writing the same .ddxpart at the same time.
type inflightSet struct {
	mu   sync.Mutex
	last map[string]chan struct{}
}

func newInflight() *inflightSet { return &inflightSet{last: map[string]chan struct{}{}} }

// start registers it as the newest copy of its file; it.waitFor is the
// previous one (nil if none).
func (s *inflightSet) start(it *Item) {
	it.done = make(chan struct{})
	s.mu.Lock()
	it.waitFor = s.last[it.Rel]
	s.last[it.Rel] = it.done
	s.mu.Unlock()
}

// finish marks it as finished (copied, skipped, failed or cancelled).
func (s *inflightSet) finish(it *Item) {
	if it.done != nil {
		select {
		case <-it.done:
		default:
			close(it.done)
		}
	}
}

// dispatchLate hands the sweep's findings to the workers until the sweep ends.
func dispatchLate(ctx context.Context, p *Plan, opt Options, st *state, lateQ *workQueue[LateJob], q chan *Item, inflight *inflightSet) {
	var budget int64 = -1 // bytes late files may still use (-1 = unknown)
	budgetKnown := false
	for {
		lj, ok := lateQ.pop()
		if !ok || ctx.Err() != nil {
			return
		}
		if lj.Change == Vanished {
			continue // a planned file that is gone: its copy reports "vanished"
		}
		it := &Item{Job: lj.Job, late: true}
		st.total.Add(1)
		st.bytesTotal.Add(it.Size)
		st.found.Add(1)
		// Free space is re-checked for the added bytes: what is free now,
		// minus what the plan still has to write.
		if !budgetKnown {
			budgetKnown = true
			if c := opt.LateChecks; c.FreeBytes != nil {
				if free, err := c.FreeBytes(nearestExisting(p.DstRoot)); err == nil {
					budget = int64(free) - (st.bytesTotal.Load() - it.Size - st.bytesDone.Load())
				}
			}
		}
		if budget >= 0 {
			if it.Size > budget {
				st.fail(it.Rel, fmt.Errorf("%w: found while copying, but there is no room left for its %d bytes", ErrNotEnoughSpace, it.Size))
				continue
			}
			budget -= it.Size
		}
		inflight.start(it)
		select {
		case q <- it:
		case <-ctx.Done():
			inflight.finish(it)
			return
		}
	}
}

// copyLate copies a file the sweep found: once any earlier copy of the same
// file has finished, it goes through the same rules as the plan (decide, the
// FAT32 limit) and is copied.
func copyLate(ctx context.Context, p *Plan, it *Item, buf []byte, opt Options, st *state) {
	if it.waitFor != nil {
		select {
		case <-it.waitFor:
		case <-ctx.Done():
			return
		}
	}
	dst := filepath.Join(p.DstRoot, it.Rel)
	var info os.FileInfo
	if fi, err := os.Lstat(dst); err == nil {
		info = fi
	}
	it.Decision, it.Blocked = decide(p.OnConflict, it.Size, it.ModTime, info)
	switch {
	case it.Decision.Skipped():
		st.skipped.Add(1)
		st.filesDone.Add(1)
		return
	case it.Blocked != "":
		st.fail(it.Rel, errors.New(it.Blocked))
		return
	}
	if fsCheck := opt.LateChecks.FileSystem; fsCheck != nil && it.Size >= fat32MaxFile {
		if name, err := fsCheck(nearestExisting(p.DstRoot)); err == nil && isFAT32(name) {
			st.fail(it.Rel, fmt.Errorf("%w: %d bytes (FAT32 stores at most 4 GiB minus 1 byte)", ErrFileTooBigForFAT32, it.Size))
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		st.fail(it.Rel, fmt.Errorf("creating folder %s: %w", filepath.Dir(dst), err))
		return
	}
	copyWithRetries(ctx, p, it, buf, opt, st)
}

// errNotRegular marks a link or device that is skipped rather than copied.
var errNotRegular = errors.New("not a regular file")

func copyWithRetries(ctx context.Context, p *Plan, it *Item, buf []byte, opt Options, st *state) {
	if it.waitFor != nil {
		select {
		case <-it.waitFor:
		case <-ctx.Done():
			return
		}
	}
	dst := filepath.Join(p.DstRoot, it.Rel)
	var err error
	for attempt := 0; attempt <= opt.retries(); attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(opt.retryDelay()):
			case <-ctx.Done():
				return
			}
		}
		var n int64
		began := time.Now()
		n, err = copyFile(ctx, it.Src, dst, it.ModTime, buf, opt.Verify, st, opt.Trace, opt.Fast, opt.direct)
		if err == nil {
			if opt.journal != nil {
				opt.journal.add(it.Rel)
			}
			opt.Trace.FileDone(time.Since(began), n)
			st.written.Add(1)
			st.bytesDone.Add(n)
			st.filesDone.Add(1)
			return
		}
		if ctx.Err() != nil {
			return // cancelled: not a failure
		}
		if errors.Is(err, errNotRegular) {
			st.notRegular.Add(1)
			st.skipped.Add(1)
			st.filesDone.Add(1)
			return
		}
		if errors.Is(err, errSourceGone) && p.MayBeStale {
			st.vanish() // deleted since the index the plan came from was made
			return
		}
		// Any other failure (including a verify mismatch, whose bad copy has
		// already been deleted) is retried after a short pause.
	}
	st.fail(it.Rel, err)
}

// errSourceGone means the source file no longer exists.
var errSourceGone = errors.New("the source file no longer exists")

// errVerify marks a checksum mismatch.
var errVerify = errors.New("verification failed")

// copyFile copies one file atomically (see writeAtomic): write
// "<dst>.ddxpart", set the modification time, then rename it to the real name.
// fp turns on the stage 18 fast paths (the zero value is v1).
func copyFile(ctx context.Context, src, dst string, mtime int64, buf []byte, verify bool, st *state, tr *trace.Recorder, fp FastPaths, direct bool) (written int64, err error) {
	m := tr.Start()
	var in *os.File
	var size int64
	if fp.SingleOpen {
		// 18a: one open; size and time come from the handle.
		var realMtime int64
		in, size, realMtime, err = openSourceSingle(src)
		tr.End(trace.SrcOpen, m)
		switch {
		case errors.Is(err, errSourceGone):
			return 0, fmt.Errorf("%w: %s", errSourceGone, src)
		case errors.Is(err, errNotRegular):
			return 0, errNotRegular
		case err != nil:
			return 0, fmt.Errorf("opening %s: %w", src, err)
		}
		mtime = realMtime
	} else {
		info, err := os.Lstat(src)
		if err != nil {
			tr.End(trace.SrcOpen, m)
			if errors.Is(err, fs.ErrNotExist) {
				return 0, fmt.Errorf("%w: %s", errSourceGone, src)
			}
			return 0, fmt.Errorf("reading %s: %w", src, err)
		}
		if !info.Mode().IsRegular() {
			tr.End(trace.SrcOpen, m)
			return 0, errNotRegular // never follow links: that could copy files from elsewhere
		}
		in, err = openSource(src)
		tr.End(trace.SrcOpen, m)
		if err != nil {
			return 0, fmt.Errorf("opening %s: %w", src, err)
		}
		size = info.Size()
	}
	defer in.Close()

	// 18b: big files straight to and from the disk.
	mode := buffered
	var reader io.Reader = in
	if fp.Unbuffered && size >= unbufferedMin {
		mode = unbuffered
		if u, err := openUnbuffered(src); err == nil {
			defer u.Close()
			reader = &unbufferedSource{f: u}
		}
	}
	// 18c: the source's checksum is computed while it is copied.
	var srcSum hash.Hash32
	mediaVerify := verify && fp.MediaVerify
	if mediaVerify {
		srcSum = crc32.New(castagnoli)
		if u, ok := reader.(*unbufferedSource); ok {
			u.h = srcSum
		} else {
			reader = io.TeeReader(reader, srcSum)
		}
	}

	var n int64
	if direct && size < directMax && mode == buffered {
		n, err = writeDirect(ctx, dst, reader, mtime, buf, tr) // 18e: no .ddxpart, no rename
	} else {
		n, err = writeAtomic(ctx, dst, reader, size, mtime, buf, mode, tr)
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("copying %s: %w", src, err)
	}
	if afterCopy != nil {
		afterCopy(dst)
	}

	if verify {
		m := tr.Start()
		var a, b uint32
		var errA, errB error
		if mediaVerify {
			a = srcSum.Sum32()
			b, errB = checksumMedia(ctx, dst)
		} else {
			a, errA = checksum(ctx, src, buf)
			b, errB = checksum(ctx, dst, buf)
		}
		tr.End(trace.Verify, m)
		switch {
		case ctx.Err() != nil:
			return n, ctx.Err()
		case errA != nil || errB != nil:
			return n, fmt.Errorf("%w: couldn't re-read the files (%v %v)", errVerify, errA, errB)
		case a != b:
			// Remove the bad copy: it has the right size and time, so a re-run
			// would otherwise consider it "already there" and keep it.
			os.Remove(dst)
			st.mismatches.Add(1)
			return n, fmt.Errorf("%w: checksum of the copy differs from the original", errVerify)
		}
	}
	return n, nil
}

// checksumMedia computes the CRC-32C of a file read with
// FILE_FLAG_NO_BUFFERING, so the bytes come from the disk itself and not
// from Windows' RAM cache (which still holds what was just written).
// Elsewhere it is a normal read.
func checksumMedia(ctx context.Context, path string) (uint32, error) {
	h := crc32.New(castagnoli)
	if f, err := openUnbuffered(path); err == nil {
		defer f.Close()
		buf := alignedBuffer(1 << 20)
		for {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			n, err := f.Read(buf) // whole aligned requests; a short read is the end
			h.Write(buf[:n])
			if err == io.EOF || (err == nil && n < len(buf)) {
				return h.Sum32(), nil
			}
			if err != nil {
				return 0, err
			}
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := io.CopyBuffer(h, &ctxReader{ctx: ctx, r: f}, make([]byte, 1<<20)); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}

// checksum computes the CRC-32C of a file.
func checksum(ctx context.Context, path string, buf []byte) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := crc32.New(castagnoli)
	if _, err := io.CopyBuffer(h, &ctxReader{ctx: ctx, r: f}, buf); err != nil {
		return 0, err
	}
	return h.Sum32(), nil
}

// ctxReader makes a long read stop promptly when the context is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
