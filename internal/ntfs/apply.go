package ntfs

import (
	"strings"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/scan"
)

// flagsFromAttrs converts Windows file attributes to index flags. Size and
// date are not in USN records, so every entry also gets FlagNoMeta: "unknown,
// fill in lazily when displayed".
func flagsFromAttrs(attrs uint32) uint8 {
	f := index.FlagNoMeta
	if attrs&AttrDirectory != 0 {
		f |= index.FlagDir
	}
	if attrs&AttrHidden != 0 {
		f |= index.FlagHidden
	}
	if attrs&AttrSystem != 0 {
		f |= index.FlagSystem
	}
	return f
}

// skipSet lowercases folder names for case-insensitive matching.
func skipSet(names []string) map[string]struct{} {
	if names == nil {
		names = scan.DefaultSkipDirs
	}
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[strings.ToLower(n)] = struct{}{}
	}
	return m
}

// BuildStats is what AddRecords kept and dropped.
type BuildStats struct {
	Files, Dirs int64
	Metafiles   int64 // NTFS's own records (MFT numbers below 24) that were ignored
	Orphans     int64 // records whose folder chain doesn't reach the root (and so were dropped)
}

// maxChainDepth guards against corrupt data where a folder is inside itself.
const maxChainDepth = 8192

// AddRecords turns raw MFT records into index entries and adds them to b.
//
// What happens to each record:
//
//   - Records numbered below 24 are NTFS metafiles ($MFT, $LogFile, $Extend...).
//     They are skipped, and so is everything inside them.
//   - System folders on the skip list (System Volume Information, $RECYCLE.BIN)
//     are skipped along with their contents.
//   - Anything whose chain of parent folders doesn't lead up to the drive's root
//     is an "orphan" and is dropped. (It was inside something we skipped.)
//   - Everything else becomes an Entry: ID = the file reference number,
//     ParentID = the parent's reference number (0 if directly in the root).
//
// skipNames == nil means scan.DefaultSkipDirs.
func AddRecords(b *index.Builder, recs []Record, rootFRN uint64, skipNames []string) BuildStats {
	var st BuildStats
	skip := skipSet(skipNames)

	// 1. Candidates: drop metafiles and skip-listed folders up front. Because
	// they never enter byFRN, their children can't reach the root (step 2).
	cand := make([]Record, 0, len(recs))
	for _, r := range recs {
		if RecordNumber(r.FRN) < FirstUserRecord {
			st.Metafiles++
			continue
		}
		if r.IsDir() {
			if _, bad := skip[strings.ToLower(r.Name)]; bad {
				continue
			}
		}
		cand = append(cand, r)
	}
	byFRN := make(map[uint64]int32, len(cand))
	for i, r := range cand {
		byFRN[r.FRN] = int32(i)
	}

	// 2. Which candidates are reachable from the root? Follow the parent chain
	// upwards, remembering the answer for every folder on the way (memoization),
	// so each folder is resolved only once however many files it holds.
	const (
		unknown   int8 = 0
		reachable int8 = 1
		orphan    int8 = 2
	)
	state := make([]int8, len(cand))
	var chain []int32
	for i := range cand {
		if state[i] != unknown {
			continue
		}
		chain = chain[:0]
		result := orphan
		for cur := int32(i); ; {
			if state[cur] != unknown {
				result = state[cur]
				break
			}
			chain = append(chain, cur)
			parent := cand[cur].ParentFRN
			if parent == rootFRN {
				result = reachable
				break
			}
			next, ok := byFRN[parent]
			if !ok || len(chain) > maxChainDepth {
				result = orphan // parent missing (skipped or metafile), or a cycle
				break
			}
			cur = next
		}
		for _, c := range chain {
			state[c] = result
		}
	}

	// 3. Emit the reachable entries.
	for i, r := range cand {
		if state[i] != reachable {
			st.Orphans++
			continue
		}
		parent := r.ParentFRN
		if parent == rootFRN {
			parent = 0
		}
		b.Add(index.Entry{
			ID:       r.FRN,
			ParentID: parent,
			Name:     r.Name,
			Size:     -1,
			ModTime:  0,
			Flags:    flagsFromAttrs(r.Attrs),
		})
		if r.IsDir() {
			st.Dirs++
		} else {
			st.Files++
		}
	}
	return st
}

// BuildIndex is AddRecords into a fresh index with the given header.
func BuildIndex(recs []Record, rootFRN uint64, skipNames []string, h index.Header) (*index.Index, BuildStats) {
	b := index.NewBuilder(h)
	st := AddRecords(b, recs, rootFRN, skipNames)
	return b.Index(), st
}

// Applier replays change-journal records onto an existing index.
//
// One rule handles create, rename AND move: "set this entry's name and parent".
// Later records win, so replaying is idempotent. And because paths are rebuilt
// from parent IDs, moving a folder updates ONE entry - every file inside it
// follows automatically, without touching them. (That is why the index stores
// parent IDs instead of full paths.)
type Applier struct {
	ix      *index.Index
	root    uint64
	skip    map[string]struct{}
	touched map[uint64]struct{}

	// Records is the number of journal records examined (metafiles excluded).
	Records int
}

// NewApplier returns an Applier for ix whose drive root is rootFRN.
// skipNames == nil means scan.DefaultSkipDirs.
func NewApplier(ix *index.Index, rootFRN uint64, skipNames []string) *Applier {
	return &Applier{ix: ix, root: rootFRN, skip: skipSet(skipNames), touched: map[uint64]struct{}{}}
}

// Changes is the number of distinct files/folders whose entry was added,
// changed or removed. (One file save writes several journal records; this
// counts the file once.)
func (a *Applier) Changes() int { return len(a.touched) }

func (a *Applier) remove(frn uint64) {
	if a.ix.Delete(frn) {
		a.touched[frn] = struct{}{}
	}
}

// Apply handles one journal record.
func (a *Applier) Apply(r Record) {
	if RecordNumber(r.FRN) < FirstUserRecord {
		return
	}
	a.Records++

	if r.IsDelete() {
		a.remove(r.FRN)
		return
	}
	// A system folder that must stay out of the index (even if it was just
	// created, or something was renamed to that name).
	if r.IsDir() {
		if _, bad := a.skip[strings.ToLower(r.Name)]; bad {
			a.remove(r.FRN)
			return
		}
	}
	parent := uint64(0)
	if r.ParentFRN != a.root {
		if _, known := a.ix.Slot(r.ParentFRN); !known {
			// It lives inside something we don't index (a skipped folder, NTFS
			// metadata). If it was moved there from the visible tree, it must disappear.
			a.remove(r.FRN)
			return
		}
		parent = r.ParentFRN
	}
	a.ix.Upsert(index.Entry{
		ID:       r.FRN,
		ParentID: parent,
		Name:     r.Name,
		Size:     -1,
		ModTime:  0,
		Flags:    flagsFromAttrs(r.Attrs),
	})
	a.touched[r.FRN] = struct{}{}
}

// ApplyDelta applies one journal record to ix (BUILD.md: ntfs.ApplyDelta).
// Prefer an Applier when replaying many records.
func ApplyDelta(ix *index.Index, r Record, rootFRN uint64) {
	NewApplier(ix, rootFRN, nil).Apply(r)
}
