// Package benchcopy times several copy programs ("engines") on the same
// source and destination, fairly, so "DriveDex v2 is faster" can be proven
// with numbers instead of claimed.
//
// Fairness rules (BUILD-V2 section 7):
//
//   - Every engine copies the same folder to its own fresh folder under one
//     bench root, <dst>\.drivedex-bench-<yyyymmdd-hhmmss>.
//   - The engine order ROTATES every round (A B C, B C A, C A B), so the
//     Windows cache warming up during round 1 doesn't always favour the same
//     engine.
//   - The MEDIAN of the rounds is reported, with the spread (slowest -
//     fastest), and a "win" only counts when it is bigger than the spread.
//   - drivedex-v1 (the same binary with every v2 feature off) is always
//     included, so a gain can't come from anything but the v2 features.
//
// Safety: the benchmark writes only inside the bench root it created itself
// (it refuses to start if that folder already exists) and deletes only paths
// under it - every delete checks the path prefix first.
package benchcopy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RootPrefix starts the name of every folder the benchmark creates.
const RootPrefix = ".drivedex-bench-"

// Engine is one way of copying a folder.
type Engine interface {
	// Name is the short name used on the command line and in tables
	// ("robocopy-mt", "drivedex-v1", ...).
	Name() string
	// Available returns nil if the engine can run here, or why not.
	Available() error
	// Copy copies the folder (or phone folder) src INTO dst, which exists
	// and is empty. The copy must be complete when Copy returns.
	Copy(ctx context.Context, src, dst string) error
}

// Size is what a folder holds.
type Size struct {
	Files int64
	Bytes int64
}

// Run is one timed copy.
type Run struct {
	Engine  string
	Round   int
	Elapsed time.Duration
	Got     Size  // what arrived at the destination
	Err     error // nil = success
}

// Result is the summary of one engine over all rounds.
type Result struct {
	Engine  string
	Runs    []Run
	Median  time.Duration // of the successful runs
	Spread  time.Duration // slowest - fastest successful run
	OK      int           // successful runs
	Skipped string        // why the engine didn't run ("" = it ran)
}

// FilesPerSec and MBps of the median run.
func (r Result) FilesPerSec(s Size) float64 {
	if r.Median <= 0 {
		return 0
	}
	return float64(s.Files) / r.Median.Seconds()
}

// MBps is megabytes (10^6) per second at the median time.
func (r Result) MBps(s Size) float64 {
	if r.Median <= 0 {
		return 0
	}
	return float64(s.Bytes) / 1e6 / r.Median.Seconds()
}

// Config is one benchmark.
type Config struct {
	Src     string // what to copy (a folder, or a phone folder for phone engines)
	Dst     string // the folder the bench root is created in
	Engines []Engine
	Rounds  int
	// Expected is what the source holds; every run's destination must hold
	// exactly this or the run counts as failed. Zero = don't check.
	Expected Size
	// BeforeRun is called before every run (for --cold: "unplug and replug
	// the source, press Enter"). An error stops the benchmark.
	BeforeRun func(engine string, round int) error
	// Log receives one line per run as it finishes.
	Log func(string)
	// OnRun is called with every finished run (the CLI uses it to give the
	// Explorer engine its timeout: 2x the slowest other engine so far).
	OnRun func(Run)
	// Now and the folder operations can be swapped by tests.
	Now func() time.Time
}

// Order returns the engine order for a round (0-based): the list rotated
// left by round, so each engine takes each position in turn.
func Order(n, round int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = (i + round) % n
	}
	return out
}

// ErrUnsafePath means a delete was refused because the path is not inside
// the benchmark's own root.
var ErrUnsafePath = errors.New("refusing to delete a path outside the benchmark folder")

// SafeRemove deletes path, but only if it is the bench root itself or lies
// inside it, and the root's name starts with RootPrefix. It is the only way
// the benchmark ever deletes anything.
func SafeRemove(root, path string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !strings.HasPrefix(filepath.Base(root), RootPrefix) {
		return fmt.Errorf("%w: %s is not a benchmark folder", ErrUnsafePath, root)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s is outside %s", ErrUnsafePath, path, root)
	}
	return os.RemoveAll(path)
}

// Measure walks a folder and counts its files and bytes.
func Measure(dir string) (Size, error) {
	var s Size
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			s.Files++
			s.Bytes += info.Size()
		}
		return nil
	})
	return s, err
}

