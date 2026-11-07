package cli

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/trash"
	"github.com/yashwardhanv/dd/internal/ui"
)

// newTrashCmd implements `drivedex trash list|restore|empty`.
func newTrashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "See, restore or empty the files a copy or sync replaced or removed",
		Long: `A copy that replaces a file, and a sync that removes one, never delete it:
the old file is moved to the destination's trash,
<dst>\.drivedex\trash\<run>\<original path>, one folder per run.
Runs older than trash_keep (default 30 days) are emptied at the next copy or
sync to that destination.`,
		Example: `  drivedex trash list D:\Backup
  drivedex trash restore D:\Backup 20261107-183000
  drivedex trash empty D:\Backup --older-than 30d`,
	}
	cmd.AddCommand(newTrashListCmd(), newTrashRestoreCmd(), newTrashEmptyCmd())
	return cmd
}

func newTrashListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <dst>",
		Short: "List the trash runs of a destination: files, size, age",
		Long: `Lists each trash run of a destination (one per copy or sync that replaced
or removed files): its name, what made it, how many files, their size and
how old it is.`,
		Example: `  drivedex trash list D:\Backup
  drivedex trash list E:\`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst, err := resolveDestination(args[0])
			if err != nil {
				return err
			}
			runs, err := trash.List(dst)
			if err != nil {
				return userErr("couldn't read the trash of "+args[0], "check the drive is connected", err)
			}
			out := cmd.OutOrStdout()
			if len(runs) == 0 {
				fmt.Fprintln(out, "The trash of "+args[0]+" is empty.")
				return nil
			}
			printTrashRuns(out, runs, time.Now())
			fmt.Fprintln(out, ui.Dim("Restore with: drivedex trash restore "+quoteArg(args[0])+" <run>   (add --path to restore one file or folder)"))
			return nil
		},
	}
}

func printTrashRuns(out io.Writer, runs []trash.RunInfo, now time.Time) {
	fmt.Fprintf(out, "%-18s %-6s %8s %10s  %s\n", "Run", "Kind", "Files", "Size", "Age")
	var files int
	var bytes int64
	for _, r := range runs {
		why := map[string]int{}
		for _, e := range r.Entries {
			why[e.Why]++
		}
		fmt.Fprintf(out, "%-18s %-6s %8s %10s  %s%s\n", r.ID, r.Kind, ui.Thousands(int64(r.Files())), ui.HumanSize(r.Bytes()),
			ui.Ago(r.Created, now), ui.Dim(whySummary(why)))
		files += r.Files()
		bytes += r.Bytes()
	}
	fmt.Fprintf(out, "%d %s, %s files, %s.\n", len(runs), plural(len(runs), "run", "runs"), ui.Thousands(int64(files)), ui.HumanSize(bytes))
}

func whySummary(why map[string]int) string {
	keys := make([]string, 0, len(why))
	for k := range why {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := ""
	for _, k := range keys {
		s += fmt.Sprintf("  %d %s", why[k], k)
	}
	return s
}

func newTrashRestoreCmd() *cobra.Command {
	var path string
	var overwrite bool
	cmd := &cobra.Command{
		Use:   "restore <dst> <run>",
		Short: "Move a run's files back to where they were",
		Long: `Moves the files of one trash run (see 'drivedex trash list') back to their
place. A file whose place has been taken since is not overwritten: it is
listed, and you are asked (in a console) whether to replace it; the file in
the way then goes to the trash itself, so nothing is lost.`,
		Example: `  drivedex trash restore D:\Backup 20261107-183000
  drivedex trash restore D:\Backup 20261107-183000 --path Photos\2024`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst, err := resolveDestination(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			in := cmd.InOrStdin()
			ask := func(rel string) bool {
				if overwrite {
					return true
				}
				if !interactive(in) {
					return false
				}
				return askYesNo(out, in, rel+" exists again. Replace it (it goes to the trash)?")
			}
			res, err := trash.Restore(dst, args[1], path, ask, time.Now())
			if err != nil {
				return userErr("couldn't restore from the trash: "+err.Error(), "see the runs with: drivedex trash list "+quoteArg(args[0]), nil)
			}
			fmt.Fprintf(out, "Restored %s %s.\n", ui.Thousands(int64(len(res.Restored))), plural(len(res.Restored), "file", "files"))
			if len(res.Existing) > 0 {
				fmt.Fprintf(out, "%s left in the trash because something is at that place now:\n", plural(len(res.Existing), "1 file was", fmt.Sprintf("%d files were", len(res.Existing))))
				for i, rel := range res.Existing {
					if i == 10 {
						fmt.Fprintf(out, "  … and %d more\n", len(res.Existing)-10)
						break
					}
					fmt.Fprintln(out, "  "+rel)
				}
				fmt.Fprintln(out, ui.Dim("Add --overwrite to replace them (the files in the way go to the trash)."))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "restore only this file or folder (relative to the destination, as 'trash list' shows it)")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace files that are in the way (they are moved to the trash first)")
	return cmd
}

func newTrashEmptyCmd() *cobra.Command {
	var olderThan string
	var yes bool
	cmd := &cobra.Command{
		Use:   "empty <dst>",
		Short: "Delete trash runs for good",
		Long: `Permanently deletes the trash runs of a destination - all of them, or with
--older-than only the older ones. This can't be undone, so it asks first
(--yes skips the question; a script without --yes is refused).`,
		Example: `  drivedex trash empty D:\Backup --older-than 30d
  drivedex trash empty D:\Backup --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dst, err := resolveDestination(args[0])
			if err != nil {
				return err
			}
			age, err := trash.ParseAge(olderThan)
			if err != nil {
				return userErr("--older-than "+err.Error(), "write it like 30d, 2w or 12h", nil)
			}
			runs, err := trash.List(dst)
			if err != nil {
				return userErr("couldn't read the trash of "+args[0], "check the drive is connected", err)
			}
			now := time.Now()
			var doomed []trash.RunInfo
			for _, r := range runs {
				if age == 0 || now.Sub(r.Created) >= age {
					doomed = append(doomed, r)
				}
			}
			out := cmd.OutOrStdout()
			if len(doomed) == 0 {
				fmt.Fprintln(out, "Nothing to empty.")
				return nil
			}
			printTrashRuns(out, doomed, now)
			if !yes {
				in := cmd.InOrStdin()
				if !interactive(in) {
					return &UserError{Msg: "emptying the trash deletes files for good, and this run can't ask first", Hint: "add --yes to confirm", Code: exitBlocked}
				}
				if !askYesNo(out, in, "Delete these files permanently? This can't be undone.") {
					fmt.Fprintln(out, "Nothing was deleted.")
					return nil
				}
			}
			gone, err := trash.Empty(dst, age, now)
			if err != nil {
				return userErr("couldn't empty the trash completely", "check the drive isn't write-protected", err)
			}
			var files int
			var bytes int64
			for _, r := range gone {
				files += r.Files()
				bytes += r.Bytes()
			}
			fmt.Fprintf(out, "Deleted %d trash %s: %s files, %s freed.\n", len(gone), plural(len(gone), "run", "runs"), ui.Thousands(int64(files)), ui.HumanSize(bytes))
			return nil
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "only runs older than this, e.g. 30d, 2w or 12h")
	cmd.Flags().BoolVar(&yes, "yes", false, "don't ask before deleting")
	return cmd
}
