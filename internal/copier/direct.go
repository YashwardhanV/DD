package copier

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yashwardhanv/dd/internal/copier/trace"
)

// Direct writes (BUILD-V2 18e, EXPERIMENTAL, off by default)
//
// Every v1 file is written as "<name>.ddxpart" and renamed when complete, so
// a real name never holds half a file. The rename is one more metadata write
// per file. When the destination folder is brand new (created by this very
// copy) there is nothing of the owner's to protect there, so small files can
// be written straight to their real names - IF something else remembers which
// files are complete. That is the resume journal: "<dst>\.drivedex-resume",
// one line per finished file, appended as files finish (flushed to the disk
// every 1,000 lines). On a re-run every file NOT in the journal is copied
// again, whatever its size and date say - so a file cut off by a crash or an
// unplug is never mistaken for a finished one. When a copy finishes with no
// failures, the journal is deleted.
//
// The journal is honoured whenever it exists, even if the re-run doesn't ask
// for direct writes, so turning the experiment off can never make a resume
// unsafe.

// ResumeJournalName is the journal's file name inside the destination folder.
const ResumeJournalName = ".drivedex-resume"

// directMax: only files smaller than this are written directly.
const directMax = 1 << 20

// journalSyncEvery is how many lines go between flushes to the disk.
const journalSyncEvery = 1000

// resumeJournal is the list of finished files of a direct-write copy.
type resumeJournal struct {
	path string
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	n    int
	done map[string]bool // from an earlier run
}

// openJournal opens (creating if needed) the journal and reads the files an
// earlier run finished.
func openJournal(dstRoot string) (*resumeJournal, error) {
	p := filepath.Join(dstRoot, ResumeJournalName)
	j := &resumeJournal{path: p, done: map[string]bool{}}
	if data, err := os.ReadFile(p); err == nil {
		lines := strings.Split(string(data), "\n")
		// The last line may be cut off by a crash: only complete lines count.
		for _, l := range lines[:len(lines)-1] {
			if l = strings.TrimRight(l, "\r"); l != "" {
				j.done[l] = true
			}
		}
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening the resume journal %s: %w", p, err)
	}
	j.f, j.w = f, bufio.NewWriter(f)
	return j, nil
}

// has reports whether an earlier run finished rel.
func (j *resumeJournal) has(rel string) bool { return j.done[filepath.ToSlash(rel)] }

// add records rel as finished.
func (j *resumeJournal) add(rel string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.w.WriteString(filepath.ToSlash(rel) + "\n")
	j.n++
	if j.n%journalSyncEvery == 0 {
		j.w.Flush()
		j.f.Sync()
	}
}

// finish closes the journal and deletes it when the copy is complete.
func (j *resumeJournal) finish(complete bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.w.Flush()
	j.f.Sync()
	j.f.Close()
	if complete {
		os.Remove(j.path)
	}
}

// setupJournal decides, before anything is written, whether this local copy
// keeps a resume journal: when direct writes are on and the destination
// folder doesn't exist yet, or when an earlier run left a journal. With an
// earlier journal, every file it doesn't list is copied again.
func setupJournal(p *Plan, opt Options) (j *resumeJournal, direct bool, err error) {
	if p.Phone != nil {
		return nil, false, nil
	}
	_, statErr := os.Stat(p.DstRoot)
	newDst := os.IsNotExist(statErr)
	_, jErr := os.Stat(filepath.Join(p.DstRoot, ResumeJournalName))
	earlier := jErr == nil
	if !earlier && !(opt.Fast.Direct && newDst) {
		return nil, false, nil
	}
	if err := os.MkdirAll(p.DstRoot, 0o755); err != nil {
		return nil, false, fmt.Errorf("creating %s: %w", p.DstRoot, err)
	}
	j, err = openJournal(p.DstRoot)
	if err != nil {
		return nil, false, err
	}
	if earlier {
		for i := range p.Items {
			it := &p.Items[i]
			if it.Decision.Skipped() && !j.has(it.Rel) {
				it.Decision = Overwrite // there, but not known to be complete
			}
		}
	}
	return j, opt.Fast.Direct && newDst, nil
}

// writeDirect writes a small file straight to its real name (18e). On any
// error the file is removed again.
func writeDirect(ctx context.Context, dst string, r io.Reader, mtime int64, buf []byte, tr *trace.Recorder) (int64, error) {
	m := tr.Start()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	tr.End(trace.DstCreate, m)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", dst, err)
	}
	m = tr.Start()
	n, err := copyData(ctx, out, r, buf, false)
	tr.End(trace.Data, m)
	if err != nil {
		out.Close()
		os.Remove(dst)
		return 0, err
	}
	t := time.Unix(mtime, 0)
	m = tr.Start()
	timeSet, err := setModTime(out, t)
	tr.End(trace.SetTime, m)
	if err != nil {
		out.Close()
		os.Remove(dst)
		return 0, fmt.Errorf("setting the time of %s: %w", dst, err)
	}
	m = tr.Start()
	err = out.Close()
	tr.End(trace.Close, m)
	if err != nil {
		os.Remove(dst)
		return 0, fmt.Errorf("writing %s: %w", dst, err)
	}
	if !timeSet {
		if err := os.Chtimes(dst, t, t); err != nil {
			os.Remove(dst)
			return 0, fmt.Errorf("setting the time of %s: %w", dst, err)
		}
	}
	return n, nil
}
