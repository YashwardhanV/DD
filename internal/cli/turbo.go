package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/ui"
)

// Hooks for the Windows-only NTFS calls, replaced by fakes in tests.
var (
	openNTFS   = ntfs.Open
	isElevated = ntfs.IsElevated
)

// adminTip is shown (once) when an NTFS drive is indexed without Administrator
// rights, and so can't use turbo mode.
const adminTip = "Tip: run in an Administrator terminal for NTFS turbo mode (often 10× faster)."

// useTurbo decides whether to index this source with the NTFS master-file-table
// reader (BUILD.md stage 6, "Mode selection"):
//
//	NTFS drive, Administrator       -> mft
//	NTFS drive, not Administrator   -> parallel walker, plus one tip
//	FAT32 / exFAT / other           -> parallel walker
//
// An explicit --mode mft that can't be honoured is an error, not a silent fallback.
func useTurbo(out io.Writer, header index.Header, opts scanOptions) (bool, error) {
	mode := strings.ToLower(strings.TrimSpace(opts.mode))
	ntfsDrive := header.SourceKind == index.KindVolume && strings.EqualFold(header.FileSystem, "NTFS")

	switch mode {
	case "mft":
		if !ntfsDrive {
			return false, userErr("NTFS turbo mode only works on whole NTFS drives",
				"index a drive such as 'drivedex index E:' (not a folder), or use --mode parallel", nil)
		}
		if !isElevated() {
			return false, userErr("NTFS turbo mode needs an Administrator terminal",
				"right-click PowerShell and choose \"Run as administrator\", then try again - or use --mode parallel", nil)
		}
		return true, nil
	case "", "auto":
		if !ntfsDrive {
			return false, nil
		}
		if isElevated() {
			return true, nil
		}
		fmt.Fprintln(out, ui.Dim(adminTip))
	}
	return false, nil
}

// indexMFT indexes an NTFS drive by reading its master file table, or - when a
// previous MFT index and a usable change journal exist - by replaying only
// what changed. It returns handled=false when turbo mode can't be used after
// all (for example the drive has no change journal) so the caller can fall back
// to the normal scanner.
func indexMFT(ctx context.Context, out io.Writer, root string, header index.Header, opts scanOptions) (handled bool, err error) {
	explicit := strings.EqualFold(strings.TrimSpace(opts.mode), "mft")
	sess, err := openNTFS(root)
	switch {
	case errors.Is(err, ntfs.ErrNeedAdmin):
		if explicit {
			return true, userErr("NTFS turbo mode needs an Administrator terminal",
				"right-click PowerShell and choose \"Run as administrator\", then try again - or use --mode parallel", err)
		}
		fmt.Fprintln(out, ui.Dim(adminTip))
		return false, nil
	case errors.Is(err, errors.ErrUnsupported):
		if explicit {
			return true, userErr("NTFS turbo mode only works on Windows", "use --mode parallel", err)
		}
		return false, nil
	case err != nil:
		if explicit {
			return true, userErr(fmt.Sprintf("couldn't open drive %s for turbo mode", root), "check the drive is connected; run with --debug for details", err)
		}
		fmt.Fprintln(out, ui.Dim("Turbo mode isn't available ("+err.Error()+"); using the normal scanner."))
		return false, nil
	}
	defer sess.Close()

	store, err := openStore()
	if err != nil {
		return true, err
	}

	sess.SkipDirs = activeSkipDirs()

	// 1. Incremental refresh: read only the changes since the last time.
	if !opts.full && !opts.withMetadata {
		if old, lerr := store.Load(header.SourceID); lerr == nil && old.Header.Scanner == "mft" {
			start := time.Now()
			res, rerr := ntfs.Refresh(ctx, sess, old)
			switch {
			case rerr == nil:
				old.Header.Label, old.Header.FileSystem = header.Label, header.FileSystem
				old.Header.LastMount, old.Header.TotalBytes = header.LastMount, header.TotalBytes
				old.Header.UpdatedAt = time.Now()
				old.Compact()
				if _, serr := store.Save(old); serr != nil {
					return true, userErr("couldn't save the refreshed index", "check there is free space and the folder is writable: "+store.Dir, serr)
				}
				fmt.Fprintf(out, "Refreshed %s from change journal — %s changes applied in %s\n",
					root, ui.Thousands(int64(res.Changes)), ui.FormatMillis(time.Since(start)))
				return true, nil
			case errors.Is(rerr, context.Canceled):
				fmt.Fprintln(out, "Stopped. Nothing was saved.")
				return true, nil
			case errors.Is(rerr, ntfs.ErrNeedFullRebuild):
				fmt.Fprintln(out, ui.Dim("Can't refresh from the change journal ("+strings.TrimPrefix(rerr.Error(), ntfs.ErrNeedFullRebuild.Error()+": ")+") - reading the whole file table instead."))
			default:
				fmt.Fprintln(out, ui.Dim("Refresh from the change journal failed ("+rerr.Error()+") - reading the whole file table instead."))
			}
		}
	}

	// 2. Full MFT scan.
	sc := &ntfs.MFTScanner{Session: sess, Root: root, WithMetadata: opts.withMetadata, SkipDirs: activeSkipDirs()}
	fmt.Fprintln(out, ui.Dim("Using NTFS turbo mode (reading the master file table)"))
	err = runIndexJob(ctx, out, header, sc, root, opts.quiet)
	switch {
	case err == nil:
		printTurboNotes(out, root, sc, opts)
		return true, nil
	case errors.Is(err, ntfs.ErrNoJournal):
		if explicit {
			return true, userErr("this drive has no NTFS change journal, which turbo mode needs",
				fmt.Sprintf("switch it on with 'drivedex journal enable %s' (Administrator), or use --mode parallel", letterOrRoot(root)), err)
		}
		fmt.Fprintf(out, "%s This drive has no NTFS change journal, so turbo mode can't read it.\n  Using the normal scanner instead. To enable turbo mode: drivedex journal enable %s\n",
			ui.Yellow("note:"), letterOrRoot(root))
		return false, nil
	}
	return true, err
}

// printTurboNotes explains anything worth knowing after an MFT index.
func printTurboNotes(out io.Writer, root string, sc *ntfs.MFTScanner, opts scanOptions) {
	if !sc.JournalActive() {
		fmt.Fprintf(out, "%s no change journal on this drive, so future refreshes will re-read everything. Switch it on: drivedex journal enable %s\n",
			ui.Yellow("note:"), letterOrRoot(root))
	}
	if opts.withMetadata {
		filled, missing := sc.MetadataFilled()
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("Sizes and dates read for %s entries (%s could not be read).", ui.Thousands(filled), ui.Thousands(missing))))
		return
	}
	fmt.Fprintln(out, ui.Dim("Turbo mode stores names only; sizes and dates are looked up when results are shown. Use --with-metadata to store them (needed for size: filters)."))
}

// letterOrRoot turns `E:\` into "E:" for messages.
func letterOrRoot(root string) string {
	if len(root) >= 2 && root[1] == ':' {
		return strings.ToUpper(root[:2])
	}
	return root
}
