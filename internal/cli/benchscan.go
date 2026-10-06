package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/scan"
	"github.com/yashwardhanv/dd/internal/ui"
)

// newBenchScanCmd implements `drivedex bench-scan <drive> --workers 1,2,4,8,16`.
func newBenchScanCmd() *cobra.Command {
	var workers string
	cmd := &cobra.Command{
		Use:   "bench-scan <drive>",
		Short: "Time scans of a drive with different numbers of parallel readers",
		Long: `Scans a drive once for each worker count you give and prints how long each
took. Use it to see how many parallel readers suit a particular drive. Nothing
is saved and the drive is only read.

Windows keeps recently read folder listings in memory, so the second and later
runs look faster than the drive really is. For honest numbers, unplug and replug
the drive between runs and compare only the first run of each setting.`,
		Example: `  drivedex bench-scan E: --workers 1,4,16
  drivedex bench-scan D:`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBenchScan(cmd.Context(), cmd.OutOrStdout(), args[0], workers)
		},
	}
	cmd.Flags().StringVar(&workers, "workers", "1,2,4,8,16", "comma-separated worker counts to try")
	return cmd
}

// parseWorkerList turns "1,2,4" into []int{1,2,4}.
func parseWorkerList(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 256 {
			return nil, userErr(fmt.Sprintf("%q isn't a valid worker count", part), "use numbers between 1 and 256, like --workers 1,4,16", err)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, userErr("--workers is empty", "use numbers like --workers 1,4,16", nil)
	}
	return out, nil
}

func runBenchScan(ctx context.Context, out io.Writer, arg, workerList string) error {
	counts, err := parseWorkerList(workerList)
	if err != nil {
		return err
	}
	root, err := resolveRoot(arg)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, ui.Yellow("Heads up:")+" Windows caches folder listings in RAM, so the 2nd and later runs look faster than the drive really is.")
	fmt.Fprintln(out, "For honest numbers, unplug and replug the drive between runs and compare only first runs.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%-8s  %-9s  %-12s  %s\n", "WORKERS", "TIME", "ENTRIES", "ENTRIES/S")

	status := ui.NewStatusLine(os.Stderr)
	for _, n := range counts {
		s := scan.NewParallel(root, n)
		start := time.Now()
		err := s.Scan(ctx, index.NewDiscardBuilder(), func(p scan.Progress) {
			status.Update(fmt.Sprintf("Scanning %s with %d workers … %s entries", root, n, ui.Thousands(p.Entries)))
		})
		status.Clear()
		elapsed := time.Since(start)
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(out, "Stopped.")
			return nil
		}
		if err != nil {
			return userErr(fmt.Sprintf("scanning %s failed", arg), "check the drive is connected and readable", err)
		}
		st := s.Stats()
		fmt.Fprintf(out, "%-8d  %-9s  %-12s  %s\n", n, ui.FormatDuration(elapsed),
			ui.Thousands(st.Entries()), ui.Thousands(ui.PerSecond(st.Entries(), elapsed)))
	}
	return nil
}
