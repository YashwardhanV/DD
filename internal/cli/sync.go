package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/manifest"
	"github.com/yashwardhanv/dd/internal/preflight"
	"github.com/yashwardhanv/dd/internal/ui"
)

// newSyncCmd implements `drivedex sync <src> <dst>`.
func newSyncCmd() *cobra.Command {
	var workers, verify, onCase, onBad, sourceList, readOrder, compare, detectMoves string
	var dryRun, yes, asJSON, verbose, caseSensitive, allowMassDelete, noManifest bool

	cmd := &cobra.Command{
		Use:   "sync <src> <dst>",
		Short: "Keep a mirror of a folder: copy what changed, follow moves, trash what was deleted",
		Long: `Makes dst\<source folder name> a mirror of src: new and changed files are
copied, files moved in the source are moved on the destination (an instant
rename, not a copy), and files deleted from the source are removed from the
mirror - into the destination's trash, never deleted outright:

  drivedex trash list <dst>       see what was removed or replaced
  drivedex trash restore <dst> <run>

Safety rules:
  - sync only removes files its copy manifest (dst\.drivedex) shows DriveDex
    placed there from this source. Anything else in the folder is left alone.
  - The first sync into a folder that already exists only copies and records;
    it deletes nothing.
  - A source with no files is refused (an unplugged drive or the wrong card
    looks like that). A run that would remove more than half of the mirror,
    or more than 1,000 files, stops unless --allow-mass-delete is given.
  - Moves come first, then copies, removals last: stopping half-way never
    leaves the mirror with fewer files than before.
  - It asks before removing or replacing anything (--yes to skip the question).`,
		Example: `  drivedex sync E:\Photos D:\Backup --dry-run
  drivedex sync E:\Photos D:\Backup
  drivedex sync phone:/DCIM/Camera D:\PhoneBackup --yes`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, ok := copier.ParseVerifyMode(verify)
			if !ok {
				return userErr(fmt.Sprintf("--verify %q isn't valid", verify), "use --verify (the same as --verify=media), --verify=cache or --verify=off", nil)
			}
			names, err := nameOptions(onCase, onBad, caseSensitive)
			if err != nil {
				return err
			}
			if err := checkCopyModes(sourceList, readOrder, compare); err != nil {
				return err
			}
			switch detectMoves {
			case "auto", "off":
			default:
				return userErr(fmt.Sprintf("--detect-moves %q isn't valid", detectMoves), "use auto or off", nil)
			}
			stdout := cmd.OutOrStdout()
			human := stdout
			if asJSON {
				human = cmd.ErrOrStderr()
			}
			js := &copyJSON{}
			err = runCopy(cmd.Context(), human, args[0], args[1], copyFlags{
				workers: effectiveWorkers(cmd, workers), onConflict: copier.OnConflictOverwrite, verify: mode, dryRun: dryRun,
				names: names, yes: yes, verbose: verbose, in: cmd.InOrStdin(), json: js,
				sourceList: sourceList, readOrder: readOrder, human: human, compare: compare, noManifest: noManifest,
				sync: &syncRun{allowMassDelete: allowMassDelete, detectMoves: detectMoves == "auto"},
			})
			if asJSON {
				finishJSON(stdout, js, err)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&workers, "workers", "auto", "files copied at once: auto (picked from the drive types) or a number")
	cmd.Flags().StringVar(&verify, "verify", "off", verifyHelp)
	cmd.Flags().Lookup("verify").NoOptDefVal = "media"
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be copied, moved, replaced and removed, without changing anything")
	addNameFlags(cmd, &onCase, &onBad, &caseSensitive)
	addCompareFlags(cmd, &compare, &noManifest)
	cmd.Flags().StringVar(&sourceList, "source-list", sourceListAuto, "where the list of files comes from: auto, index or scan (see 'drivedex copy --help')")
	cmd.Flags().StringVar(&readOrder, "read-order", copier.ReadOrderAuto, "order of small files: size, disk (experimental) or auto")
	cmd.Flags().BoolVar(&allowMassDelete, "allow-mass-delete", false, "allow a sync that removes more than half of the mirror or more than 1,000 files")
	cmd.Flags().StringVar(&detectMoves, "detect-moves", "auto", "follow files moved or renamed in the source with a rename on the destination: auto or off")
	cmd.Flags().BoolVar(&yes, "yes", false, "don't ask before removing or replacing files (they still go to the trash)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object on stdout (messages go to stderr)")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "list every affected file in the report")
	return cmd
}

// addCompareFlags adds --compare and --no-manifest (copy and sync).
func addCompareFlags(cmd *cobra.Command, compare *string, noManifest *bool) {
	cmd.Flags().StringVar(compare, "compare", copier.CompareAuto, `when a file already at the destination counts as up to date: auto (journal where it can be used, else fast), fast (same size and time; a daylight-saving hour on FAT32 is tolerated), strict (no such tolerance), journal (the copy manifest and the NTFS change journal prove the source unchanged - catches edits that kept the old time; needs Administrator), checksum (same content: reads both files)`)
	cmd.Flags().BoolVar(noManifest, "no-manifest", false, `don't keep the copy manifest in <dst>\.drivedex (journal comparison, move detection and sync deletions need it)`)
}

// checkCopyModes validates --source-list, --read-order and --compare.
func checkCopyModes(sourceList, readOrder, compare string) error {
	switch sourceList {
	case sourceListAuto, sourceListIndex, sourceListScan:
	default:
		return userErr(fmt.Sprintf("--source-list %q isn't valid", sourceList), "use auto, index or scan", nil)
	}
	if !copier.ValidReadOrder(readOrder) {
		return userErr(fmt.Sprintf("--read-order %q isn't valid", readOrder), "use auto, disk or size", nil)
	}
	if !copier.ValidCompare(compare) {
		return userErr(fmt.Sprintf("--compare %q isn't valid", compare), "use auto, fast, strict, journal or checksum", nil)
	}
	return nil
}

