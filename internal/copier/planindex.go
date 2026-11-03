package copier

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yashwardhanv/dd/internal/copier/trace"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ui"
)

// Planning a drive copy from the index (BUILD-V2 stage 17)
//
// v1 starts every copy by listing the source folder ("Reading the file
// list..."), on the slowest device in the chain, and copies nothing until the
// listing is complete. When the drive has a fresh index that listing is
// already on the PC: v2 plans from it and starts copying at once.
//
// The rules (BUILD-V2 section 4.3):
//
//	NTFS turbo index + Administrator + journal  -> refresh from the journal (ms), plan from it, no sweep
//	flash / SSD source, fresh index             -> plan from the index, sweep in parallel
//	hard disk source without the journal        -> list it (v1): a sweep would make the head
//	                                               jump between listing and reading
//	--plan walk, --fresh, --v1                  -> list it (v1)

// Plan sources of a local copy.
const (
	PlanWalk     = "walk"
	PlanIndex    = "index"
	PlanUSNIndex = "usn+index"
)

// DefaultIndexPlanMaxAge is index_plan_max_age's default.
const DefaultIndexPlanMaxAge = 15 * time.Minute

// minMetaShare: at least this share of the folder's files must have a known
// size and time (NTFS turbo indexes made without --with-metadata don't).
const minMetaShare = 0.95

// LocalPlanRequest describes a local folder copy to plan.
type LocalPlanRequest struct {
	Src     string // the folder being copied (absolute)
	DstRoot string
	Policy  string
	Workers int

	// Scope is where Src lives in a saved index (ok=false: no index covers it).
	Scope   index.FolderScope
	ScopeOK bool
	// Mode: "auto", "index" (or an error saying why not) or "walk" (v1).
	Mode string
	// MaxAge is index_plan_max_age (0 = 15 min, < 0 = any age).
	MaxAge time.Duration
	// Refreshed: the index was just brought up to date from the NTFS change
	// journal, so its age doesn't matter and no sweep is needed.
	Refreshed bool
	// SeekPenalty / SeekKnown: is the source a spinning hard disk?
	SeekPenalty, SeekKnown bool
	Now                    time.Time
}

// LocalPlanResult is the plan plus what the CLI shows and needs afterwards.
type LocalPlanResult struct {
	Plan *Plan
	// Why is the line printed before the copy.
	Why string
	// Sweeper is the reconciling sweep running alongside the copy (nil = none).
	Sweeper *DirSweeper
	// Scope is the index the plan came from (zero for a listed source).
	Scope index.FolderScope
}

// PlanLocalSmart plans a local copy from the index when the rules allow it,
// otherwise by listing the source like v1.
func PlanLocalSmart(ctx context.Context, r LocalPlanRequest) (LocalPlanResult, error) {
	if r.Now.IsZero() {
		r.Now = time.Now()
	}
	st, err := os.Stat(r.Src)
	if err != nil {
		return LocalPlanResult{}, fmt.Errorf("opening %s: %w", r.Src, err)
	}
	why, usable := localIndexUsable(r, st.IsDir())
	if !usable {
		if r.Mode == "index" {
			return LocalPlanResult{}, fmt.Errorf("%w: %s", ErrIndexNotUsable, why)
		}
		p, err := PlanLocal(ctx, r.Src, r.DstRoot, r.Policy, r.Workers)
		if err != nil {
			return LocalPlanResult{}, err
		}
		msg := "Listed the source"
		if why != "" {
			msg += " (" + why + ")"
		}
		return LocalPlanResult{Plan: p, Why: msg + "."}, nil
	}
	if inside(r.Src, r.DstRoot) {
		return LocalPlanResult{}, fmt.Errorf("%w: %s is inside %s", ErrDestinationInsideSource, r.DstRoot, r.Src)
	}
	return planFromLocalIndex(ctx, r)
}

