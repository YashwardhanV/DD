package copier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/manifest"
)

// Mass-delete guard: a sync that would remove more than this share of the
// mirrored files, or more than massDeleteFiles of them, needs
// --allow-mass-delete. It protects against the wrong card in the reader.
const (
	massDeleteShare = 0.5
	massDeleteFiles = 1000
)

// ErrEmptySource: a sync from a source that lists no files is refused (it
// would make the mirror empty; an unplugged or wrong card looks like that).
var ErrEmptySource = errors.New("the source has no files")

// Removal is a mirrored file that is no longer in the source.
type Removal struct {
	Rel  string // relative to DstRoot
	Size int64
}

// SyncPlan is a copy plan plus what keeping a mirror needs.
type SyncPlan struct {
	*Plan
	// Top is the mirror folder relative to DstRoot (the source folder's name).
	Top string
	// Adopt: the mirror folder exists but the manifest has no record of
	// DriveDex placing anything in it. This first sync only copies and
	// records; it deletes nothing.
	Adopt bool
	// Moves are the items renamed on the destination instead of copied.
	Moves []*Item
	// Removals are recorded files of this source that the source no longer has.
	Removals []Removal
	// Mirrored is how many files of this source the manifest recorded under
	// Top before this run.
	Mirrored int
	// KeptUnlisted counts recorded files not removed because their source
	// folder couldn't be listed completely.
	KeptUnlisted int
	// MassDelete: the removals exceed the guard (see massDeleteShare).
	MassDelete bool
	// Gone are records whose file is no longer on the destination (someone
	// deleted it there): nothing to remove, the record is dropped.
	Gone []string
}

// SyncOptions are the inputs of PlanSync.
type SyncOptions struct {
	Manifest    *manifest.Manifest // may be empty, never nil
	SrcID       string
	DetectMoves bool
	// ByHash (--compare checksum): a move pair must also match by content.
	ByHash bool
}

