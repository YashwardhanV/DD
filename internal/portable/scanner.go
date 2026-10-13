package portable

import (
	"context"
	"errors"
	"fmt"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/scan"
)

// Item is one file or folder on a device.
type Item struct {
	Name    string
	IsDir   bool
	Size    int64 // bytes; -1 = unknown
	ModTime int64 // unix seconds; 0 = unknown
}

// Entry is an Item as listed by a Folder. Sub is the opened sub-folder for a
// folder entry (nil for files); whoever receives it must Close it.
type Entry struct {
	Item
	Sub Folder
}

// Folder is one folder on a device - or the device itself, whose entries are
// its storages ("Internal storage", "SD card").
type Folder interface {
	List(ctx context.Context) ([]Entry, error)
	Close()
}

// ErrNoStorage means the device shows no storage at all. A phone does that
// while it is locked, or while it is set to "charging only".
var ErrNoStorage = errors.New("the device isn't showing any storage")

// maxFirstErrors is how many unreadable folders are remembered for the report.
const maxFirstErrors = 10

// Scanner indexes one portable device. It implements scan.Scanner.
//
// It walks the device one folder at a time, on the calling goroutine only:
// the Windows Shell objects behind Folder must stay on the thread that made them.
type Scanner struct {
	// Open opens the device's top level.
	Open  func(ctx context.Context) (Folder, error)
	stats scan.Stats
}

// Name implements scan.Scanner.
func (s *Scanner) Name() string { return "shell" }

// Stats returns the tally of the most recent Scan; Errors counts folders that couldn't be read.
func (s *Scanner) Stats() scan.Stats { return s.stats }

type pending struct {
	folder Folder
	id     uint64
	path   string // for error messages
}

// Scan implements scan.Scanner.
func (s *Scanner) Scan(ctx context.Context, b *index.Builder, report func(scan.Progress)) error {
	s.stats = scan.Stats{}
	root, err := s.Open(ctx)
	if err != nil {
		return err
	}
	defer root.Close()

	top, err := root.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the device's storage: %w", err)
	}
	storages := 0
	for _, e := range top {
		if e.Sub != nil {
			storages++
		}
	}
	if storages == 0 {
		closeAll(top)
		return ErrNoStorage
	}

	var nextID uint64
	var stack []pending
	defer func() { // on cancel or error, release every folder not yet visited
		for _, p := range stack {
			p.folder.Close()
		}
	}()
	var last int64
	progress := func() {
		if report != nil && b.Len()-last >= 256 {
			last = b.Len()
			report(scan.Progress{Entries: b.Len(), Dirs: s.stats.Dirs, Errors: s.stats.Errors})
		}
	}
	add := func(parent uint64, parentPath string, entries []Entry) {
		// Pushed in reverse so folders are visited in listing order.
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if e.Name == "" || e.Name == "." || e.Name == ".." {
				if e.Sub != nil {
					e.Sub.Close()
				}
				continue
			}
			nextID++
			if e.Sub != nil || e.IsDir {
				b.Add(index.Entry{ID: nextID, ParentID: parent, Name: e.Name, Flags: index.FlagDir})
				s.stats.Dirs++
				if e.Sub != nil {
					stack = append(stack, pending{folder: e.Sub, id: nextID, path: joinPath(parentPath, e.Name)})
				}
				continue
			}
			ent := index.Entry{ID: nextID, ParentID: parent, Name: e.Name, Size: e.Size, ModTime: e.ModTime}
			if e.Size < 0 {
				ent.Flags = index.FlagNoMeta
			}
			b.Add(ent)
			s.stats.Files++
		}
		progress()
	}

	add(0, "", top)
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := p.folder.List(ctx)
		p.folder.Close()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.stats.Errors++
			if len(s.stats.FirstErrors) < maxFirstErrors {
				s.stats.FirstErrors = append(s.stats.FirstErrors, fmt.Sprintf("%s: %v", p.path, err))
			}
			continue
		}
		add(p.id, p.path, entries)
	}
	if report != nil {
		report(scan.Progress{Entries: b.Len(), Dirs: s.stats.Dirs, Errors: s.stats.Errors})
	}
	return nil
}

func closeAll(entries []Entry) {
	for _, e := range entries {
		if e.Sub != nil {
			e.Sub.Close()
		}
	}
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + `\` + name
}
