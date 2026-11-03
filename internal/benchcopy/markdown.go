package benchcopy

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Describe is what the report says about the machine and drives.
type Describe struct {
	Date    time.Time
	TestSet string // e.g. "A" (5,000+ files under 1 MB), as in BUILD-V2 section 7
	Source  string // model, file system, connection and USB speed
	Dest    string
	Cold    bool // every run followed an unplug/replug of the source
	Rounds  int
}

// columns maps engines to the columns of docs/BENCHMARKS.md section 2.
var columns = []struct{ engine, title string }{
	{"explorer", "Expl."}, {"robocopy-mt", "robo MT"}, {"drivedex-v1", "v1"}, {"drivedex", "v2"},
}

// secs formats a median with its spread: "12.4 ±0.3".
func secs(r Result) string {
	if r.OK == 0 {
		if r.Skipped != "" {
			return "-"
		}
		return "failed"
	}
	return fmt.Sprintf("%.1f ±%.1f", r.Median.Seconds(), r.Spread.Seconds())
}

// WriteTable prints a plain table for the terminal: engine, median, spread,
// files/s, MB/s.
func WriteTable(w io.Writer, results []Result, s Size) {
	fmt.Fprintf(w, "%-12s %10s %9s %10s %9s  %s\n", "ENGINE", "MEDIAN", "SPREAD", "FILES/S", "MB/S", "RUNS")
	for _, r := range results {
		if r.Skipped != "" {
			fmt.Fprintf(w, "%-12s %10s %9s %10s %9s  skipped: %s\n", r.Engine, "-", "-", "-", "-", r.Skipped)
			continue
		}
		if r.OK == 0 {
			fmt.Fprintf(w, "%-12s %10s %9s %10s %9s  every run failed\n", r.Engine, "-", "-", "-", "-")
			continue
		}
		fmt.Fprintf(w, "%-12s %9.2fs %8.2fs %10.0f %9.1f  %d of %d ok\n", r.Engine, r.Median.Seconds(), r.Spread.Seconds(),
			r.FilesPerSec(s), r.MBps(s), r.OK, len(r.Runs))
	}
}

// Verdict compares drivedex (v2) with drivedex-v1 in one line.
func Verdict(results []Result) string {
	var v1, v2 *Result
	for i := range results {
		switch results[i].Engine {
		case "drivedex-v1":
			v1 = &results[i]
		case "drivedex":
			v2 = &results[i]
		}
	}
	if v1 == nil || v2 == nil || v1.OK == 0 || v2.OK == 0 {
		return ""
	}
	ratio := v1.Median.Seconds() / v2.Median.Seconds()
	switch {
	case Beats(*v2, *v1):
		return fmt.Sprintf("v2 is %.2fx as fast as v1 (the gain is bigger than the spread between rounds).", ratio)
	case Beats(*v1, *v2):
		return fmt.Sprintf("v2 is SLOWER than v1 here (%.2fx) - keep the v2 setting off for this case.", ratio)
	}
	return fmt.Sprintf("No difference between v1 and v2 (%.2fx is within the spread between rounds).", ratio)
}

// WriteMarkdown prints one row for the table in docs/BENCHMARKS.md section 2
// (times are medians in seconds, ± the spread):
//
//	| Date | Test set | Source (model, FS, port) | Destination | Files / size | Expl. | robo MT | v1 | v2 | Notes |
func WriteMarkdown(w io.Writer, d Describe, results []Result, s Size) {
	by := map[string]Result{}
	for _, r := range results {
		by[r.Engine] = r
	}
	cells := []string{
		d.Date.Format("2006-01-02"), cell(d.TestSet), cell(d.Source), cell(d.Dest),
		fmt.Sprintf("%d / %s", s.Files, sizeText(s.Bytes)),
	}
	for _, c := range columns {
		if r, ok := by[c.engine]; ok {
			cells = append(cells, secs(r))
		} else {
			cells = append(cells, "-")
		}
	}
	var notes []string
	if d.Cold {
		notes = append(notes, "cold (replugged before every run)")
	} else {
		notes = append(notes, "warm cache")
	}
	notes = append(notes, fmt.Sprintf("%d rounds, median", max(d.Rounds, 1)))
	for _, r := range results {
		known := false
		for _, c := range columns {
			known = known || c.engine == r.Engine
		}
		if !known && r.Skipped == "" {
			notes = append(notes, r.Engine+" "+secs(r))
		}
	}
	cells = append(cells, strings.Join(notes, "; "))
	fmt.Fprintln(w, "| Date | Test set | Source (model, FS, port) | Destination | Files / size | Expl. | robo MT | v1 | v2 | Notes |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|")
	fmt.Fprintln(w, "| "+strings.Join(cells, " | ")+" |")
}

func cell(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "|", "/"))
	if s == "" {
		return "-"
	}
	return s
}

func sizeText(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}
