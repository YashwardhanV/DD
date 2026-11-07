// Package trash is DriveDex's recoverable trash on a destination drive.
//
// Files that a sync removes, and old versions that a copy replaces, are never
// deleted: they are moved (a rename on the same drive: instant, no extra space
// at that moment) to
//
//	<dstRoot>\.drivedex\trash\<YYYYMMDD-HHMMSS>\<original relative path>
//
// and each run folder has a run.json saying what was moved and why. `drivedex
// trash restore` moves them back; only `drivedex trash empty` (or the
// trash_keep setting) deletes them for good (INV-3).
package trash

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yashwardhanv/dd/internal/manifest"
)

// Reasons a file went to the trash.
const (
	Removed  = "removed"  // sync: no longer in the source
	Replaced = "replaced" // an older version a copy or sync overwrote
)

// RunFile is the description of one run folder.
const RunFile = "run.json"

// runIDLayout names run folders (sortable, local time).
const runIDLayout = "20060102-150405"

// Entry is one file in a trash run.
type Entry struct {
	Rel     string    `json:"rel"`
	Why     string    `json:"why"`
	Size    int64     `json:"size"`
	MTimeNs int64     `json:"mtime_ns"`
	At      time.Time `json:"at"`
}

// RunInfo describes a run folder.
type RunInfo struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Kind    string    `json:"kind,omitempty"` // "copy" or "sync"
	Source  string    `json:"source,omitempty"`
	Entries []Entry   `json:"entries"`
}

// Files and Bytes total a run.
func (r RunInfo) Files() int { return len(r.Entries) }

// Bytes is the size of every file in the run.
func (r RunInfo) Bytes() int64 {
	var n int64
	for _, e := range r.Entries {
		n += e.Size
	}
	return n
}

// Dir is the trash folder of a destination.
func Dir(dstRoot string) string { return filepath.Join(manifest.Dir(dstRoot), manifest.TrashDir) }

// Run collects the files one copy or sync moves to the trash. The run folder
// is created by the first Move; a run that moves nothing leaves no trace.
type Run struct {
	root string
	info RunInfo
	dir  string

	mu      sync.Mutex
	created bool
	dirty   bool
}

// Begin starts a trash run for dstRoot (kind "copy" or "sync").
func Begin(dstRoot, kind, source string, now time.Time) *Run {
	id := now.Format(runIDLayout)
	return &Run{root: dstRoot, info: RunInfo{ID: id, Created: now, Kind: kind, Source: source}}
}

// ID is the run's folder name.
func (r *Run) ID() string { return r.info.ID }

// Move moves dstRoot\rel into the trash. Safe for concurrent use.
func (r *Run) Move(rel, why string) error {
	if r == nil {
		return errors.New("no trash for this destination")
	}
	src := filepath.Join(r.root, rel)
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if !r.created {
		if err := r.createDir(); err != nil {
			r.mu.Unlock()
			return err
		}
	}
	dir := r.dir
	r.mu.Unlock()

	to := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(to); err == nil {
		// The same path twice in one run (replaced, then replaced again):
		// keep both.
		to = uniqueName(to)
	}
	if err := os.Rename(src, to); err != nil {
		return fmt.Errorf("moving %s to the trash: %w", src, err)
	}
	r.mu.Lock()
	r.info.Entries = append(r.info.Entries, Entry{Rel: rel, Why: why, Size: st.Size(), MTimeNs: st.ModTime().UnixNano(), At: time.Now()})
	r.dirty = true
	r.mu.Unlock()
	return nil
}

// createDir makes the run folder, choosing a new name if a run of the same
// second exists.
func (r *Run) createDir() error {
	base := Dir(r.root)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	id := r.info.ID
	for i := 2; ; i++ {
		dir := filepath.Join(base, id)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			r.dir, r.info.ID, r.created = dir, id, true
			return nil
		}
		if !os.IsExist(err) {
			return err
		}
		id = fmt.Sprintf("%s-%d", r.info.ID, i)
	}
}

func uniqueName(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Lstat(c); err != nil {
			return c
		}
	}
}

// Moved is how many files the run moved so far.
func (r *Run) Moved() (files int, bytes int64) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.info.Files(), r.info.Bytes()
}

// Close writes run.json (if anything was moved).
func (r *Run) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty {
		return nil
	}
	b, err := json.MarshalIndent(r.info, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(r.dir, RunFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	r.dirty = false
	return os.Rename(tmp, filepath.Join(r.dir, RunFile))
}

// List returns the trash runs of dstRoot, oldest first. A run folder without
// run.json (the process died before writing it) is still listed, from its
// files, with the reason unknown.
func List(dstRoot string) ([]RunInfo, error) {
	ents, err := os.ReadDir(Dir(dstRoot))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []RunInfo
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		info, err := readRun(dstRoot, e.Name())
		if err != nil {
			continue
		}
		runs = append(runs, info)
	}
	sort.Slice(runs, func(a, b int) bool { return runs[a].ID < runs[b].ID })
	return runs, nil
}

