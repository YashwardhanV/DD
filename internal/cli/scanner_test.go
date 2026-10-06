package cli

import (
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/scan"
)

// fakeDriveType makes chooseScanner believe the drive is an HDD / flash / unknown.
func fakeDriveType(t *testing.T, penalty, known bool) {
	t.Helper()
	old := seekPenaltyFor
	seekPenaltyFor = func(string) (bool, bool) { return penalty, known }
	t.Cleanup(func() { seekPenaltyFor = old })
}

func TestChooseScannerAutoPicksWorkersFromDriveType(t *testing.T) {
	cases := []struct {
		name            string
		penalty, known  bool
		wantNote        string
		wantWorkersNote string
	}{
		{"hdd", true, true, "Using 2 workers", "spinning hard disk"},
		{"unknown", false, false, "Using 4 workers", "drive type unknown"},
		{"flash", false, true, "Using ", "flash/SSD detected"},
	}
	for _, c := range cases {
		fakeDriveType(t, c.penalty, c.known)
		s, note, err := chooseScanner(t.TempDir(), scanOptions{mode: "auto", workers: "auto"})
		if err != nil {
			t.Fatal(err)
		}
		if s.Name() != "parallel" || !strings.Contains(note, c.wantNote) || !strings.Contains(note, c.wantWorkersNote) {
			t.Errorf("%s: scanner=%s note=%q", c.name, s.Name(), note)
		}
	}
}

func TestChooseScannerExplicitChoices(t *testing.T) {
	fakeDriveType(t, true, true) // would pick 2 workers if asked to auto-detect
	s, note, err := chooseScanner("x", scanOptions{mode: "parallel", workers: "7"})
	if err != nil || s.(*scan.Parallel).Workers != 7 || !strings.Contains(note, "7 workers (set with --workers)") {
		t.Errorf("explicit workers: %v %q %v", s, note, err)
	}
	s, _, err = chooseScanner("x", scanOptions{mode: "walk", workers: "auto"})
	if err != nil || s.Name() != "walk" {
		t.Errorf("walk: %v %v", s, err)
	}
	s, _, _ = chooseScanner("x", scanOptions{}) // zero value behaves like auto
	if s.Name() != "parallel" {
		t.Errorf("zero options should mean auto, got %s", s.Name())
	}
}

func TestChooseScannerRejectsBadFlags(t *testing.T) {
	for _, o := range []scanOptions{{mode: "turbo"}, {workers: "0"}, {workers: "-2"}, {workers: "lots"}, {workers: "9999"}} {
		if _, _, err := chooseScanner("x", o); err == nil {
			t.Errorf("%+v should be rejected", o)
		} else if ue, ok := err.(*UserError); !ok || ue.Hint == "" {
			t.Errorf("%+v: want a friendly error with a hint, got %v", o, err)
		}
	}
}

func TestIndexModeFlagsEndToEnd(t *testing.T) {
	tree, res := withFixture(t)
	fakeDriveType(t, false, true)

	for _, args := range [][]string{
		{"index", tree},
		{"index", tree, "--mode", "walk"},
		{"index", tree, "--mode", "parallel", "--workers", "3"},
		{"index", tree, "--workers", "1"},
	} {
		out, err := run(t, args...)
		if err != nil || !strings.Contains(out, "Indexed ") {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if want := fmtCount(res.Files + res.Dirs); !strings.Contains(out, want+" entries") {
			t.Errorf("%v: expected %s entries in:\n%s", args, want, out)
		}
	}
	out, _ := run(t, "index", tree, "--mode", "parallel", "--workers", "3")
	if !strings.Contains(out, "Using 3 workers (set with --workers)") {
		t.Errorf("missing worker note:\n%s", out)
	}
	// All four runs replaced the same index: still exactly one, and searchable.
	out, _ = run(t, "search", "resume")
	if !strings.Contains(out, "in 1 index") {
		t.Errorf("expected a single index:\n%s", out)
	}
	if _, err := run(t, "index", tree, "--workers", "banana"); err == nil {
		t.Error("bad --workers should fail")
	}
}

func TestBenchScan(t *testing.T) {
	tree, res := withFixture(t)
	out, err := run(t, "bench-scan", tree, "--workers", "1,4")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Heads up:", "caches folder listings", "WORKERS", "ENTRIES/S", fmtCount(res.Files + res.Dirs)} {
		if !strings.Contains(out, want) {
			t.Errorf("bench-scan output missing %q:\n%s", want, out)
		}
	}
	// One row per setting.
	if rows := strings.Count(out, fmtCount(res.Files+res.Dirs)); rows != 2 {
		t.Errorf("want 2 result rows, got %d:\n%s", rows, out)
	}
	for _, bad := range []string{"", "0", "a,b", "1,,x"} {
		if _, err := run(t, "bench-scan", tree, "--workers", bad); err == nil {
			t.Errorf("--workers %q should be rejected", bad)
		}
	}
}

func TestParseWorkerList(t *testing.T) {
	got, err := parseWorkerList(" 1, 2 ,16 ,")
	if err != nil || len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 16 {
		t.Errorf("got %v, %v", got, err)
	}
}