// PlanSync turns a copy plan (made with OnConflictOverwrite) into a mirror
// plan: which recorded files to remove and which to move.
//
// Ownership rule: only files the manifest records as placed by DriveDex for
// THIS source are ever removed or moved. A file someone else put in the
// mirror folder is left alone.
func PlanSync(ctx context.Context, p *Plan, o SyncOptions) (*SyncPlan, error) {
	sp := &SyncPlan{Plan: p, Top: p.Top}
	if p.Top == "" {
		return nil, errors.New("sync needs a folder as its source, not a single file")
	}
	if len(p.Items) == 0 {
		return nil, ErrEmptySource
	}
	m := o.Manifest
	recs := m.Under(p.Top)
	mine := recs[:0]
	for _, r := range recs {
		if r.SrcID == o.SrcID {
			mine = append(mine, r)
		}
	}
	sp.Mirrored = len(mine)
	if _, err := os.Stat(filepath.Join(p.DstRoot, p.Top)); err == nil && len(mine) == 0 {
		sp.Adopt = true
		return sp, nil
	}

	fold := strings.ToUpper
	if p.DstFS.CaseSensitive {
		fold = func(s string) string { return s }
	}
	planned := make(map[string]bool, len(p.Items))
	for i := range p.Items {
		planned[fold(manifest.Key(p.Items[i].Rel))] = true
	}
	unlisted := make([]string, 0, len(p.UnlistedDirs))
	for _, d := range p.UnlistedDirs {
		unlisted = append(unlisted, fold(manifest.Key(d)))
	}
	underUnlisted := func(k string) bool {
		for _, d := range unlisted {
			if k == d || strings.HasPrefix(k, d+`\`) {
				return true
			}
		}
		return false
	}

	// Recorded files the source no longer has, still on the destination as
	// DriveDex left them.
	var vanished []manifest.Record
	for _, r := range mine {
		k := fold(r.Rel)
		if planned[k] {
			continue
		}
		if underUnlisted(k) {
			sp.KeptUnlisted++
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, err := os.Lstat(filepath.Join(p.DstRoot, filepath.FromSlash(strings.ReplaceAll(r.Rel, `\`, "/")))); err != nil {
			sp.Gone = append(sp.Gone, r.Rel) // already gone: nothing to remove, the record is dropped
			continue
		}
		vanished = append(vanished, r)
	}

	if o.DetectMoves && len(vanished) > 0 {
		used := sp.pairMoves(ctx, vanished, o)
		kept := vanished[:0]
		for _, r := range vanished {
			if !used[r.Rel] {
				kept = append(kept, r)
			}
		}
		vanished = kept
	}
	for _, r := range vanished {
		sp.Removals = append(sp.Removals, Removal{Rel: filepath.FromSlash(strings.ReplaceAll(r.Rel, `\`, "/")), Size: r.DstSize})
	}
	sort.Slice(sp.Removals, func(a, b int) bool { return sp.Removals[a].Rel < sp.Removals[b].Rel })
	n := len(sp.Removals)
	sp.MassDelete = n > massDeleteFiles || (sp.Mirrored > 0 && float64(n) > massDeleteShare*float64(sp.Mirrored))
	return sp, nil
}

// pairMoves matches vanished records with new files (Copy decisions): by
// NTFS file ID when both sides know it, otherwise by name, size and time,
// counting only unique one-to-one pairs. Paired items get MoveFrom; the
// returned set holds the records used.
func (sp *SyncPlan) pairMoves(ctx context.Context, vanished []manifest.Record, o SyncOptions) map[string]bool {
	used := map[string]bool{}
	byID := map[uint64]int{}
	type key struct {
		name string
		size int64
	}
	byKey := map[key][]int{}
	for i, r := range vanished {
		if r.SrcFileID != 0 {
			byID[r.SrcFileID] = i
		}
		name := r.Rel[strings.LastIndexByte(r.Rel, '\\')+1:]
		k := key{strings.ToUpper(name), r.SrcSize}
		byKey[k] = append(byKey[k], i)
	}
	var newByKey = map[key][]*Item{}
	for i := range sp.Items {
		it := &sp.Items[i]
		if it.Decision != Copy || it.Blocked != "" {
			continue
		}
		if it.SrcFileID != 0 {
			if ri, ok := byID[it.SrcFileID]; ok && !used[vanished[ri].Rel] {
				sp.claim(it, vanished[ri], used)
				continue
			}
		}
		_, name := splitDirBase(it.Rel)
		k := key{strings.ToUpper(name), it.Size}
		newByKey[k] = append(newByKey[k], it)
	}
	var bufp *[]byte
	for k, items := range newByKey {
		cands := byKey[k]
		if len(items) != 1 || len(cands) != 1 || used[vanished[cands[0]].Rel] {
			continue // ambiguous (or nothing): copied normally
		}
		it, r := items[0], vanished[cands[0]]
		same, _ := fsinfo.SameTime(it.MTimeNs(), r.SrcMTimeNs, sp.SrcFS, sp.SrcFS, it.TimeCoarse)
		if !same {
			continue
		}
		if o.ByHash {
			if r.HashKind != manifest.HashXXH3 || sp.Phone != nil {
				continue
			}
			if bufp == nil {
				bufp = bufPool.Get().(*[]byte)
				defer bufPool.Put(bufp)
			}
			h, err := hashCached(ctx, it.Src, *bufp)
			if err != nil || h != r.Hash {
				continue
			}
		}
		sp.claim(it, r, used)
	}
	return used
}

func (sp *SyncPlan) claim(it *Item, r manifest.Record, used map[string]bool) {
	used[r.Rel] = true
	it.MoveFrom = filepath.FromSlash(strings.ReplaceAll(r.Rel, `\`, "/"))
	sp.Moves = append(sp.Moves, it)
}

// SyncCounts summarises a sync plan for the confirmation.
type SyncCounts struct {
	Copy, Replace, Move, Remove int
	CopyBytes, ReplaceBytes     int64
	MoveBytes, RemoveBytes      int64
}

// Counts tallies what the sync will do.
func (sp *SyncPlan) SyncCounts() SyncCounts {
	var c SyncCounts
	for i := range sp.Items {
		it := &sp.Items[i]
		switch {
		case it.MoveFrom != "":
			c.Move++
			c.MoveBytes += it.Size
		case it.Decision == Copy:
			c.Copy++
			c.CopyBytes += it.Size
		case it.Decision == Overwrite:
			c.Replace++
			c.ReplaceBytes += it.Size
		}
	}
	for _, r := range sp.Removals {
		c.Remove++
		c.RemoveBytes += r.Size
	}
	return c
}

// ApplyMoves renames each moved file on the destination (an instant rename on
// the same drive) and marks the item done. A move that fails leaves the item
// as an ordinary copy. record is called for every move that succeeded.
func (sp *SyncPlan) ApplyMoves(record func(it *Item, from string)) (moved int, err error) {
	for _, it := range sp.Moves {
		from := filepath.Join(sp.DstRoot, it.MoveFrom)
		to := filepath.Join(sp.DstRoot, it.Rel)
		if _, serr := os.Lstat(to); serr == nil {
			it.MoveFrom = "" // something appeared there meanwhile: copy normally
			continue
		}
		if merr := os.MkdirAll(filepath.Dir(to), 0o755); merr != nil {
			err = merr
			it.MoveFrom = ""
			continue
		}
		if rerr := os.Rename(from, to); rerr != nil {
			err = fmt.Errorf("moving %s to %s: %w", it.MoveFrom, it.Rel, rerr)
			it.MoveFrom = ""
			continue
		}
		if fi, serr := os.Lstat(to); serr == nil {
			it.Dst = dstStateOf(fi)
		}
		it.Decision, it.Why = SkipSame, WhyMoved
		if record != nil {
			record(it, it.MoveFrom)
		}
		moved++
	}
	return moved, err
}

// ApplyRemovals moves every removed file to the trash, then removes the
// folders that became empty because of it. forget is called for each file
// moved. Removals run last, after the copies, so an interruption never
// leaves the mirror with fewer files than before.
func (sp *SyncPlan) ApplyRemovals(ctx context.Context, t Trasher, forget func(rel string)) (removed int, bytes int64, err error) {
	parents := map[string]bool{}
	for _, r := range sp.Removals {
		if ctx.Err() != nil {
			return removed, bytes, ctx.Err()
		}
		if terr := t.Move(r.Rel, "removed"); terr != nil {
			if os.IsNotExist(terr) {
				forget(r.Rel)
				continue
			}
			err = terr
			continue
		}
		forget(r.Rel)
		removed++
		bytes += r.Size
		for d := filepath.Dir(r.Rel); d != "." && d != sp.Top && d != ""; d = filepath.Dir(d) {
			parents[d] = true
		}
	}
	// Deepest first; os.Remove only removes an empty folder, and a folder the
	// source still has is kept even when empty.
	keep := map[string]bool{}
	for _, d := range sp.Dirs {
		keep[strings.ToUpper(d)] = true
	}
	dirs := make([]string, 0, len(parents))
	for d := range parents {
		if !keep[strings.ToUpper(d)] {
			dirs = append(dirs, d)
		}
	}
	sort.Slice(dirs, func(a, b int) bool { return len(dirs[a]) > len(dirs[b]) })
	for _, d := range dirs {
		os.Remove(filepath.Join(sp.DstRoot, d))
	}
	return removed, bytes, err
}