func readRun(dstRoot, id string) (RunInfo, error) {
	dir := filepath.Join(Dir(dstRoot), id)
	info := RunInfo{ID: id}
	if b, err := os.ReadFile(filepath.Join(dir, RunFile)); err == nil && json.Unmarshal(b, &info) == nil {
		info.ID = id
		// Keep only entries still in the folder (some may have been restored).
		kept := info.Entries[:0]
		for _, e := range info.Entries {
			if _, err := os.Lstat(filepath.Join(dir, e.Rel)); err == nil {
				kept = append(kept, e)
			}
		}
		info.Entries = kept
	}
	// Files not described by run.json (a run that crashed before writing it).
	known := map[string]bool{}
	for _, e := range info.Entries {
		known[strings.ToLower(manifest.Key(e.Rel))] = true
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == RunFile || rel == RunFile+".tmp" || known[strings.ToLower(manifest.Key(rel))] {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		info.Entries = append(info.Entries, Entry{Rel: rel, Why: "unknown", Size: st.Size(), MTimeNs: st.ModTime().UnixNano(), At: st.ModTime()})
		return nil
	})
	if info.Created.IsZero() {
		if t, perr := time.ParseInLocation(runIDLayout, id[:min(len(id), len(runIDLayout))], time.Local); perr == nil {
			info.Created = t
		} else if st, serr := os.Stat(dir); serr == nil {
			info.Created = st.ModTime()
		}
	}
	return info, err
}

// RestoreResult says what Restore did.
type RestoreResult struct {
	Restored []string // relative paths moved back
	Existing []string // left in the trash because something is already at that path
}

// Restore moves the files of run id (all, or only those at or below path)
// back to where they were. A file whose original place is taken is left in
// the trash and listed in Existing, unless overwrite says yes for it — then
// the file in the way goes to a new trash run first (nothing is lost).
func Restore(dstRoot, id, path string, overwrite func(rel string) bool, now time.Time) (RestoreResult, error) {
	var res RestoreResult
	info, err := readRun(dstRoot, id)
	if err != nil {
		return res, err
	}
	if _, err := os.Stat(filepath.Join(Dir(dstRoot), id)); err != nil {
		return res, fmt.Errorf("there is no trash run %q in %s", id, dstRoot)
	}
	prefix := strings.ToLower(manifest.Key(path))
	var aside *Run
	dir := filepath.Join(Dir(dstRoot), id)
	for _, e := range info.Entries {
		k := strings.ToLower(manifest.Key(e.Rel))
		if prefix != "" && k != prefix && !strings.HasPrefix(k, prefix+`\`) {
			continue
		}
		from := filepath.Join(dir, e.Rel)
		to := filepath.Join(dstRoot, e.Rel)
		if _, err := os.Lstat(to); err == nil {
			if overwrite == nil || !overwrite(e.Rel) {
				res.Existing = append(res.Existing, e.Rel)
				continue
			}
			if aside == nil {
				aside = Begin(dstRoot, "restore", "", now)
			}
			if err := aside.Move(e.Rel, Replaced); err != nil {
				return res, err
			}
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return res, err
		}
		if err := os.Rename(from, to); err != nil {
			return res, fmt.Errorf("restoring %s: %w", e.Rel, err)
		}
		res.Restored = append(res.Restored, e.Rel)
	}
	if aside != nil {
		aside.Close()
	}
	pruneEmpty(dir)
	return res, nil
}

// Empty permanently deletes the runs created before now-olderThan (all runs
// when olderThan is 0) and returns them.
func Empty(dstRoot string, olderThan time.Duration, now time.Time) ([]RunInfo, error) {
	runs, err := List(dstRoot)
	if err != nil {
		return nil, err
	}
	var gone []RunInfo
	for _, r := range runs {
		if olderThan > 0 && now.Sub(r.Created) < olderThan {
			continue
		}
		if err := os.RemoveAll(filepath.Join(Dir(dstRoot), r.ID)); err != nil {
			return gone, err
		}
		gone = append(gone, r)
	}
	if left, _ := os.ReadDir(Dir(dstRoot)); len(left) == 0 {
		os.Remove(Dir(dstRoot))
	}
	return gone, nil
}

// pruneEmpty removes dir's empty sub-folders, and dir itself if it ends up
// holding nothing but run.json.
func pruneEmpty(dir string) {
	var dirs []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != dir {
			dirs = append(dirs, p)
		}
		return nil
	})
	sort.Slice(dirs, func(a, b int) bool { return len(dirs[a]) > len(dirs[b]) })
	for _, d := range dirs {
		os.Remove(d) // fails (harmlessly) unless empty
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) == 0 || (len(ents) == 1 && ents[0].Name() == RunFile) {
		os.RemoveAll(dir)
	}
}

// ParseAge understands "30d", "2w", "12h", "90m" (a plain Go duration also works).
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := map[byte]float64{'d': 24, 'w': 24 * 7}
	if m, ok := mult[s[len(s)-1]]; ok {
		var n float64
		if _, err := fmt.Sscanf(s[:len(s)-1], "%g", &n); err == nil && n >= 0 {
			if d := n * m * float64(time.Hour); d < math.MaxInt64 {
				return time.Duration(d), nil
			}
			return time.Duration(math.MaxInt64), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q isn't a time like \"30d\", \"2w\" or \"12h\"", s)
	}
	return d, nil
}
