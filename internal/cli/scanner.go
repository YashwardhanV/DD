package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/yashwardhanv/dd/internal/scan"
	"github.com/yashwardhanv/dd/internal/volume"
)

// seekPenaltyFor asks Windows whether the drive holding a path is a spinning
// disk. A variable so tests can fake the answer.
var seekPenaltyFor = volume.SeekPenalty

// scanOptions are the --mode / --workers flags shared by commands that scan.
type scanOptions struct {
	mode    string // auto | walk | parallel
	workers string // auto | a number
}

// addScanFlags registers --mode and --workers on a command.
func addScanFlags(f interface {
	StringVar(p *string, name string, value string, usage string)
}, o *scanOptions) {
	f.StringVar(&o.mode, "mode", "auto", "how to scan: auto, walk (one thread) or parallel")
	f.StringVar(&o.workers, "workers", "auto", "parallel readers: auto (picked from the drive type) or a number")
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
		return scan.NewWalk(root), "Using the single-threaded walk scanner", nil
	case "", "auto", "parallel":
		// auto currently means parallel; stage 6 adds NTFS "mft" turbo mode.
		if !auto {
			return scan.NewParallel(root, n), fmt.Sprintf("Using %d workers (set with --workers)", n), nil
		}
		penalty, known := seekPenaltyFor(root)
		n, why := scan.AutoWorkers(penalty, known)
		return scan.NewParallel(root, n), fmt.Sprintf("Using %d workers (%s)", n, why), nil
	default:
		return nil, "", userErr(fmt.Sprintf("--mode %q isn't valid", o.mode), "use auto, walk or parallel", nil)
	}
}
