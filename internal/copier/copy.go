package copier

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

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
	counts := p.Counts()
	st := &state{total: counts.Files, bytesTotal: counts.WriteBytes}

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

	smallQ := make(chan *Item)
	bigQ := make(chan *Item)
	var wg sync.WaitGroup
	work := func(queues ...chan *Item) {
		defer wg.Done()
		bufp := bufPool.Get().(*[]byte)
		defer bufPool.Put(bufp)
		for _, q := range queues {
			for it := range q {
				copyWithRetries(ctx, p.DstRoot, it, *bufp, opt, st)
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
	go dispatch(smallQ, func(it *Item) bool { return !isBig(it) })
	go dispatch(bigQ, isBig)
	dwg.Wait()
	wg.Wait()
}

// errNotRegular marks a link or device that is skipped rather than copied.
var errNotRegular = errors.New("not a regular file")

func copyWithRetries(ctx context.Context, dstRoot string, it *Item, buf []byte, opt Options, st *state) {
	dst := filepath.Join(dstRoot, it.Rel)
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
		n, err = copyFile(ctx, it.Src, dst, it.ModTime, buf, opt.Verify, st)
		if err == nil {
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
		// Any other failure (including a verify mismatch, whose bad copy has
		// already been deleted) is retried after a short pause.
	}
	st.fail(it.Rel, err)
}

// errVerify marks a checksum mismatch.
var errVerify = errors.New("verification failed")

// copyFile copies one file atomically: write "<dst>.ddxpart", set the modification
// time, then rename it to the real name.
//
// There is deliberately no fsync here: forcing every small file to the platter
// destroys USB throughput. (Use "Safely Remove" before unplugging a drive.)
func copyFile(ctx context.Context, src, dst string, mtime int64, buf []byte, verify bool, st *state) (written int64, err error) {
	info, err := os.Lstat(src)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return 0, errNotRegular // never follow links: that could copy files from elsewhere
	}
	in, err := openSource(src)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	part := dst + PartSuffix
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", part, err)
	}
	cleanup := func() { out.Close(); os.Remove(part) }

	// Reserve the whole file's space up front: one allocation instead of
	// growing the file a megabyte at a time. With several files written at
	// once this keeps each file in one piece on the destination instead of
	// interleaving them. Best effort: if it can't, the writes still work.
	if info.Size() >= preallocMin {
		preallocate(out, info.Size())
	}

	// struct{ io.Writer } hides *os.File's ReadFrom method; otherwise
	// io.CopyBuffer would ignore our big buffer and use a small default one.
	n, err := io.CopyBuffer(struct{ io.Writer }{out}, &ctxReader{ctx: ctx, r: in}, buf)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("copying %s: %w", src, err)
	}
	// Set the time through the handle we already have, before closing. Doing
	// it by name afterwards (os.Chtimes) opens the file a second time and
	// rewrites its folder entry a second time - on a pen drive each of those
	// is a slow little write, paid once per file.
	t := time.Unix(mtime, 0)
	timeSet, err := setModTime(out, t)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("setting the time of %s: %w", part, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(part)
		return 0, fmt.Errorf("writing %s: %w", part, err)
	}
	if !timeSet {
		if err := os.Chtimes(part, t, t); err != nil {
			os.Remove(part)
			return 0, fmt.Errorf("setting the time of %s: %w", part, err)
		}
	}
	if err := os.Rename(part, dst); err != nil {
		os.Remove(part)
		return 0, fmt.Errorf("moving %s into place: %w", part, err)
	}
	if afterCopy != nil {
		afterCopy(dst)
	}

	if verify {
		a, errA := checksum(ctx, src, buf)
		b, errB := checksum(ctx, dst, buf)
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
