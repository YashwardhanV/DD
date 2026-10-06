// Package scan reads a drive's file list into an index.Builder.
//
// There are several scanners with the same shape (the Scanner interface):
// a simple single-threaded directory walker, a parallel walker tuned for
// flash drives, an NTFS master-file-table reader, and an Android/ADB reader.
// The rest of the program doesn't care which one produced the entries.
package scan

import (
	"context"
	"strings"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

// Progress is a snapshot of a running scan, handed to the report callback.
type Progress struct {
	Entries, Dirs, Errors int64
	Current               string
}

// Scanner fills a Builder with every entry of one source.
type Scanner interface {
	// Name is "walk", "parallel", "mft" or "adb" (stored in the index header).
	Name() string
	// Scan reads the source. report may be nil; it is called at most every
	// 200 ms, plus once at the end.
	Scan(ctx context.Context, b *index.Builder, report func(Progress)) error
}

// Stats is the final tally of a scan.
type Stats struct {
	Files, Dirs, Errors int64
	// FirstErrors holds up to 20 "path: reason" strings so the user can see
	// *why* some folders were skipped without drowning in output.
	FirstErrors []string
}

// Entries is the number of files plus folders.
func (s Stats) Entries() int64 { return s.Files + s.Dirs }

// StatsProvider is implemented by scanners that can report Stats after Scan.
type StatsProvider interface {
	Stats() Stats
}

// maxRememberedErrors is how many error messages a scan keeps.
const maxRememberedErrors = 20

// DefaultSkipDirs are Windows system folders that are never worth indexing.
// Matching is case-insensitive and applies at any depth.
var DefaultSkipDirs = []string{"System Volume Information", "$RECYCLE.BIN", "$Recycle.Bin"}

// skipSet is a case-insensitive set of folder names.
type skipSet map[string]struct{}

func newSkipSet(names []string) skipSet {
	s := make(skipSet, len(names))
	for _, n := range names {
		s[strings.ToLower(n)] = struct{}{}
	}
	return s
}

func (s skipSet) has(name string) bool {
	_, ok := s[strings.ToLower(name)]
	return ok
}

// reportInterval is the minimum time between progress callbacks.
const reportInterval = 200 * time.Millisecond

// throttle calls a report function at most once per reportInterval.
type throttle struct {
	report func(Progress)
	last   time.Time
}

func newThrottle(report func(Progress)) *throttle {
	return &throttle{report: report, last: time.Now()}
}

// maybe reports p if enough time has passed since the last report.
func (t *throttle) maybe(p Progress) {
	if t.report == nil {
		return
	}
	if now := time.Now(); now.Sub(t.last) >= reportInterval {
		t.last = now
		t.report(p)
	}
}

// final always reports p (used for the last update of a scan).
func (t *throttle) final(p Progress) {
	if t.report != nil {
		t.report(p)
	}
}
