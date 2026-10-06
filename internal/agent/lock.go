package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// A lock file keeps two DriveDex programs (the agent and a `drivedex index`
// you typed yourself) from re-indexing the same drive at the same moment and
// overwriting each other's work.
//
// It is just an empty-ish file called "<id>.lock" next to the index. Creating
// it with O_EXCL succeeds for exactly one program at a time.

// StaleLockAge: a lock whose file hasn't been touched for this long is assumed
// to belong to a program that crashed, and is taken over.
const StaleLockAge = 10 * time.Minute

// lockRefresh is how often a running holder "touches" its lock so a long scan
// is never mistaken for a crash. A variable so tests can speed it up.
var lockRefresh = time.Minute

// ErrLocked means another program is already indexing that source.
var ErrLocked = errors.New("this drive is already being indexed by another DriveDex process")

// AcquireLock takes the lock at path, or returns ErrLocked. Call the returned
// function to release it (it also removes the file). Release is safe to call twice.
func AcquireLock(path string) (release func(), err error) {
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid %d\nstarted %s\n", os.Getpid(), time.Now().Format(time.RFC3339))
			f.Close()
			return holdLock(path), nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("creating lock %s: %w", path, err)
		}
		st, serr := os.Stat(path)
		if serr != nil {
			continue // the holder just released it: try again
		}
		if time.Since(st.ModTime()) > StaleLockAge {
			// Left behind by a crashed program: take over. (If two programs
			// race to do this, one of them loses and gets ErrLocked below.)
			os.Remove(path)
			continue
		}
		return nil, ErrLocked
	}
	return nil, ErrLocked
}

// holdLock starts the "I'm still alive" refresher and returns the release func.
func holdLock(path string) func() {
	stop := make(chan struct{})
	var once sync.Once
	interval := lockRefresh // read now, not inside the goroutine
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				os.Chtimes(path, now, now)
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(stop)
			os.Remove(path)
		})
	}
}

// IsLocked reports whether a fresh (non-stale) lock exists at path.
func IsLocked(path string) bool {
	st, err := os.Stat(path)
	return err == nil && time.Since(st.ModTime()) <= StaleLockAge
}
