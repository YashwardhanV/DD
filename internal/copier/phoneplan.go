package copier

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/yashwardhanv/dd/internal/copier/trace"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/ui"
)

// Planning a phone copy from the phone's saved index (BUILD-V2 stage 15)
//
// v1 always started a phone copy by listing the folder on the phone with one
// big 'find' - even though the background agent indexed the phone when it was
// plugged in. For a folder with tens of thousands of files that listing takes
// a while, and nothing is copied until it ends.
//
// v2 starts from the saved index instead and asks the phone only for what
// CHANGED since the index was made:
//
//	find <folder> -type f -cmin -M ...      (M = minutes since the index + 2)
//
// "-cmin" matches files whose status changed in the last M minutes: new
// files, edited files, and files that were moved or renamed. The plan is
// "every file the index knows in that folder" plus "everything the phone
// says changed" (the phone wins where both have a file). A file deleted
// since the index was made can't be listed by the phone any more: it shows up
// at transfer time as "vanished" - reported, but not counted as a failure.

// Plan sources a phone copy can use.
const (
	PlanPhoneFind  = "phone-find"
	PlanPhoneIndex = "phone-index"
)

// PhonePlanMaxAge is the oldest phone index a plan is based on (older ones
// mean the phone is listed in full, as in v1).
const PhonePlanMaxAge = 7 * 24 * time.Hour

// clockSkewMinutes is added to the delta window, in case the phone's clock
// and the PC's disagree a little.
const clockSkewMinutes = 2

// maxNoMetaShare: an index where more than this share of the files have no
// size/time (a names-only scan) can't be planned from.
const maxNoMetaShare = 0.05

// PhonePlanRequest describes a phone copy to plan.
type PhonePlanRequest struct {
	ADB     *phone.ADB
	Serial  string
	Remote  string // absolute folder or file on the phone
	DstRoot string
	Policy  string
	Workers int

	// Index is the phone's saved index (nil = none).
	Index *index.Index
	// Mode is "auto" (index when usable, else find), "index" (index or an
	// error saying why not) or "find" (always list the phone: --fresh, --v1).
	Mode string
	// MaxAge is the oldest usable index (0 = PhonePlanMaxAge; < 0 = any age,
	// for copy_plan_from_index = "always").
	MaxAge time.Duration
	// Now is the current time (tests set it).
	Now time.Time
}

// PhonePlanResult is a plan plus what the CLI needs to tell the owner and to
// refresh the index afterwards.
type PhonePlanResult struct {
	Plan *Plan
	// Why is the one line printed before the copy, e.g. "Planned from the
	// phone index (made 12 min ago) + 214 changed files."
	Why string
	// Changed are the files the delta listing returned (paths relative to
	// the index root), to upsert into the saved index after the copy.
	Changed []index.PathUpdate
	// CoversIndex is true when the delta listed the index's whole root, so
	// the index may be marked as fresh as of DeltaTime.
	CoversIndex bool
	DeltaTime   time.Time
}

// PlanPhoneSmart plans a phone copy, from the saved index plus a delta
// listing when that is possible and trustworthy, otherwise like v1.
func PlanPhoneSmart(ctx context.Context, r PhonePlanRequest) (PhonePlanResult, error) {
	if r.Now.IsZero() {
		r.Now = time.Now()
	}
	info, err := statRemote(ctx, r.ADB, r.Serial, r.Remote)
	if err != nil {
		return PhonePlanResult{}, err
	}
	why, usable := phoneIndexUsable(ctx, r, info)
	if !usable {
		if r.Mode == "index" {
			return PhonePlanResult{}, fmt.Errorf("%w: %s", ErrIndexNotUsable, why)
		}
		plan, err := planPhoneFind(ctx, r.ADB, r.Serial, r.Remote, info, r.DstRoot, r.Policy, r.Workers)
		if err != nil {
			return PhonePlanResult{}, err
		}
		return PhonePlanResult{Plan: plan, Why: "Listed the phone folder (" + why + ")."}, nil
	}
	return planFromPhoneIndex(ctx, r)
}

// ErrIndexNotUsable means --plan index was asked for but the index can't be used.
var ErrIndexNotUsable = fmt.Errorf("the saved index can't be used to plan this copy")

// phoneIndexUsable decides whether the phone's index can plan this copy,
// and if not, says why in a few words.
func phoneIndexUsable(ctx context.Context, r PhonePlanRequest, info phone.PathInfo) (string, bool) {
	switch {
	case r.Mode == "find":
		return "a fresh listing was asked for", false
	case r.Index == nil:
		return "this phone has no saved index yet", false
	case !info.IsDir:
		return "a single file", false
	}
	h := &r.Index.Header
	maxAge := r.MaxAge
	if maxAge == 0 {
		maxAge = PhonePlanMaxAge
	}
	age := r.Now.Sub(h.UpdatedAt)
	if maxAge > 0 && age > maxAge {
		return "the phone index was made " + ui.Ago(h.UpdatedAt, r.Now), false
	}
	if age < -time.Hour {
		return "the phone index is dated in the future (clock change?)", false
	}
	rel, ok := phoneRel(h.RootPath, r.Remote)
	if !ok {
		return "the folder is outside the indexed part of the phone (" + h.RootPath + ")", false
	}
	if _, ok := r.Index.FindFolder(rel); !ok {
		return "the folder is newer than the phone index", false
	}
	if share := noMetaShare(r.Index); share > maxNoMetaShare {
		return "the phone index has no sizes and dates", false
	}
	caps, err := r.ADB.Probe(ctx, r.Serial)
	if err != nil || !caps.FindCmin {
		return "this phone's find can't list only what changed (-cmin)", false
	}
	return "", true
}

