package copier

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/index"
)

// ErrNotAFolder means the folder to plan from isn't in the index as a folder.
var ErrNotAFolder = errors.New("that folder isn't in the saved index")

// Children is a "compressed sparse row" table of an index's tree: for every
// folder, the slots of its direct children, all in one array. Built once in
// O(n), it lets a walk visit each entry once instead of walking up from each
// entry to the root (index.Under is O(depth) per entry).
type Children struct {
	off   []int32 // children of folder f are kids[off[f]:off[f+1]]
	kids  []int32
	slots map[uint64]int32 // folder ID -> folder number (0 = the root)
}

// BuildChildren makes the table for ix (tombstones are left out).
func BuildChildren(ix *index.Index) *Children {
	n := ix.Len()
	c := &Children{slots: make(map[uint64]int32)}
	// Folder numbers: 0 for the root, then 1 + slot for every folder.
	folderOf := func(parentID uint64) (int32, bool) {
		if parentID == 0 {
			return 0, true
		}
		s, ok := ix.Slot(parentID)
		if !ok {
			return 0, false
		}
		return int32(s) + 1, true
	}
	count := make([]int32, n+2)
	parentNo := make([]int32, n)
	for s := 0; s < n; s++ {
		parentNo[s] = -1
		if ix.Flags[s]&index.FlagDeleted != 0 {
			continue
		}
		if f, ok := folderOf(ix.Parents[s]); ok {
			parentNo[s] = f
			count[f+1]++
		}
	}
	c.off = make([]int32, n+2)
	for f := 1; f < n+2; f++ {
		c.off[f] = c.off[f-1] + count[f]
	}
	c.kids = make([]int32, c.off[n+1])
	next := append([]int32(nil), c.off...)
	for s := 0; s < n; s++ {
		if f := parentNo[s]; f >= 0 {
			c.kids[next[f]] = int32(s)
			next[f]++
		}
	}
	return c
}

// Of returns the child slots of the folder with the given ID (0 = the root).
func (c *Children) Of(ix *index.Index, folderID uint64) []int32 {
	f := int32(0)
	if folderID != 0 {
		s, ok := ix.Slot(folderID)
		if !ok {
			return nil
		}
		f = int32(s) + 1
	}
	return c.kids[c.off[f]:c.off[f+1]]
}

// PlanFromIndex plans copying the folder with ID folderID (0 = the whole
// source) of ix, currently found at srcAbs, into dstRoot. It produces exactly
// what PlanLocal would produce for the same tree - same folders, same files,
// same sizes and times, same links skipped - but without listing the source:
// the saved index already is the list. Use it only for an index proven fresh
// (ntfs.Fresh; INV-5).
func PlanFromIndex(ctx context.Context, ix *index.Index, folderID uint64, srcAbs, dstRoot, policy string, workers int) (*Plan, error) {
	if folderID != 0 {
		s, ok := ix.Slot(folderID)
		if !ok || ix.Flags[s]&index.FlagDir == 0 {
			return nil, ErrNotAFolder
		}
	}
	if inside(srcAbs, dstRoot) {
		return nil, fmt.Errorf("%w: %s is inside %s", ErrDestinationInsideSource, dstRoot, srcAbs)
	}
	name := srcName(srcAbs)
	children := BuildChildren(ix)
	coarse := !ix.Header.ExactTimes()
	sep := string(filepath.Separator)

	type frame struct {
		id  uint64
		rel string // relative to the copied folder, OS separators ("" = the folder itself)
	}
	dirs := []string{name}
	var jobs []Job
	var links int64
	stack := []frame{{id: folderID}}
	visited := 0
	for len(stack) > 0 {
		fr := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, s32 := range children.Of(ix, fr.id) {
			s := int(s32)
			if visited++; visited%8192 == 0 && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			rel := ix.Names[s]
			if fr.rel != "" {
				rel = fr.rel + sep + rel
			}
			fl := ix.Flags[s]
			switch {
			case fl&index.FlagReparse != 0:
				// A link or junction (a junction is a folder with a reparse
				// point): never followed or copied, as PlanLocal does.
				links++
			case fl&index.FlagDir != 0:
				dirs = append(dirs, name+sep+rel)
				stack = append(stack, frame{id: ix.IDs[s], rel: rel})
			default:
				jobs = append(jobs, Job{
					Src:        srcAbs + sepUnlessThere(srcAbs) + rel,
					Rel:        name + sep + rel,
					Size:       max(ix.Sizes[s], 0),
					ModTime:    ix.MTimes[s],
					ModTimeNs:  ix.ModTimeNs(s),
					TimeCoarse: coarse,
					SrcFileID:  ix.IDs[s],
				})
			}
		}
	}
	p := finalize(dstRoot, dirs, jobs, policy, workers, fsinfo.OrDefault(srcAbs))
	p.LinksSkipped = links
	p.FromIndex = true
	return p, nil
}

func sepUnlessThere(dir string) string {
	if len(dir) > 0 && (dir[len(dir)-1] == filepath.Separator || dir[len(dir)-1] == '/') {
		return ""
	}
	return string(filepath.Separator)
}