// Bench runs the benchmark and returns one Result per engine (in the order
// given), plus the bench root it used (already deleted when Bench returns).
func Bench(ctx context.Context, c Config) ([]Result, string, error) {
	if c.Rounds < 1 {
		c.Rounds = 1
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	logf := func(format string, a ...any) {
		if c.Log != nil {
			c.Log(fmt.Sprintf(format, a...))
		}
	}

	results := make([]Result, len(c.Engines))
	var runnable []int
	for i, e := range c.Engines {
		results[i].Engine = e.Name()
		if err := e.Available(); err != nil {
			results[i].Skipped = err.Error()
			logf("%-12s skipped: %v", e.Name(), err)
			continue
		}
		runnable = append(runnable, i)
	}
	if len(runnable) == 0 {
		return results, "", errors.New("none of the engines can run here")
	}

	root := filepath.Join(c.Dst, RootPrefix+now().Format("20060102-150405"))
	if err := os.MkdirAll(c.Dst, 0o755); err != nil {
		return results, "", fmt.Errorf("creating %s: %w", c.Dst, err)
	}
	// Mkdir (not MkdirAll): it fails if the folder exists, so we never
	// adopt - and later delete - a folder somebody else made.
	if err := os.Mkdir(root, 0o755); err != nil {
		return results, "", fmt.Errorf("creating the benchmark folder %s: %w", root, err)
	}
	defer SafeRemove(root, root)

	for round := 0; round < c.Rounds; round++ {
		for _, k := range Order(len(runnable), round) {
			i := runnable[k]
			e := c.Engines[i]
			if err := ctx.Err(); err != nil {
				return results, root, err
			}
			if c.BeforeRun != nil {
				if err := c.BeforeRun(e.Name(), round+1); err != nil {
					return results, root, err
				}
			}
			run := runOnce(ctx, root, e, c.Src, round+1, c.Expected, now)
			results[i].Runs = append(results[i].Runs, run)
			if c.OnRun != nil {
				c.OnRun(run)
			}
			if run.Err != nil {
				logf("%-12s round %d: failed after %s: %v", e.Name(), round+1, run.Elapsed.Round(time.Millisecond), run.Err)
			} else {
				logf("%-12s round %d: %s", e.Name(), round+1, run.Elapsed.Round(time.Millisecond))
			}
			if ctx.Err() != nil {
				return results, root, ctx.Err()
			}
		}
	}
	for i := range results {
		summarize(&results[i])
	}
	return results, root, nil
}

// runOnce times one engine into a fresh folder, checks what arrived, and
// deletes the folder again.
func runOnce(ctx context.Context, root string, e Engine, src string, round int, want Size, now func() time.Time) Run {
	dir := filepath.Join(root, fmt.Sprintf("%s-r%d", e.Name(), round))
	run := Run{Engine: e.Name(), Round: round}
	if err := os.Mkdir(dir, 0o755); err != nil {
		run.Err = err
		return run
	}
	defer SafeRemove(root, dir)
	start := now()
	err := e.Copy(ctx, src, dir)
	run.Elapsed = now().Sub(start)
	if st, ok := e.(SelfTimed); ok && err == nil {
		if d := st.LastElapsed(); d > 0 {
			run.Elapsed = d // the engine measured itself more precisely (explorer)
		}
	}
	if err != nil {
		run.Err = err
		return run
	}
	got, err := Measure(dir)
	run.Got = got
	if err != nil {
		run.Err = fmt.Errorf("checking what arrived: %w", err)
		return run
	}
	if want != (Size{}) && got != want {
		run.Err = fmt.Errorf("incomplete copy: %d files / %d bytes arrived, expected %d / %d", got.Files, got.Bytes, want.Files, want.Bytes)
	}
	return run
}

// SelfTimed is an engine that measures its own copy time because the
// wall-clock time of Copy includes waiting that isn't copying (the Explorer
// engine polls for the copy to finish).
type SelfTimed interface {
	LastElapsed() time.Duration
}

// summarize fills Median, Spread and OK from the runs.
func summarize(r *Result) {
	var ok []time.Duration
	for _, run := range r.Runs {
		if run.Err == nil {
			ok = append(ok, run.Elapsed)
		}
	}
	r.OK = len(ok)
	if len(ok) == 0 {
		return
	}
	sort.Slice(ok, func(a, b int) bool { return ok[a] < ok[b] })
	r.Median = Median(ok)
	r.Spread = ok[len(ok)-1] - ok[0]
}

// Median of sorted durations (the mean of the middle two for an even count).
func Median(sorted []time.Duration) time.Duration {
	n := len(sorted)
	switch {
	case n == 0:
		return 0
	case n%2 == 1:
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// Beats reports whether a is faster than b by more than the spread of
// either (BUILD-V2 section 7: otherwise it is "no difference").
func Beats(a, b Result) bool {
	if a.OK == 0 || b.OK == 0 {
		return false
	}
	return b.Median-a.Median > max(a.Spread, b.Spread)
}
