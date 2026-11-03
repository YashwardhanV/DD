package cli

import (
	"bufio"
	"fmt"

	"github.com/yashwardhanv/dd/internal/agent"
	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/ui"
)

// loadPhoneIndex returns the saved index of the phone with this serial, or
// nil if there is none (or it can't be read - the copy then lists the phone).
func loadPhoneIndex(serial string) *index.Index {
	store, err := openStore()
	if err != nil {
		return nil
	}
	id := phone.SourceID(serial)
	if !store.Exists(id) {
		return nil
	}
	ix, err := store.Load(id)
	if err != nil {
		return nil
	}
	return ix
}

// refreshPhoneIndex puts what the delta listing learned into the saved phone
// index after a copy - a free refresh. The index is only marked fresh
// ("updated now") when the delta covered the whole indexed part of the phone;
// otherwise changes elsewhere on the phone would be missed by the next delta.
// If the agent (or 'drivedex index phone') is re-indexing the phone right now,
// this is skipped: its fresh index wins.
func refreshPhoneIndex(out *bufio.Writer, serial string, plan copier.PhonePlanResult, res copier.Result) {
	if plan.Plan == nil || plan.Plan.Source != copier.PlanPhoneIndex || (len(plan.Changed) == 0 && !plan.CoversIndex) || res.Cancelled {
		return
	}
	store, err := openStore()
	if err != nil {
		return
	}
	id := phone.SourceID(serial)
	release, err := agent.AcquireLock(store.LockPath(id))
	if err != nil {
		return // being re-indexed right now
	}
	defer release()
	ix, err := store.Load(id) // re-read: never write back an older copy
	if err != nil {
		return
	}
	n := ix.UpsertPaths(plan.Changed)
	if plan.CoversIndex && !plan.DeltaTime.IsZero() && plan.DeltaTime.After(ix.Header.UpdatedAt) {
		ix.Header.UpdatedAt = plan.DeltaTime
	} else if n == 0 {
		return
	}
	ix.Header.EntryCount = ix.LiveLen()
	if _, err := store.Save(ix); err != nil {
		fmt.Fprintln(out, ui.Dim("note: couldn't update the phone index: "+err.Error()))
		return
	}
	if n > 0 {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("Phone index updated with %d changed %s.", n, plural(n, "file", "files"))))
	}
}
