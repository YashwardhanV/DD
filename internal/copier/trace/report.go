package trace

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PhaseStat is one row of the "where the time went" table.
type PhaseStat struct {
	Phase   string        `json:"phase"`
	Total   time.Duration `json:"total_ns"`
	Share   float64       `json:"share"`    // of all measured phase time, 0..1
	Count   int64         `json:"count"`    // how many times the phase ran
	PerFile time.Duration `json:"per_file"` // Total / files copied (0 when no files)
}

// Report is the result of a traced copy. It is also what --trace-file writes
// (as JSON), so a slow copy can be attached to a bug report or compared later.
type Report struct {
	Elapsed     time.Duration `json:"elapsed_ns"`
	Files       int64         `json:"files"`
	Bytes       int64         `json:"bytes"`
	FilesPerSec float64       `json:"files_per_sec"`
	MBps        float64       `json:"mb_per_sec"`
	// TimeToFirstFile is how long after the start the first file was complete.
	TimeToFirstFile time.Duration `json:"time_to_first_file_ns"`
	// Phases has one row per phase that ran, in phase order.
	Phases []PhaseStat `json:"phases"`
	// PhaseTotal is the sum of every phase (more than Elapsed when several
	// workers ran at once).
	PhaseTotal time.Duration `json:"phase_total_ns"`
	AvgFile    time.Duration `json:"avg_file_ns"`
	P50        time.Duration `json:"p50_ns"`
	P95        time.Duration `json:"p95_ns"`
	Histogram  []int64       `json:"histogram"` // per-file time buckets, log2 of microseconds
	Context    Context       `json:"context"`
	Hints      []string      `json:"hints"`
	Notes      []string      `json:"notes,omitempty"`
}

// Report summarises everything recorded so far. elapsed is the copy's wall
// time. A nil Recorder gives an empty report.
func (r *Recorder) Report(elapsed time.Duration, ctx Context) Report {
	rep := Report{Elapsed: elapsed, Context: ctx}
	if r == nil {
		rep.Hints = []string{}
		return rep
	}
	rep.Files = r.files.Load()
	rep.Bytes = r.bytes.Load()
	if s := elapsed.Seconds(); s > 0 {
		rep.FilesPerSec = float64(rep.Files) / s
		rep.MBps = float64(rep.Bytes) / 1e6 / s
	}
	if f := r.firstFile.Load(); f > 0 {
		rep.TimeToFirstFile = time.Duration(f)
	}
	var total int64
	for p := Phase(0); p < NumPhases; p++ {
		total += r.ns[p].Load()
	}
	rep.PhaseTotal = time.Duration(total)
	for p := Phase(0); p < NumPhases; p++ {
		ns, n := r.ns[p].Load(), r.count[p].Load()
		if n == 0 {
			continue
		}
		st := PhaseStat{Phase: p.String(), Total: time.Duration(ns), Count: n}
		if total > 0 {
			st.Share = float64(ns) / float64(total)
		}
		if rep.Files > 0 {
			st.PerFile = time.Duration(ns / rep.Files)
		}
		rep.Phases = append(rep.Phases, st)
	}
	rep.Histogram = make([]int64, histBuckets)
	for i := range rep.Histogram {
		rep.Histogram[i] = r.perFile[i].Load()
	}
	rep.P50 = percentile(rep.Histogram, 0.50)
	rep.P95 = percentile(rep.Histogram, 0.95)
	if rep.Files > 0 {
		// Average time per file, from the phases that are spent on files.
		var perFile int64
		for _, p := range []Phase{SrcOpen, DstCreate, Prealloc, Data, SetTime, Close, Rename, Verify, PhoneStream, Place} {
			perFile += r.ns[p].Load()
		}
		rep.AvgFile = time.Duration(perFile / rep.Files)
	}
	r.mu.Lock()
	rep.Notes = append([]string(nil), r.notes...)
	r.mu.Unlock()
	rep.Hints = Hints(rep)
	return rep
}

