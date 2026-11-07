package manifest

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrLocked means another DriveDex copy or sync is writing to this destination.
var ErrLocked = errors.New("another DriveDex copy or sync is writing to this destination right now")

// Journal flushing: every flushEvery records or flushInterval, and at the end.
const (
	flushEvery    = 500
	flushInterval = 2 * time.Second
)

// WrittenBy is recorded in the manifest header (set by the CLI to its version).
var WrittenBy = "drivedex"

// Store is an open manifest of one destination, locked for writing.
type Store struct {
	root string // the destination folder (dstRoot)
	dir  string // root\.drivedex

	lock *os.File
	m    *Manifest

	mu        sync.Mutex
	j         *os.File
	jw        *bufio.Writer
	pending   int
	lastFlush time.Time
	changed   bool
	err       error // the first write error (later records are dropped: safe, see package doc)

	recovered int
	torn      bool
}

// Dir is the manifest folder of a destination.
func Dir(dstRoot string) string { return filepath.Join(dstRoot, DirName) }

// Exists reports whether dstRoot has a manifest (compacted or a journal).
func Exists(dstRoot string) bool {
	for _, f := range []string{ManifestFile, JournalFile} {
		if _, err := os.Stat(filepath.Join(Dir(dstRoot), f)); err == nil {
			return true
		}
	}
	return false
}

// Open locks dstRoot's manifest for writing and loads it, creating the folder
// if needed. A journal left by an interrupted run is replayed up to its first
// damaged frame and compacted into manifest.ddm before anything else.
func Open(dstRoot string) (*Store, error) {
	s := &Store{root: dstRoot, dir: Dir(dstRoot)}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", s.dir, err)
	}
	hideFolder(s.dir)
	lock, err := lockFile(filepath.Join(s.dir, LockFile))
	if err != nil {
		return nil, err
	}
	s.lock = lock
	m, recovered, torn, err := load(s.dir)
	if err != nil {
		s.unlock()
		return nil, err
	}
	s.m, s.recovered, s.torn = m, recovered, torn
	if recovered > 0 || torn {
		// Fold the leftover journal in now, so this run starts a fresh one.
		if err := s.compact(); err != nil {
			s.unlock()
			return nil, err
		}
	}
	return s, nil
}

// Read loads dstRoot's manifest without locking or changing anything (for
// reports). A missing manifest is an empty one.
func Read(dstRoot string) (*Manifest, error) {
	m, _, _, err := load(Dir(dstRoot))
	return m, err
}

func load(dir string) (m *Manifest, recovered int, torn bool, err error) {
	m = New()
	f, err := os.Open(filepath.Join(dir, ManifestFile))
	switch {
	case err == nil:
		m, err = Decode(f)
		f.Close()
		if err != nil {
			return nil, 0, false, fmt.Errorf("%s: %w", filepath.Join(dir, ManifestFile), err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, 0, false, err
	}
	data, err := os.ReadFile(filepath.Join(dir, JournalFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, 0, false, err
	}
	if len(data) > 0 {
		recovered, torn = ReplayJournal(data, m)
	}
	return m, recovered, torn, nil
}

// Manifest is the in-memory manifest (records added this run included).
func (s *Store) Manifest() *Manifest { return s.m }

// Root is the destination folder.
func (s *Store) Root() string { return s.root }

// Recovered reports what the leftover journal of an interrupted run held:
// how many records were recovered, and whether it ended in a torn frame.
func (s *Store) Recovered() (int, bool) { return s.recovered, s.torn }

// Put records a file now in place. Safe for concurrent use.
func (s *Store) Put(r Record) error {
	r.Rel = Key(r.Rel)
	return s.append(putPayload(r), func() { s.m.Put(r) })
}

// Refresh updates a record in memory only; it is saved when the store is
// closed. For changes that are safe to lose in a crash, because the old
// record is still true (moving a journal bookmark forward). Not journaled,
// so a run that only refreshes appends nothing.
func (s *Store) Refresh(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m.Put(r)
	s.changed = true
}

// Delete forgets a destination path (removed, or moved to the trash).
func (s *Store) Delete(rel string) error {
	return s.append(deletePayload(rel), func() { s.m.Delete(rel) })
}

func (s *Store) append(payload []byte, apply func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply()
	s.changed = true
	if s.err != nil {
		return s.err
	}
	if s.j == nil {
		j, err := os.OpenFile(filepath.Join(s.dir, JournalFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			s.err = err
			return err
		}
		s.j, s.jw, s.lastFlush = j, bufio.NewWriterSize(j, 64<<10), time.Now()
	}
	if _, err := s.jw.Write(frame(payload)); err != nil {
		s.err = err
		return err
	}
	s.pending++
	if s.pending >= flushEvery || time.Since(s.lastFlush) >= flushInterval {
		return s.flushLocked()
	}
	return nil
}

// Flush pushes the journal to the drive (FlushFileBuffers).
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	if s.j == nil || s.err != nil {
		return s.err
	}
	if err := s.jw.Flush(); err != nil {
		s.err = err
		return err
	}
	if err := s.j.Sync(); err != nil {
		s.err = err
		return err
	}
	s.pending, s.lastFlush = 0, time.Now()
	return nil
}

// Close compacts the run's records into manifest.ddm (atomically), removes
// the journal and releases the lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.unlock()
	ferr := s.flushLocked()
	if s.j != nil {
		s.j.Close()
		s.j = nil
	}
	if !s.changed {
		return ferr
	}
	if ferr != nil {
		return ferr // keep the journal: the next run replays what made it to the drive
	}
	return s.compact()
}

// Abandon closes the files without compacting, as if the process had died
// (tests; the journal is replayed by the next Open).
func (s *Store) Abandon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.j != nil {
		s.jw.Flush()
		s.j.Close()
		s.j = nil
	}
	s.unlock()
}

// compact writes manifest.ddm atomically (temporary file, flush, rename) and
// then removes the journal.
func (s *Store) compact() error {
	var buf bytes.Buffer
	if err := Encode(&buf, s.m, WrittenBy); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.dir, ManifestFile), buf.Bytes()); err != nil {
		return fmt.Errorf("saving the copy manifest: %w", err)
	}
	if err := os.Remove(filepath.Join(s.dir, JournalFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.changed = false
	return nil
}

func (s *Store) unlock() {
	if s.lock != nil {
		s.lock.Close()
		s.lock = nil
	}
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
