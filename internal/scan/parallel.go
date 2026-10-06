package scan

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/yashwardhanv/dd/internal/index"
)

// batchSize is how many entries a worker collects before handing them to the
// collector in one go (sending one at a time would drown in channel overhead).
const batchSize = 256

// readDir lists a directory. It is a variable so tests can make chosen
// folders fail. It uses File.ReadDir (unsorted) rather than os.ReadDir
// because we don't care about order and sorting every folder is wasted work.
var readDir = func(path string) ([]fs.DirEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1) // on error this still returns the entries read so far
}

// Parallel scans a folder tree with many goroutines at once.
//
// How it works (this is the classic "worker pool"):
//
//	                     ┌──────────── work queue of folders ◀──────────┐
//	                     │ (unbounded: workers push sub-folders back)    │
//	                     ▼                                               │
//	worker ... worker ──▶ read one folder ──▶ new sub-folders ───────────┘
//	      │
//	      └──▶ batches of entries ──▶ ONE collector goroutine ──▶ index.Builder
//
// The builder is not thread-safe, so exactly one goroutine (the collector)
// ever touches it.
type Parallel struct {
	Root     string
	SkipDirs []string // nil = DefaultSkipDirs
	Workers  int      // number of goroutines reading folders; <= 0 means 4

	stats Stats
}

// NewParallel returns a Parallel scanner for root using the given number of workers.
func NewParallel(root string, workers int) *Parallel {
	return &Parallel{Root: root, Workers: workers}
}

// Name implements Scanner.
func (p *Parallel) Name() string { return "parallel" }

// Stats returns the tally of the most recent Scan.
func (p *Parallel) Stats() Stats { return p.stats }

// dirJob is one folder waiting to be read.
type dirJob struct {
	abs string // full path on disk
	rel string // path relative to the scan root ("" for the root)
	id  uint64 // the folder's entry ID (0 for the root)
}

// workQueue is an UNBOUNDED stack of folders plus a count of folders that are
// queued or being processed ("pending").
//
// It is deliberately not a bounded channel: a worker that is busy pushing the
// sub-folders it found would block when the channel is full, and if every
// worker is blocked pushing, nobody is left to pop - a deadlock. With an
// unbounded slice, push never blocks.
//
// The scan is finished exactly when pending reaches 0: nothing is queued and
// nothing is being worked on, so nothing can ever be queued again.
type workQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	jobs    []dirJob
	pending int
}

func newWorkQueue() *workQueue {
	q := &workQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *workQueue) push(j dirJob) {
	q.mu.Lock()
	q.pending++
	q.jobs = append(q.jobs, j)
	q.mu.Unlock()
	q.cond.Signal()
}

// pop returns the next folder, waiting while the queue is empty but other
// workers may still add more. It returns false when the whole scan is done.
// (It takes from the end: roughly depth-first, which keeps the queue short and
// reads nearby folders together.)
func (q *workQueue) pop() (dirJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.jobs) == 0 && q.pending > 0 {
		q.cond.Wait()
	}
	if len(q.jobs) == 0 { // pending == 0: finished
		return dirJob{}, false
	}
	j := q.jobs[len(q.jobs)-1]
	q.jobs = q.jobs[:len(q.jobs)-1]
	return j, true
}

// done marks one folder as fully processed (its sub-folders were already pushed).
func (q *workQueue) done() {
	q.mu.Lock()
	q.pending--
	finished := q.pending == 0
	q.mu.Unlock()
	if finished {
		q.cond.Broadcast() // wake idle workers so they can exit
	}
}