// percentile reads the q-th quantile from the log2 histogram. The answer is
// the upper edge of the bucket it falls in (so p50 "9 ms" means "half the
// files took at most about 9 ms").
func percentile(hist []int64, q float64) time.Duration {
	var n int64
	for _, c := range hist {
		n += c
	}
	if n == 0 {
		return 0
	}
	want := int64(q*float64(n) + 0.999999) // ceil: the file at that rank
	if want < 1 {
		want = 1
	}
	var seen int64
	for b, c := range hist {
		seen += c
		if seen >= want {
			return bucketUpper(b)
		}
	}
	return bucketUpper(len(hist) - 1)
}

// Share returns the share (0..1) of a phase in the report, 0 if it didn't run.
func (rep Report) Share(p Phase) float64 {
	for _, s := range rep.Phases {
		if s.Phase == p.String() {
			return s.Share
		}
	}
	return 0
}

// Time returns the total time of a phase in the report.
func (rep Report) Time(p Phase) time.Duration {
	for _, s := range rep.Phases {
		if s.Phase == p.String() {
			return s.Total
		}
	}
	return 0
}

// AvgFileBytes is the average size of a copied file.
func (rep Report) AvgFileBytes() int64 {
	if rep.Files == 0 {
		return 0
	}
	return rep.Bytes / rep.Files
}

// WriteText prints the report for a person:
//
//	Where the time went            total     share   per file
//	  listing the source           41.2 s     14 %
//	  creating files (.ddxpart)    88.0 s     30 %    4.8 ms
func (rep Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "Where the time went (%d workers, plan: %s)\n", max(rep.Context.Workers, 1), orDash(rep.Context.PlanSource))
	fmt.Fprintf(w, "  %-30s %10s %7s %10s\n", "phase", "total", "share", "per file")
	for _, s := range rep.Phases {
		per := ""
		if s.PerFile > 0 && s.Phase != PlanList.String() && s.Phase != PlanDst.String() && s.Phase != PhoneList.String() {
			per = fmtDur(s.PerFile)
		}
		fmt.Fprintf(w, "  %-30s %10s %6.0f %% %10s\n", s.Phase, fmtDur(s.Total), s.Share*100, per)
	}
	if rep.Files > 0 {
		fmt.Fprintf(w, "Per-file time: p50 %s, p95 %s", fmtDur(rep.P50), fmtDur(rep.P95))
		if rep.TimeToFirstFile > 0 {
			fmt.Fprintf(w, " · first file done after %s", fmtDur(rep.TimeToFirstFile))
		}
		fmt.Fprintln(w)
	}
	if rep.PhaseTotal > rep.Elapsed && rep.Elapsed > 0 {
		fmt.Fprintln(w, "  (phase times are added up over all workers, so together they exceed the copy's own time)")
	}
	for _, n := range rep.Notes {
		fmt.Fprintln(w, "  · "+n)
	}
	if len(rep.Hints) > 0 {
		fmt.Fprintln(w, "Hints:")
		for _, h := range rep.Hints {
			fmt.Fprintln(w, "  - "+h)
		}
	}
}

// WriteJSONFile saves the report as JSON at path (written to a temporary
// name first, then renamed, so a half-written file never appears).
func (rep Report) WriteJSONFile(path string) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// ReadJSONFile loads a report written by WriteJSONFile.
func ReadJSONFile(path string) (Report, error) {
	var rep Report
	data, err := os.ReadFile(path)
	if err != nil {
		return rep, err
	}
	err = json.Unmarshal(data, &rep)
	return rep, err
}

// LastReportPath is where the CLI keeps the report of the most recent traced
// copy (doctor reads it to explain the per-file-overhead hint).
func LastReportPath(dir string) string { return filepath.Join(dir, "last-copy-trace.json") }

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// fmtDur is a short duration: "41.2 s", "4.8 ms", "870 µs".
func fmtDur(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d >= time.Second:
		return fmt.Sprintf("%.1f s", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
	case d > 0:
		return fmt.Sprintf("%d µs", d.Microseconds())
	}
	return "0"
}
