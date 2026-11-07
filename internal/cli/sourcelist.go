package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// Values of copy --source-list.
const (
	sourceListAuto  = "auto"  // the saved index when it is provably current, else list the source
	sourceListIndex = "index" // the saved index, or fail
	sourceListScan  = "scan"  // always list the source (the old behaviour)
)

// planLocalSource plans copying the local folder src into dst, from the saved
// index when that is allowed and proven current (INV-5), otherwise by listing
// the source. It always prints one line saying which it used, and why when
// "auto" fell back to listing.
func planLocalSource(ctx context.Context, out io.Writer, src, dst string, f copyFlags, workers int) (*copier.Plan, string, error) {
	mode := f.sourceList
	if mode == "" {
		mode = sourceListAuto
	}
	if mode != sourceListScan {
		plan, why, err := planFromIndex(ctx, src, dst, f, workers)
		if err != nil {
			return nil, "", err
		}
		if plan != nil {
			return plan, "index", nil
		}
		if mode == sourceListIndex {
			return nil, "", userErr("the saved index can't be used to plan this copy: "+why,
				"use --source-list auto (or scan) to list the source instead", nil)
		}
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("Listing %s … (index not usable: %s)", src, why)))
	} else {
		fmt.Fprintln(out, ui.Dim("Listing "+src+" … (--source-list scan)"))
	}
	plan, err := copier.PlanLocal(ctx, src, dst, f.onConflict, max(workers, 2))
	return plan, "scan", err
}

// planFromIndex tries the saved index. It returns (nil, why, nil) when the
// index can't be used, with why in plain words.
func planFromIndex(ctx context.Context, src, dst string, f copyFlags, workers int) (*copier.Plan, string, error) {
	if driveOf(src) == "" {
		return nil, "the source is not on a drive letter", nil
	}
	vol, err := volumeForPath(src)
	if err != nil || !vol.Ready {
		return nil, "the drive can't be examined", nil
	}
	store, err := openStore()
	if err != nil {
		return nil, "the index folder can't be found", nil
	}
	id := volume.SourceID(vol)
	if !store.Exists(id) {
		return nil, "the drive hasn't been indexed", nil
	}
	if !strings.EqualFold(vol.FileSystem, "NTFS") {
		return nil, "the drive is " + vol.FileSystem + ", not NTFS (no change journal to prove the index is current)", nil
	}
	if !isElevated() {
		return nil, "not running as Administrator", nil
	}

	var plan *copier.Plan
	var why string
	var took time.Duration
	lockErr := withIndexLock(id, vol.Label, func() error {
		ix, err := store.Load(id)
		if err != nil {
			why = "the saved index can't be read"
			return nil
		}
		rel, within := relUnder(vol.Root, src)
		if !within {
			why = "the source is not on the indexed drive"
			return nil
		}
		folderID, found := ix.FindFolder(rel)
		res, err := ntfs.Fresh(ctx, ix, ntfs.FreshEnv{
			Mounted: true, SerialMatches: true, FileSystem: vol.FileSystem, Elevated: true, Root: vol.Root,
			Open: openNTFS, Lister: ntfsLister(vol.Root),
			Save:     func(ix *index.Index) error { _, err := store.Save(ix); return err },
			FolderID: map[bool]uint64{true: folderID, false: 0}[found], Workers: workers,
		})
		if err != nil {
			return err
		}
		if !res.Fresh {
			why = res.Why
			return nil
		}
		if folderID, found = ix.FindFolder(rel); !found {
			why = "the folder isn't in the index"
			return nil
		}
		plan, err = copier.PlanFromIndex(ctx, ix, folderID, src, dst, f.onConflict, max(workers, 2))
		if err != nil {
			return err
		}
		took = res.Took
		return nil
	})
	if lockErr != nil {
		var ue *UserError
		if errors.As(lockErr, &ue) && plan == nil && why == "" {
			return nil, "the drive's index is being updated right now", nil
		}
		if errors.Is(lockErr, context.Canceled) {
			return nil, "", lockErr
		}
		if plan == nil && why == "" {
			return nil, "", lockErr
		}
	}
	if plan == nil {
		return nil, why, nil
	}
	fmt.Fprintln(stdoutOf(f), ui.Dim(fmt.Sprintf("Planned from the saved index of %s (journal checked, %s).", letterOrRoot(vol.Root), roughSeconds(took))))
	return plan, "", nil
}

// relUnder returns path relative to root (with `\`), if it is inside it.
func relUnder(root, path string) (string, bool) {
	r := strings.ToLower(strings.TrimRight(root, `\/`))
	p := strings.ReplaceAll(path, "/", `\`)
	if strings.EqualFold(strings.TrimRight(p, `\`), r) {
		return "", true
	}
	if !strings.HasPrefix(strings.ToLower(p), r+`\`) {
		return "", false
	}
	return strings.Trim(p[len(r):], `\`), true
}

func roughSeconds(d time.Duration) string { return fmt.Sprintf("%.2f s", d.Seconds()) }

// stdoutOf is where the copy's human messages go.
func stdoutOf(f copyFlags) io.Writer {
	if f.human != nil {
		return f.human
	}
	return io.Discard
}