// Scan implements Scanner.
func (p *Parallel) Scan(ctx context.Context, b *index.Builder, report func(Progress)) error {
	p.stats = Stats{}
	root := filepath.Clean(p.Root)

	st, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("opening %s: %w", root, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a folder", root)
	}

	workers := p.Workers
	if workers <= 0 {
		workers = 4
	}
	skipNames := p.SkipDirs
	if skipNames == nil {
		skipNames = DefaultSkipDirs
	}
	skip := newSkipSet(skipNames)

	var (
		nextID   atomic.Uint64
		errMu    sync.Mutex // guards p.stats.Errors / FirstErrors and rootErr
		rootErr  error
		queue    = newWorkQueue()
		out      = make(chan []index.Entry, 64)
		workerWG sync.WaitGroup
	)
	noteError := func(path string, err error) {
		errMu.Lock()
		p.stats.Errors++
		if len(p.stats.FirstErrors) < maxRememberedErrors {
			p.stats.FirstErrors = append(p.stats.FirstErrors, fmt.Sprintf("%s: %v", path, err))
		}
		errMu.Unlock()
	}

	// processDir reads one folder, queues its sub-folders and sends its entries on.
	processDir := func(job dirJob) {
		entries, err := readDir(job.abs)
		if err != nil {
			if job.rel == "" {
				errMu.Lock()
				rootErr = fmt.Errorf("reading %s: %w", job.abs, err)
				errMu.Unlock()
				return
			}
			noteError(job.abs, err)
			// fall through: entries read before the error are still useful
		}

		batch := make([]index.Entry, 0, min(len(entries), batchSize))
		for _, d := range entries {
			name := d.Name()
			isDir := d.IsDir() // false for symlinks and junctions: we never follow them
			if isDir && skip.has(name) {
				continue
			}
			e := index.Entry{ID: nextID.Add(1), ParentID: job.id, Name: name, Size: -1}
			if info, ierr := d.Info(); ierr != nil {
				noteError(filepath.Join(job.abs, name), ierr)
				e.Flags |= index.FlagNoMeta
			} else {
				e.Size = info.Size()
				e.ModTime = info.ModTime().Unix()
				e.Flags |= attrFlags(info, name)
			}
			if isDir {
				e.Flags |= index.FlagDir
				e.Size = 0
				rel := name
				if job.rel != "" {
					rel = job.rel + string(filepath.Separator) + name
				}
				queue.push(dirJob{abs: filepath.Join(job.abs, name), rel: rel, id: e.ID})
			}
			batch = append(batch, e)
			if len(batch) == batchSize {
				out <- batch
				batch = make([]index.Entry, 0, batchSize)
			}
		}
		if len(batch) > 0 {
			out <- batch
		}
	}

	worker := func() {
		defer workerWG.Done()
		for {
			job, ok := queue.pop()
			if !ok {
				return
			}
			// After Ctrl+C we stop reading folders but still mark each one done,
			// so the queue drains and every worker exits promptly.
			if ctx.Err() == nil {
				processDir(job)
			}
			queue.done()
		}
	}

	queue.push(dirJob{abs: root})
	for i := 0; i < workers; i++ {
		workerWG.Add(1)
		go worker()
	}
	// When every worker has exited nothing more can arrive: close the channel
	// so the collector loop below ends.
	go func() {
		workerWG.Wait()
		close(out)
	}()

	// The collector: the only goroutine that touches the builder.
	th := newThrottle(report)
	var files, dirs int64
	var current string
	progress := func() Progress {
		errMu.Lock()
		errs := p.stats.Errors
		errMu.Unlock()
		return Progress{Entries: files + dirs, Dirs: dirs, Errors: errs, Current: current}
	}
	for batch := range out {
		for _, e := range batch {
			b.Add(e)
			if e.Flags&index.FlagDir != 0 {
				dirs++
			} else {
				files++
			}
		}
		current = batch[len(batch)-1].Name
		th.maybe(progress())
	}

	p.stats.Files, p.stats.Dirs = files, dirs
	th.final(progress())

	errMu.Lock()
	defer errMu.Unlock()
	if rootErr != nil {
		return rootErr
	}
	return ctx.Err() // nil unless the scan was cancelled
}
