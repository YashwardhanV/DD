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
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/scan"
	"github.com/yashwardhanv/dd/internal/ui"
)

// newIndexCmd implements `drivedex index <path>`: scan once, save the result,
// and from then on `drivedex search` finds files instantly without touching
// the drive.
func newIndexCmd() *cobra.Command {
	var opts scanOptions
	cmd := &cobra.Command{
		Use:   "index <path>",
		Short: "Scan a folder, drive or Android phone and save its index for instant search",
		Long: `Reads every file name on a folder or drive once and saves the list on this PC.
After that, 'drivedex search' answers in milliseconds without touching the
drive - even when the drive is unplugged. Run it again to refresh.

For an Android phone use 'phone' (or 'phone:<serial>' when several are
connected). The phone lists its own files over USB debugging (ADB), which is
far faster than browsing it in Explorer.`,
		Example: `  drivedex index D:\Projects
  drivedex index E:
  drivedex index phone
  drivedex index E: --workers 8
  drivedex index E: --mode walk`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIndex(cmd.Context(), cmd.OutOrStdout(), args[0], opts)
		},
	}
	addScanFlags(cmd.Flags(), &opts)
	return cmd
}

func runIndex(ctx context.Context, out io.Writer, arg string, opts scanOptions) error {
	if serial, ok := phone.ParseTarget(arg); ok {
		return runIndexPhone(ctx, out, serial)
	}
	root, err := resolveRoot(arg)
	if err != nil {
		return err
	}
	header, err := headerFor(arg, root)
	if err != nil {
		return err
	}
	return indexRoot(ctx, out, root, header, opts)
}

// indexRoot scans a folder or drive and saves the result as the index
// described by header. (Split from runIndex so tests can index a plain folder
// "as if" it were a drive.)
func indexRoot(ctx context.Context, out io.Writer, root string, header index.Header, opts scanOptions) error {
	scanner, note, err := chooseScanner(root, opts)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, ui.Dim(note))
	return runIndexJob(ctx, out, header, scanner, root)
}

// runIndexJob is the part every kind of source shares: run the scanner, save
// the index atomically, and report. display is what to call the source in messages.
func runIndexJob(ctx context.Context, out io.Writer, header index.Header, scanner scan.Scanner, display string) error {
	store, err := openStore()
	if err != nil {
		return err
	}

	now := time.Now()
	header.CreatedAt, header.UpdatedAt = now, now
	// Refreshing keeps the original creation time.
	if old, err := index.ReadHeaderFile(store.Path(header.SourceID)); err == nil && !old.CreatedAt.IsZero() {
		header.CreatedAt = old.CreatedAt
	}
	header.Scanner = scanner.Name()
	builder := index.NewBuilder(header)

	status := ui.NewStatusLine(os.Stderr)
	start := time.Now()
	err = scanner.Scan(ctx, builder, func(p scan.Progress) {
		status.Update(fmt.Sprintf("Indexing %s … %s entries (%s/s)",
			display, ui.Thousands(p.Entries), ui.Thousands(ui.PerSecond(p.Entries, time.Since(start)))))
	})
	status.Clear()
	elapsed := time.Since(start)

	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(out, "Stopped. Nothing was saved (an unfinished index would be misleading).")
			return nil
		}
		return scanFailure(display, header, err)
	}

	ix := builder.Index()
	ix.Header.ScanMillis = elapsed.Milliseconds()
	ix.Header.UpdatedAt = time.Now()
	size, err := store.Save(ix)
	if err != nil {
		return userErr("couldn't save the index", "check there is free space on this PC and the folder is writable: "+store.Dir, err)
	}

	fmt.Fprintf(out, "Indexed %s — %s entries in %s → saved (%s)\n",
		display, ui.Thousands(int64(ix.Len())), ui.FormatDuration(elapsed), ui.HumanSize(size))
	if sp, ok := scanner.(scan.StatsProvider); ok {
		if header.SourceKind == index.KindPhone {
			printPhoneNotes(out, scanner, sp.Stats())
		} else {
			printScanErrors(out, sp.Stats())
		}
	}
	return nil
}

// scanFailure turns a scan error into a friendly message.
func scanFailure(display string, h index.Header, err error) error {
	if h.SourceKind == index.KindPhone {
		return userErr(fmt.Sprintf("couldn't read the files on %s", display),
			"unlock the phone, make sure USB debugging is allowed, then try again (run 'drivedex doctor' for a check-up)", err)
	}
	return userErr(fmt.Sprintf("indexing %s failed", display), "check the drive is connected and readable", err)
}
