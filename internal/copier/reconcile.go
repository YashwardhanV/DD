package copier

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yashwardhanv/dd/internal/scan"
)

// The reconciling sweep (BUILD-V2 stage 17)
//
// A plan made from a saved index starts copying at once - but the index can be
// a few minutes old. While the copy runs, a quick directory sweep lists the
// source again (two workers, os.ReadDir: on Windows the listing itself carries
// every file's size and time, so no file is opened) and compares it with the
// plan:
//
//	in the source, not in the plan   -> Added:    copied too ("+N found while copying")
//	size or time differ from the plan -> Modified: copied (again) with its real content
//	in the plan, not in the source    -> Vanished: nothing to do (the copy reports it)
//
// So every file present when the sweep ends is either copied or reported.

// Change is what the sweep found about one file.
type Change uint8

const (
	Added    Change = iota // in the source, not in the plan
	Modified               // size or modification time differ from the plan
	Vanished               // in the plan, no longer in the source
)

// LateJob is a file the sweep found after the copy started.
type LateJob struct {
	Job
	Change Change
}

// Reconciler finds what a plan from an index missed. Sweep calls emit for
// every difference (from its own goroutine; the copier makes that safe while
// workers run) and returns when the whole source was listed.
type Reconciler interface {
	Sweep(ctx context.Context, emit func(j Job, c Change)) error
}

// SweepResult is what a finished sweep saw, so the caller can refresh the
// drive's index with it (the next copy then plans from a fresh index).
type SweepResult struct {
	Complete                  bool // the whole source was listed (not cancelled, no error)
	Added, Modified, Vanished int
	// Files is every file seen: Rel relative to the folder being copied
	// (OS separators), with size and time.
	Files []Job
}

// DirSweeper is the Reconciler for a local folder.
type DirSweeper struct {
	Src     string         // the folder being copied
	Name    string         // its name inside the destination (Job.Rel starts with it)
	Known   map[string]Job // the plan's files, by Rel
	Workers int            // default 2

	mu     sync.Mutex
	result SweepResult
}

// Result returns what the last Sweep saw.
func (s *DirSweeper) Result() SweepResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// skipDir: the same folders v1's listing skips.
func skipDir(name string) bool {
	for _, d := range scan.DefaultSkipDirs {
		if strings.EqualFold(d, name) {
			return true
		}
	}
	return false
}

// Sweep implements Reconciler.
func (s *DirSweeper) Sweep(ctx context.Context, emit func(j Job, c Change)) error {
	workers := s.Workers
	if workers <= 0 {
		workers = 2
	}
	var (
		mu    sync.Mutex
		seen  = make(map[string]bool, len(s.Known))
		files []Job
		res   SweepResult
		first error
	)
	dirs := newWorkQueue[string]()
	var pending sync.WaitGroup
	pending.Add(1)
	dirs.push("")
	go func() { pending.Wait(); dirs.close() }()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				rel, ok := dirs.pop()
				if !ok {
					return
				}
				func() {
					defer pending.Done()
					if ctx.Err() != nil {
						return
					}
					entries, err := os.ReadDir(filepath.Join(s.Src, rel))
					if err != nil {
						mu.Lock()
						if first == nil {
							first = err
						}
						mu.Unlock()
						return
					}
					for _, e := range entries {
						childRel := filepath.Join(rel, e.Name())
						if e.IsDir() {
							if !skipDir(e.Name()) {
								pending.Add(1)
								dirs.push(childRel)
							}
							continue
						}
						if !e.Type().IsRegular() {
							continue // links are never copied
						}
						info, err := e.Info() // from the listing on Windows: no file is opened
						if err != nil {
							continue // gone between listing and asking
						}
						j := Job{
							Src: filepath.Join(s.Src, childRel), Rel: filepath.Join(s.Name, childRel),
							Size: info.Size(), ModTime: info.ModTime().Unix(),
						}
						known, wasPlanned := s.Known[j.Rel]
						mu.Lock()
						seen[j.Rel] = true
						files = append(files, j)
						switch {
						case !wasPlanned:
							res.Added++
						case known.Size != j.Size || known.ModTime != j.ModTime:
							res.Modified++
						}
						mu.Unlock()
						switch {
						case !wasPlanned:
							emit(j, Added)
						case known.Size != j.Size || known.ModTime != j.ModTime:
							emit(j, Modified)
						}
					}
				}()
			}
		}()
	}
	wg.Wait()

	complete := ctx.Err() == nil && first == nil
	if complete {
		for rel, j := range s.Known {
			if !seen[rel] {
				res.Vanished++
				emit(j, Vanished)
			}
		}
	}
	res.Complete = complete
	res.Files = files
	s.mu.Lock()
	s.result = res
	s.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return first
}
