package cli

import (
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

// newIndexCmd implements `drivedex index <path>`: scan once, save the result,
// and from then on `drivedex search` finds files instantly without touching
// the drive.
func newIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index <path>",
		Short: "Scan a folder or drive and save its index for instant search",
		Long: `Reads every file name on a folder or drive once and saves the list on this PC.
After that, 'drivedex search' answers in milliseconds without touching the
drive - even when the drive is unplugged. Run it again to refresh.`,
		Example: `  drivedex index D:\Projects
  drivedex index E:`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIndex(cmd.Context(), cmd.OutOrStdout(), args[0])
		},
	}
	return cmd
}

func runIndex(ctx context.Context, out io.Writer, arg string) error {
	root, err := resolveRoot(arg)
	if err != nil {
		return err
	}
	store, err := openStore()
	if err != nil {
		return err
	}

	header := index.FolderHeader(root)
	now := time.Now()
	header.CreatedAt, header.UpdatedAt = now, now
	// Refreshing keeps the original creation time.
	if old, err := index.ReadHeaderFile(store.Path(header.SourceID)); err == nil && !old.CreatedAt.IsZero() {
		header.CreatedAt = old.CreatedAt
	}

	var scanner scan.Scanner = scan.NewWalk(root)
	header.Scanner = scanner.Name()
	builder := index.NewBuilder(header)

	status := ui.NewStatusLine(os.Stderr)
	start := time.Now()
	err = scanner.Scan(ctx, builder, func(p scan.Progress) {
		status.Update(fmt.Sprintf("Indexing %s … %s entries (%s/s)",
			root, ui.Thousands(p.Entries), ui.Thousands(ui.PerSecond(p.Entries, time.Since(start)))))
	})
	status.Clear()
	elapsed := time.Since(start)

	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(out, "Stopped. Nothing was saved (an unfinished index would be misleading).")
			return nil
		}
		return userErr(fmt.Sprintf("indexing %s failed", arg), "check the drive is connected and readable", err)
	}

	ix := builder.Index()
	ix.Header.ScanMillis = elapsed.Milliseconds()
	ix.Header.UpdatedAt = time.Now()
	size, err := store.Save(ix)
	if err != nil {
		return userErr("couldn't save the index", "check there is free space on this PC and the folder is writable: "+store.Dir, err)
	}

	fmt.Fprintf(out, "Indexed %s — %s entries in %s → saved (%s)\n",
		root, ui.Thousands(int64(ix.Len())), ui.FormatDuration(elapsed), ui.HumanSize(size))
	if sp, ok := scanner.(scan.StatsProvider); ok {
		printScanErrors(out, sp.Stats())
	}
	return nil
}
