package scan

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yashwardhanv/dd/internal/index"
)

// Walk is the simplest scanner: one goroutine visits every folder in turn
// ("walk every shelf"). It is the baseline every faster scanner is measured
// against, and the reference the parallel scanner is tested against.
type Walk struct {
	// Root is the folder or drive root to scan.
	Root string
	// SkipDirs lists folder names to skip (case-insensitive, any depth).
	// nil means DefaultSkipDirs.
	SkipDirs []string
	// Visit, if set, is called for every entry that is kept, with its path
	// relative to Root. It lets `drivedex scan` print files as they are found.
	Visit func(rel string, e index.Entry)

	stats Stats
}

// NewWalk returns a Walk scanner for root.
func NewWalk(root string) *Walk { return &Walk{Root: root} }

// Name implements Scanner.
func (w *Walk) Name() string { return "walk" }

// Stats returns the tally of the most recent Scan.
func (w *Walk) Stats() Stats { return w.stats }

// Scan implements Scanner.
func (w *Walk) Scan(ctx context.Context, b *index.Builder, report func(Progress)) error {
	w.stats = Stats{}
	root := filepath.Clean(w.Root)

	// A problem with the root itself is fatal; everything below it is not.
	st, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("opening %s: %w", root, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a folder", root)
	}

	skipNames := w.SkipDirs
	if skipNames == nil {
		skipNames = DefaultSkipDirs
	}
	skip := newSkipSet(skipNames)

	// WalkDir builds child paths as root + separator + name, so stripping this
	// prefix gives each entry's path relative to the root.
	sep := string(filepath.Separator)
	prefix := root
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}

	// dirIDs maps a folder's relative path to its ID. The root is "" -> 0.
	// WalkDir always visits a folder before its children, so by the time we
	// meet a file its parent folder is already in the map.
	dirIDs := map[string]uint64{"": 0}
	var nextID uint64
	th := newThrottle(report)
	var current string
	progress := func() Progress {
		return Progress{Entries: w.stats.Entries(), Dirs: w.stats.Dirs, Errors: w.stats.Errors, Current: current}
	}

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			w.noteError(path, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir // can't read inside this folder; carry on with the rest
			}
			return nil
		}
		if path == root {
			return nil // the root itself is never an entry
		}

		name := d.Name()
		isDir := d.IsDir()
		if isDir && skip.has(name) {
			return filepath.SkipDir
		}

		// Stop promptly on Ctrl+C: check at every folder and every 1,000 entries.
		if isDir || w.stats.Entries()%1000 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}

		rel := strings.TrimPrefix(path, prefix)
		parentRel := ""
		if i := strings.LastIndexByte(rel, filepath.Separator); i >= 0 {
			parentRel = rel[:i]
		}

		e := index.Entry{Name: name, ParentID: dirIDs[parentRel], Size: -1}
		nextID++
		e.ID = nextID

		if info, ierr := d.Info(); ierr != nil {
			w.noteError(path, ierr)
			e.Flags |= index.FlagNoMeta
		} else {
			e.Size = info.Size()
			e.ModTime = info.ModTime().Unix()
			e.Flags |= attrFlags(info, name)
		}
		if isDir {
			e.Flags |= index.FlagDir
			e.Size = 0
			dirIDs[rel] = e.ID
			w.stats.Dirs++
		} else {
			w.stats.Files++
		}

		b.Add(e)
		if w.Visit != nil {
			w.Visit(rel, e)
		}
		current = path
		if w.stats.Entries()&63 == 0 { // checking the clock every entry would be wasteful
			th.maybe(progress())
		}
		return nil
	})
	th.final(progress())
	if err != nil {
		return err
	}
	return nil
}

// noteError counts an error and remembers the first few for display.
func (w *Walk) noteError(path string, err error) {
	w.stats.Errors++
	if len(w.stats.FirstErrors) < maxRememberedErrors {
		w.stats.FirstErrors = append(w.stats.FirstErrors, fmt.Sprintf("%s: %v", path, err))
	}
}