// syncIssues are the sync's own pre-flight findings.
func syncIssues(sp *copier.SyncPlan, allowMassDelete bool, dst string) []preflight.Issue {
	var out []preflight.Issue
	mirror := filepath.Join(dst, sp.Top)
	if sp.Adopt {
		out = append(out, preflight.Issue{Code: "sync-adopt", Severity: preflight.Info,
			Message: mirror + " already exists, but DriveDex has no record of placing files there. This first sync copies and records only, and deletes nothing; the next one mirrors deletions."})
	}
	if sp.KeptUnlisted > 0 {
		out = append(out, preflight.Issue{Code: "sync-unlisted", Severity: preflight.Info, Count: sp.KeptUnlisted,
			Message: fmt.Sprintf("%s %s not removed because %s source folder couldn't be read completely.",
				ui.Thousands(int64(sp.KeptUnlisted)), plural(sp.KeptUnlisted, "file is", "files are"), plural(sp.KeptUnlisted, "its", "their"))})
	}
	if sp.MassDelete {
		is := preflight.Issue{Code: "mass-delete", Severity: preflight.Block, Count: len(sp.Removals),
			Message: fmt.Sprintf("The sync would remove %s of the %s files it mirrors in %s.",
				ui.Thousands(int64(len(sp.Removals))), ui.Thousands(int64(sp.Mirrored)), mirror),
			Hint: "check that the right drive or card is connected; if this is what you want, add --allow-mass-delete (the files go to the trash)"}
		for _, r := range sp.Removals {
			is.Paths = append(is.Paths, r.Rel)
		}
		if allowMassDelete {
			is.Severity, is.Hint, is.Fixed = preflight.Warn, "", "allowed with --allow-mass-delete; they go to the trash"
		}
		out = append(out, is)
	}
	return out
}

// countMoves is how many files a sync would move (copy mode only reports it).
func countMoves(ctx context.Context, plan *copier.Plan, m *manifest.Manifest, srcID string) int {
	if m.Len() == 0 || plan.Top == "" || len(plan.Items) == 0 {
		return 0
	}
	sp, err := copier.PlanSync(ctx, plan, copier.SyncOptions{Manifest: m, SrcID: srcID, DetectMoves: true})
	if err != nil || sp.Adopt {
		return 0
	}
	for _, it := range sp.Moves {
		it.MoveFrom = ""
	}
	return len(sp.Moves)
}

// printSyncDryRun lists what a sync would do. Nothing is changed.
func printSyncDryRun(out *bufio.Writer, sp *copier.SyncPlan, c copier.Counts) error {
	for i := range sp.Items {
		it := &sp.Items[i]
		switch {
		case it.MoveFrom != "":
			fmt.Fprintf(out, "  %-28s %s  %s\n", "move", it.Rel, ui.Dim("(from "+it.MoveFrom+")"))
		case it.Decision.Skipped():
			continue
		default:
			fmt.Fprintf(out, "  %-28s %s  %s\n", map[bool]string{true: "copy", false: "replace"}[it.Decision == copier.Copy], it.Rel, ui.Dim("("+ui.HumanSize(it.Size)+")"))
		}
	}
	for _, r := range sp.Removals {
		fmt.Fprintf(out, "  %-28s %s  %s\n", "remove (to the trash)", r.Rel, ui.Dim("("+ui.HumanSize(r.Size)+")"))
	}
	sc := sp.SyncCounts()
	fmt.Fprintf(out, "\nWould copy %s, replace %s, move %s and remove %s files; %s already up to date. Nothing was changed (dry run).\n",
		ui.Thousands(int64(sc.Copy)), ui.Thousands(int64(sc.Replace)), ui.Thousands(int64(sc.Move)), ui.Thousands(int64(sc.Remove)), ui.Thousands(c.Skipped-int64(sc.Move)))
	return nil
}

// moved updates the manifest after a file was renamed on the destination to
// follow a move in the source. The journal bookmark is kept as it was: the
// rename shows in the journal, so journal comparison will look at the file
// again next time (INV-1).
func (b *destBook) moved(it *copier.Item, from string) {
	if b.store == nil {
		return
	}
	rec, ok := b.m.Get(from)
	b.store.Delete(from)
	if !ok {
		return
	}
	rec.Rel = it.Rel
	rec.DstSize, rec.DstMTimeNs = it.Dst.Size, it.Dst.MTimeNs
	if it.SrcFileID != 0 {
		rec.SrcFileID = it.SrcFileID
	}
	b.store.Put(rec)
}

// forget drops the record of a file removed from the mirror.
func (b *destBook) forget(rel string) {
	if b.store != nil {
		b.store.Delete(rel)
	}
}

// forgetGone drops the records of files someone deleted from the mirror.
func (b *destBook) forgetGone(sp *copier.SyncPlan) {
	for _, rel := range sp.Gone {
		b.forget(rel)
	}
}

// finishJSON prints the JSON result object of copy or sync.
func finishJSON(stdout interface{ Write([]byte) (int, error) }, js *copyJSON, err error) {
	if err != nil && js.Error == "" {
		js.Error, js.ExitCode = err.Error(), 1
		var ue *UserError
		if errors.As(err, &ue) && ue.Code != 0 {
			js.ExitCode = ue.Code
		}
	}
	printJSON(stdout, js)
}

// upperFirst capitalises the first letter of a sentence.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// quoteArg quotes a path for a command line shown to the user, if needed.
func quoteArg(s string) string {
	if strings.ContainsAny(s, " \t&()") {
		return `"` + s + `"`
	}
	return s
}
