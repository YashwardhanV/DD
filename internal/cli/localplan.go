package cli

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/yashwardhanv/dd/internal/agent"
	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/ui"
)

// findCoveringIndex finds the saved index that covers the local folder src
// (a whole-drive index through the drive's current letter, or a folder index
// through its path) without loading every index: the headers are read first
// and only the best match - the one with the longest root - is loaded.
func findCoveringIndex(src string) (index.FolderScope, bool) {
	store, err := openStore()
	if err != nil {
		return index.FolderScope{}, false
	}
	infos, err := store.List()
	if err != nil {
		return index.FolderScope{}, false
	}
	env := currentEnv(false)
	best, bestLen := "", -1
	for _, in := range infos {
		if in.Err != nil {
			continue
		}
		h := in.Header
		var root string
		switch h.SourceKind {
		case index.KindVolume:
			r, ok := env.Mounts[h.SourceID]
			if !ok {
				continue
			}
			root = r
		case index.KindFolder:
			root = h.RootPath
		default:
			continue
		}
		if _, ok := index.RelativeTo(root, src); ok && len(root) > bestLen {
			best, bestLen = h.SourceID, len(root)
		}
	}
	if best == "" {
		return index.FolderScope{}, false
	}
	ix, err := store.Load(best)
	if err != nil {
		return index.FolderScope{}, false
	}
	return index.LocateScope([]*index.Index{ix}, env, src)
}

// refreshFromJournal brings an NTFS turbo-mode index up to date from the
// drive's change journal (milliseconds) and saves it. It only works in an
// Administrator terminal; ok is false whenever it couldn't be done.
func refreshFromJournal(ctx context.Context, scope index.FolderScope) (index.FolderScope, bool) {
	ix := scope.Index
	if ix.Header.Scanner != "mft" || ix.Header.SourceKind != index.KindVolume || !isElevated() {
		return scope, false
	}
	root, ok := currentEnv(false).Mounts[ix.Header.SourceID]
	if !ok {
		return scope, false
	}
	sess, err := openNTFS(root)
	if err != nil {
		return scope, false
	}
	defer sess.Close()
	sess.SkipDirs = activeSkipDirs()
	store, err := openStore()
	if err != nil {
		return scope, false
	}
	release, err := agent.AcquireLock(store.LockPath(ix.Header.SourceID))
	if err != nil {
		return scope, false // being re-indexed right now
	}
	defer release()
	if _, err := ntfs.Refresh(ctx, sess, ix); err != nil {
		return scope, false
	}
	ix.Header.UpdatedAt = time.Now()
	ix.Compact()
	store.Save(ix) // best effort: the refreshed index is used either way
	id, ok := ix.FindFolder(scope.Rel)
	if !ok {
		return scope, false
	}
	return index.FolderScope{Index: ix, FolderID: id, Rel: scope.Rel}, true
}

// planLocalCopy plans a local copy with the stage 17 rules.
func planLocalCopy(ctx context.Context, src, dst, policy string, workers int, mode string, anyAge bool) (copier.LocalPlanResult, error) {
	req := copier.LocalPlanRequest{Src: src, DstRoot: dst, Policy: policy, Workers: workers, Mode: mode}
	if mode != "walk" {
		req.Scope, req.ScopeOK = findCoveringIndex(src)
		if req.ScopeOK {
			req.Scope, req.Refreshed = refreshFromJournal(ctx, req.Scope)
		}
		req.SeekPenalty, req.SeekKnown = seekPenaltyFor(src)
		if anyAge {
			req.MaxAge = -1
		} else if d, err := activeConfig.IndexPlanMaxAgeDuration(); err == nil {
			req.MaxAge = d
		}
	}
	return copier.PlanLocalSmart(ctx, req)
}

// refreshLocalIndex puts what the sweep saw into the drive's saved index, so
// the next copy plans from a fresh index: new and changed files are added,
// files that are gone are removed. The index is marked fresh only when the
// copied folder is the whole indexed source. NTFS turbo-mode indexes are left
// alone (their IDs are the file system's own; the journal keeps them fresh).
func refreshLocalIndex(out *bufio.Writer, scope index.FolderScope, sweep copier.SweepResult, sweepStart time.Time) {
	if !sweep.Complete || scope.Index == nil || scope.Index.Header.Scanner == "mft" {
		return
	}
	store, err := openStore()
	if err != nil {
		return
	}
	id := scope.Index.Header.SourceID
	release, err := agent.AcquireLock(store.LockPath(id))
	if err != nil {
		return
	}
	defer release()
	ix, err := store.Load(id) // re-read: never write back an older copy
	if err != nil || ix.Header.Scanner == "mft" {
		return
	}
	prefix := strings.Trim(scope.Rel, `\`)
	join := func(relBelow string) string {
		if prefix == "" {
			return relBelow
		}
		return prefix + `\` + relBelow
	}
	seen := make(map[string]bool, len(sweep.Files))
	updates := make([]index.PathUpdate, 0, len(sweep.Files))
	for _, j := range sweep.Files {
		// Job.Rel is "<folder name>\<path below it>".
		below := j.Rel
		if i := strings.IndexRune(below, filepath.Separator); i >= 0 {
			below = below[i+1:]
		}
		below = strings.ReplaceAll(below, string(filepath.Separator), `\`)
		seen[strings.ToLower(below)] = true
		updates = append(updates, index.PathUpdate{Rel: join(below), Size: j.Size, ModTime: j.ModTime})
	}
	changed := ix.UpsertPaths(updates)
	removed := 0
	if folderID, ok := ix.FindFolder(prefix); ok {
		var gone []uint64
		ix.WalkFolder(ix.BuildChildren(), folderID, func(slot int, rel string) bool {
			if ix.Flags[slot]&index.FlagDir == 0 && !seen[strings.ToLower(rel)] {
				gone = append(gone, ix.IDs[slot])
			}
			return true
		})
		for _, g := range gone {
			if ix.Delete(g) {
				removed++
			}
		}
	}
	if changed == 0 && removed == 0 && prefix != "" {
		return
	}
	if prefix == "" && sweepStart.After(ix.Header.UpdatedAt) {
		ix.Header.UpdatedAt = sweepStart
	}
	ix.Compact()
	if _, err := store.Save(ix); err != nil {
		fmt.Fprintln(out, ui.Dim("note: couldn't update the index: "+err.Error()))
		return
	}
	if changed+removed > 0 {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("Index of %s updated: %d added or changed, %d removed.", describeIndex(ix.Header), changed, removed)))
	}
}
