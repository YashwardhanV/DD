package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/scan"
	"github.com/yashwardhanv/dd/internal/ui"
)

// newScanCmd implements `drivedex scan <path>`: walk a folder or drive and
// print every file. It saves nothing - it exists to show how long a plain
// "walk every shelf" scan takes, which everything else is compared against.
func newScanCmd() *cobra.Command {
	var countOnly bool
	var limit int

	cmd := &cobra.Command{
		Use:   "scan <path>",
		Short: "Walk a folder or drive and print every file (nothing is saved)",
		Long: `Walks a folder or drive one directory at a time and prints the path of every
file and folder, then a summary with the time taken. Nothing is saved - use
'drivedex index' for that. This is the slow baseline that the faster scanners
are measured against.`,
		Example: `  drivedex scan D:\Projects
  drivedex scan E:\ --count-only
  drivedex scan . --limit 20`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScan(cmd.Context(), cmd.OutOrStdout(), args[0], countOnly, limit)
		},
	}
	cmd.Flags().BoolVar(&countOnly, "count-only", false, "don't print paths, just count and show the summary")
	cmd.Flags().IntVar(&limit, "limit", 0, "print at most N paths (the whole drive is still scanned and counted)")
	return cmd
}

func runScan(ctx context.Context, stdout io.Writer, arg string, countOnly bool, limit int) error {
	root, err := resolveRoot(arg)
	if err != nil {
		return err
	}

	w := scan.NewWalk(root)
	w.SkipDirs = activeSkipDirs()
	out := bufio.NewWriterSize(stdout, 64*1024)
	defer out.Flush()

	printed := 0
	if !countOnly {
		w.Visit = func(rel string, _ index.Entry) {
			if limit > 0 && printed >= limit {
				return
			}
			fmt.Fprintln(out, rel)
			printed++
		}
	}

	// A live status line only makes sense when we are not also printing paths.
	status := ui.NewStatusLine(os.Stderr)
	start := time.Now()
	var report func(scan.Progress)
	if countOnly {
		report = func(p scan.Progress) {
			status.Update(fmt.Sprintf("Scanning %s … %s entries (%s/s)",
				root, ui.Thousands(p.Entries), ui.Thousands(ui.PerSecond(p.Entries, time.Since(start)))))
		}
	}

	err = w.Scan(ctx, index.NewDiscardBuilder(), report)
	status.Clear()
	elapsed := time.Since(start)
	st := w.Stats()

	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintf(out, "Stopped after %s — %s entries so far.\n", ui.FormatDuration(elapsed), ui.Thousands(st.Entries()))
			return nil
		}
		return userErr(fmt.Sprintf("scan of %s failed", arg), "check the drive is connected and readable", err)
	}

	fmt.Fprintf(out, "Scanned %s in %s — %s files, %s folders, %d errors (%s entries/s)\n",
		root, ui.FormatDuration(elapsed), ui.Thousands(st.Files), ui.Thousands(st.Dirs), st.Errors,
		ui.Thousands(ui.PerSecond(st.Entries(), elapsed)))
	printScanErrors(out, st)
	return nil
}

// printScanErrors shows the first few errors with a reassuring hint.
func printScanErrors(out io.Writer, st scan.Stats) {
	if st.Errors == 0 {
		return
	}
	const show = 5
	fmt.Fprintln(out, ui.Dim("Some folders could not be read (this is normal: Windows protects some system folders):"))
	for i, e := range st.FirstErrors {
		if i == show {
			break
		}
		fmt.Fprintln(out, ui.Dim("  "+e))
	}
	if more := st.Errors - int64(min(show, len(st.FirstErrors))); more > 0 {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("  … and %d more", more)))
	}
}