// localIndexUsable applies the rules; when the index can't be used it says why.
func localIndexUsable(r LocalPlanRequest, isDir bool) (string, bool) {
	switch {
	case r.Mode == "walk":
		return "", false
	case !isDir:
		return "", false
	case !r.ScopeOK || r.Scope.Index == nil:
		return "no saved index covers it", false
	}
	h := &r.Scope.Index.Header
	if !r.Refreshed {
		if r.SeekPenalty && r.SeekKnown {
			return "a hard disk: planning from the index there needs the NTFS change journal (Administrator)", false
		}
		maxAge := r.MaxAge
		if maxAge == 0 {
			maxAge = DefaultIndexPlanMaxAge
		}
		if age := r.Now.Sub(h.UpdatedAt); maxAge > 0 && age > maxAge {
			return "the index was made " + ui.Ago(h.UpdatedAt, r.Now) + ", older than index_plan_max_age", false
		}
	}
	if share := metaShare(r.Scope.Index, r.Scope.FolderID); share < minMetaShare {
		return fmt.Sprintf("only %.0f%% of its files have a size and date in the index (an NTFS turbo index without --with-metadata)", share*100), false
	}
	return "", true
}

// metaShare is the share of files under a folder with a known size and time.
func metaShare(ix *index.Index, folderID uint64) float64 {
	var files, known int
	ix.WalkFolder(ix.BuildChildren(), folderID, func(slot int, _ string) bool {
		if ix.Flags[slot]&index.FlagDir == 0 {
			files++
			if ix.Flags[slot]&index.FlagNoMeta == 0 && ix.Sizes[slot] >= 0 {
				known++
			}
		}
		return true
	})
	if files == 0 {
		return 1
	}
	return float64(known) / float64(files)
}

// planFromLocalIndex builds the plan from the index entries under the folder.
func planFromLocalIndex(ctx context.Context, r LocalPlanRequest) (LocalPlanResult, error) {
	tr := trace.From(ctx)
	ix := r.Scope.Index
	name := srcName(r.Src)

	m := tr.Start()
	dirs := []string{name}
	var jobs []Job
	var unknown []int // jobs whose size/time the index doesn't know (looked up below)
	ix.WalkFolder(ix.BuildChildren(), r.Scope.FolderID, func(slot int, relBelow string) bool {
		rel := strings.ReplaceAll(relBelow, `\`, string(filepath.Separator))
		if ix.Flags[slot]&index.FlagDir != 0 {
			dirs = append(dirs, filepath.Join(name, rel))
			return true
		}
		if ix.Flags[slot]&index.FlagNoMeta != 0 || ix.Sizes[slot] < 0 {
			unknown = append(unknown, len(jobs))
		}
		jobs = append(jobs, Job{Src: filepath.Join(r.Src, rel), Rel: filepath.Join(name, rel), Size: max(ix.Sizes[slot], 0), ModTime: ix.MTimes[slot]})
		return true
	})
	// The few files whose size/time the index lacks (changed since an NTFS
	// journal refresh) are looked up directly; one that is gone is dropped.
	if len(unknown) > 0 {
		gone := make([]bool, len(jobs))
		parallelFor(len(unknown), r.Workers, func(i int) {
			j := &jobs[unknown[i]]
			st, err := os.Lstat(j.Src)
			if err != nil {
				gone[unknown[i]] = errors.Is(err, fs.ErrNotExist)
				return
			}
			j.Size, j.ModTime = st.Size(), st.ModTime().Unix()
		})
		kept := jobs[:0]
		for i, j := range jobs {
			if !gone[i] {
				kept = append(kept, j)
			}
		}
		jobs = kept
	}
	tr.End(trace.PlanList, m)

	p := finalizeTraced(tr, r.DstRoot, dirs, jobs, r.Policy, r.Workers)
	p.MayBeStale = true
	res := LocalPlanResult{Plan: p, Scope: r.Scope}
	label := ix.Header.Label
	if strings.TrimSpace(label) == "" {
		label = r.Src
	}
	if r.Refreshed {
		p.Source = PlanUSNIndex
		res.Why = fmt.Sprintf("Planned from the index of %s (refreshed from the NTFS change journal) - %s files.", label, ui.Thousands(int64(len(jobs))))
		return res, nil
	}
	p.Source = PlanIndex
	known := make(map[string]Job, len(jobs))
	for _, j := range jobs {
		known[j.Rel] = j
	}
	res.Sweeper = &DirSweeper{Src: r.Src, Name: name, Known: known}
	p.Reconciler = res.Sweeper
	res.Why = fmt.Sprintf("Planned from the index of %s (made %s) - %s files; checking for changes while copying.",
		label, ui.Ago(ix.Header.UpdatedAt, r.Now), ui.Thousands(int64(len(jobs))))
	return res, nil
}
