package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// inProcessRunner runs "drivedex ..." engines by calling the CLI in this
// process, so the benchmark copies for real without a built .exe.
type inProcessRunner struct {
	t     *testing.T
	mu    sync.Mutex
	calls [][]string
}

func (r *inProcessRunner) Run(ctx context.Context, name string, args ...string) (int, string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if name != "drivedex.exe" {
		return 1, "not expected in the test", nil
	}
	out, err := run(r.t, args...)
	if err != nil {
		return 1, out + err.Error(), nil
	}
	return 0, out, nil
}

func useBenchFakes(t *testing.T) *inProcessRunner {
	t.Helper()
	r := &inProcessRunner{t: t}
	oldR, oldE, oldN := benchRunner, benchExe, benchNow
	benchRunner = r
	benchExe = func() (string, error) { return "drivedex.exe", nil }
	clock := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	benchNow = func() time.Time { mu.Lock(); defer mu.Unlock(); clock = clock.Add(time.Second); return clock }
	t.Cleanup(func() { benchRunner, benchExe, benchNow = oldR, oldE, oldN })
	return r
}

func TestBenchCopyRunsEnginesAndCleansUp(t *testing.T) {
	tree, _ := withFixture(t)
	r := useBenchFakes(t)
	dst := filepath.Join(t.TempDir(), "BenchTmp")
	out, err := run(t, "bench", "copy", tree, dst, "--rounds", "2", "--yes", "--markdown", "--set", "A")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"400 files", "drivedex-v1", "skipped:", "| Date | Test set |", "| A |", "warm cache", "2 rounds, median"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	var v1, v2 int
	for _, c := range r.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, " --v1") {
			v1++
		} else {
			v2++
		}
		if !strings.Contains(joined, filepath.Join(dst, ".drivedex-bench-")) {
			t.Errorf("engine wrote outside the bench root: %s", joined)
		}
	}
	if v1 != 2 || v2 != 2 {
		t.Errorf("v1 runs %d, v2 runs %d, want 2 each: %v", v1, v2, r.calls)
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Errorf("the bench folder was not deleted: %v", entries)
	}
}

func TestBenchCopyAsksFirst(t *testing.T) {
	tree, _ := withFixture(t)
	useBenchFakes(t)
	dst := filepath.Join(t.TempDir(), "BenchTmp")
	root := NewRootCmd("test")
	var b strings.Builder
	root.SetOut(&b)
	root.SetErr(&b)
	root.SetIn(strings.NewReader("n\n"))
	root.SetArgs([]string{"bench", "copy", tree, dst})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "[y/N]") || !strings.Contains(b.String(), "Nothing was written") {
		t.Errorf("output:\n%s", b.String())
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Errorf("wrote after a 'no': %v", entries)
	}
}

func TestBenchCopyRejectsBadInput(t *testing.T) {
	tree, _ := withFixture(t)
	useBenchFakes(t)
	if _, err := run(t, "bench", "copy", tree, t.TempDir(), "--engines", "teleport", "--yes"); err == nil || !strings.Contains(err.Error(), "unknown engine") {
		t.Errorf("unknown engine: %v", err)
	}
	if _, err := run(t, "bench", "copy", tree, filepath.Join(tree, "inside"), "--yes"); err == nil {
		t.Error("a destination inside the source must be refused")
	}
	if _, err := run(t, "bench", "copy", tree, t.TempDir(), "--rounds", "0"); err == nil {
		t.Error("--rounds 0 must be refused")
	}
}

func TestCopyTracePrintsTheReportAndSavesJSON(t *testing.T) {
	tree, res := withFixture(t)
	dst := filepath.Join(t.TempDir(), "out")
	tf := filepath.Join(t.TempDir(), "t.json")
	out, err := run(t, "copy", tree, dst, "--trace", "--trace-file", tf)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Where the time went", "plan: walk", "listing the source", "data (read + write)", "Per-file time: p50", "Trace saved to"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	data, err := os.ReadFile(tf)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Files   int64 `json:"files"`
		Context struct {
			PlanSource string `json:"PlanSource"`
		} `json:"context"`
	}
	if err := json.Unmarshal(data, &rep); err != nil || rep.Files != int64(res.Files) {
		t.Errorf("trace file: %v files=%d\n%s", err, rep.Files, data)
	}
	// Without --trace there is no report.
	out, _ = run(t, "copy", tree, filepath.Join(t.TempDir(), "o2"), "--v1")
	if strings.Contains(out, "Where the time went") || !strings.Contains(out, "Copied 400 files") {
		t.Errorf("plain copy:\n%s", out)
	}
}
