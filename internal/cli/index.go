package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/agent"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/portable"
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
		Short: "Scan a folder, drive, phone or other portable device and save its index for instant search",
		Long: `Reads every file name on a folder or drive once and saves the list on this PC.
After that, 'drivedex search' answers in milliseconds without touching the
drive - even when the drive is unplugged. Run it again to refresh.

For an Android phone use 'phone' (or 'phone:<serial>' when several are
connected). The phone lists its own files over USB debugging (ADB), which is
far faster than browsing it in Explorer.

For a phone in "File transfer" mode (no USB debugging needed), an iPhone, a
camera or any other device Explorer shows under This PC, use 'device' (or
'device:<name>' when several are connected). The background agent does this
by itself as soon as such a device is plugged in.

On an NTFS drive, run this in an Administrator terminal to use "turbo mode":
DriveDex then reads the drive's master file table directly (millions of files
in seconds) and refreshes later from its change journal in milliseconds.`,
		Example: `  drivedex index D:\Projects
  drivedex index E:
  drivedex index phone
  drivedex index device
  drivedex index "device:Galaxy S21"
  drivedex index E: --workers 8
  drivedex index E: --mode walk
  drivedex index E: --mode mft --with-metadata   (Administrator terminal)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.workers = effectiveWorkers(cmd, opts.workers)
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
	if which, ok := portable.ParseTarget(arg); ok {
		return runIndexDevice(ctx, out, which)
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
	if _, _, err := parseWorkers(opts.workers); err != nil { // reject a bad --workers before any slow work
		return err
	}
	// Only one DriveDex process may rebuild a given index at a time (the
	// background agent uses the same lock).
	return withIndexLock(header.SourceID, header.Label, func() error { return indexRootLocked(ctx, out, root, header, opts) })
}

func indexRootLocked(ctx context.Context, out io.Writer, root string, header index.Header, opts scanOptions) error {
	turbo, err := useTurbo(out, header, opts)
	if err != nil {
		return err
	}
	if turbo {
		if handled, err := indexMFT(ctx, out, root, header, opts); handled || err != nil {
			return err
		}
	}
	scanner, note, err := chooseScanner(root, opts)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, ui.Dim(note))
	return runIndexJob(ctx, out, header, scanner, root, opts.quiet)
}

// runIndexJob is the part every kind of source shares: run the scanner, save
// the index atomically, and report. display is what to call the source in messages.
func runIndexJob(ctx context.Context, out io.Writer, header index.Header, scanner scan.Scanner, display string, quiet bool) error {
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
	if quiet {
		status = ui.NewStatusLine(nil) // disabled: something else owns the screen
	}
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
		switch header.SourceKind {
		case index.KindPhone:
			printPhoneNotes(out, scanner, sp.Stats())
		case index.KindDevice:
			printDeviceNotes(out, sp.Stats())
		default:
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
	if h.SourceKind == index.KindDevice {
		return userErr(fmt.Sprintf("couldn't read the files on %s", display),
			"unlock it, and on a phone choose \"File transfer\" in the USB notification (iPhone: tap \"Trust\"), then try again", err)
	}
	return userErr(fmt.Sprintf("indexing %s failed", display), "check the drive is connected and readable", err)
}

// withIndexLock runs fn while holding the lock for one index. If another
// DriveDex process (perhaps the background agent) is already indexing the same
// source, it explains instead of starting a second scan.
func withIndexLock(sourceID, label string, fn func() error) error {
	store, err := openStore()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(store.Dir, 0o755); err != nil {
		return userErr("couldn't create the folder for saved indexes", "check the folder is writable: "+store.Dir, err)
	}
	release, err := agent.AcquireLock(store.LockPath(sourceID))
	if errors.Is(err, agent.ErrLocked) {
		name := label
		if name == "" {
			name = sourceID
		}
		return userErr(fmt.Sprintf("%s is already being indexed by another DriveDex process", name),
			"wait a moment (the background agent may be doing it) and try again; a leftover lock expires after 10 minutes", err)
	}
	if err != nil {
		return userErr("couldn't take the indexing lock", "check the index folder is writable: "+store.Dir, err)
	}
	defer release()
	return fn()
}