// phoneRel returns remote relative to the index root ("/sdcard"), with "/".
func phoneRel(root, remote string) (string, bool) {
	root = path.Clean("/" + strings.TrimSpace(root))
	remote = path.Clean("/" + strings.TrimSpace(remote))
	if remote == root {
		return "", true
	}
	if root == "/" {
		return strings.TrimPrefix(remote, "/"), true
	}
	if strings.HasPrefix(remote, root+"/") {
		return remote[len(root)+1:], true
	}
	return "", false
}

// noMetaShare is the share of files in the index without size/time.
func noMetaShare(ix *index.Index) float64 {
	var files, noMeta int
	for slot := range ix.IDs {
		if ix.Flags[slot]&(index.FlagDir|index.FlagDeleted) != 0 {
			continue
		}
		files++
		if ix.Flags[slot]&index.FlagNoMeta != 0 || ix.Sizes[slot] < 0 {
			noMeta++
		}
	}
	if files == 0 {
		return 0
	}
	return float64(noMeta) / float64(files)
}

// planFromPhoneIndex builds the plan from the index plus the delta.
func planFromPhoneIndex(ctx context.Context, r PhonePlanRequest) (PhonePlanResult, error) {
	tr := trace.From(ctx)
	ix := r.Index
	h := &ix.Header
	rel, _ := phoneRel(h.RootPath, r.Remote)
	folderID, _ := ix.FindFolder(rel)
	remote := path.Clean(r.Remote)

	// 1. Ask the phone what changed since the index was made.
	minutes := int(r.Now.Sub(h.UpdatedAt).Minutes()+0.999) + clockSkewMinutes
	m := tr.Start()
	deltaTime := r.Now
	changed, err := r.ADB.ListChanged(ctx, r.Serial, remote, minutes)
	tr.End(trace.PhoneList, m)
	if err != nil {
		return PhonePlanResult{}, fmt.Errorf("asking the phone what changed in %s: %w", remote, err)
	}

	// 2. Everything the index knows in that folder (PlanList: reading the index).
	m = tr.Start()
	name := phoneFolderName(remote)
	dirs := []string{name}
	jobs := map[string]Job{} // by phone path
	order := []string{}
	ix.WalkFolder(ix.BuildChildren(), folderID, func(slot int, relBelow string) bool {
		if ix.Flags[slot]&index.FlagDir != 0 {
			dirs = append(dirs, filepath.Join(name, SafeRel(relBelow)))
			return true
		}
		src := remote + "/" + strings.ReplaceAll(relBelow, `\`, "/")
		jobs[src] = Job{Src: src, Rel: filepath.Join(name, SafeRel(relBelow)), Size: max(ix.Sizes[slot], 0), ModTime: ix.MTimes[slot]}
		order = append(order, src)
		return true
	})
	tr.End(trace.PlanList, m)

	// 3. The phone's answer wins: new files are added, changed ones updated.
	var updates []index.PathUpdate
	for _, f := range changed {
		below, ok := phoneRel(remote, f.Path)
		if !ok || below == "" {
			continue
		}
		src := remote + "/" + below
		if _, known := jobs[src]; !known {
			order = append(order, src)
		}
		jobs[src] = Job{Src: src, Rel: filepath.Join(name, SafeRel(below)), Size: f.Size, ModTime: f.ModTime}
		if idxRel, ok := phoneRel(h.RootPath, f.Path); ok {
			updates = append(updates, index.PathUpdate{Rel: idxRel, Size: f.Size, ModTime: f.ModTime})
		}
	}
	list := make([]Job, 0, len(order))
	for _, src := range order {
		list = append(list, jobs[src])
	}

	plan := finalizeTraced(tr, r.DstRoot, dirs, list, r.Policy, r.Workers)
	plan.Phone = &PhoneSource{ADB: r.ADB, Serial: r.Serial, Remote: remote}
	plan.Source = PlanPhoneIndex
	plan.MayBeStale = true
	return PhonePlanResult{
		Plan:        plan,
		Why:         fmt.Sprintf("Planned from the phone index (made %s) + %d changed %s.", ui.Ago(h.UpdatedAt, r.Now), len(changed), plural(len(changed), "file", "files")),
		Changed:     updates,
		CoversIndex: rel == "",
		DeltaTime:   deltaTime,
	}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
