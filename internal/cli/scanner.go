package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/pflag"

	"github.com/yashwardhanv/dd/internal/scan"
	"github.com/yashwardhanv/dd/internal/volume"
)

// seekPenaltyFor asks Windows whether the drive holding a path is a spinning
// disk. A variable so tests can fake the answer.
var seekPenaltyFor = volume.SeekPenalty

// scanOptions are the --mode / --workers flags shared by commands that scan.
type scanOptions struct {
	mode         string // auto | walk | parallel | mft
	workers      string // auto | a number
	withMetadata bool   // NTFS turbo mode: also read every size and date
	full         bool   // NTFS turbo mode: skip the incremental refresh
	quiet        bool   // no live status line (used while the full-screen UI owns the terminal)
}

// addScanFlags registers --mode, --workers and the turbo-mode flags on a command.
func addScanFlags(f *pflag.FlagSet, o *scanOptions) {
	f.StringVar(&o.mode, "mode", "auto", "how to scan: auto, walk (one thread), parallel, or mft (NTFS turbo mode, Administrator)")
	f.StringVar(&o.workers, "workers", "auto", "parallel readers: auto (picked from the drive type) or a number")
	f.BoolVar(&o.withMetadata, "with-metadata", false, "NTFS turbo mode: also read every file size and date (slower, but complete)")
	f.BoolVar(&o.full, "full", false, "NTFS turbo mode: re-read the whole file table instead of refreshing from the change journal")
}

// parseWorkers turns "auto" or "8" into (n, isAuto).
func parseWorkers(s string) (n int, auto bool, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "auto" {
		return 0, true, nil
	}
	n, err = strconv.Atoi(s)
	if err != nil || n < 1 || n > 256 {
		return 0, false, userErr(fmt.Sprintf("--workers %q isn't valid", s), "use auto or a number between 1 and 256", err)
	}
	return n, false, nil
}

// chooseScanner picks the scanner for root and returns a short note telling
// the user what was chosen and why.
func chooseScanner(root string, o scanOptions) (scan.Scanner, string, error) {
	n, auto, err := parseWorkers(o.workers)
	if err != nil {
		return nil, "", err
	}
	switch strings.ToLower(strings.TrimSpace(o.mode)) {
	case "walk":
		w := scan.NewWalk(root)
		w.SkipDirs = activeSkipDirs()
		return w, "Using the single-threaded walk scanner", nil
	case "", "auto", "parallel":
		// (NTFS turbo mode "mft" is chosen before we get here; see useTurbo.)
		if !auto {
			p := scan.NewParallel(root, n)
			p.SkipDirs = activeSkipDirs()
			return p, fmt.Sprintf("Using %d workers (set with --workers)", n), nil
		}
		penalty, known := seekPenaltyFor(root)
		n, why := scan.AutoWorkers(penalty, known)
		p := scan.NewParallel(root, n)
		p.SkipDirs = activeSkipDirs()
		return p, fmt.Sprintf("Using %d workers (%s)", n, why), nil
	default:
		return nil, "", userErr(fmt.Sprintf("--mode %q isn't valid", o.mode), "use auto, walk, parallel or mft", nil)
	}
}
