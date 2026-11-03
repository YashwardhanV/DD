package benchcopy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeEngine "copies" by writing the expected files, and advances a fake
// clock by its fixed duration per run.
type fakeEngine struct {
	name    string
	took    []time.Duration // per call; the last value repeats
	clock   *time.Time
	calls   *[]string
	files   int
	failAt  int // 1-based call number that fails (0 = never)
	partial bool
	n       int
}

func (f *fakeEngine) Name() string     { return f.name }
func (f *fakeEngine) Available() error { return nil }
func (f *fakeEngine) Copy(ctx context.Context, src, dst string) error {
	f.n++
	*f.calls = append(*f.calls, f.name)
	d := f.took[min(f.n-1, len(f.took)-1)]
	*f.clock = f.clock.Add(d)
	if f.n == f.failAt {
		return errors.New("boom")
	}
	n := f.files
	if f.partial {
		n--
	}
	for i := 0; i < n; i++ {
		os.WriteFile(filepath.Join(dst, "f"+string(rune('a'+i))), []byte("12345"), 0o644)
	}
	return nil
}

type unavailable struct{}

func (unavailable) Name() string                               { return "nope" }
func (unavailable) Available() error                           { return errors.New("not here") }
func (unavailable) Copy(context.Context, string, string) error { return nil }

func TestOrderRotates(t *testing.T) {
	want := [][]int{{0, 1, 2}, {1, 2, 0}, {2, 0, 1}, {0, 1, 2}}
	for r, w := range want {
		got := Order(3, r)
		for i := range w {
			if got[i] != w[i] {
				t.Fatalf("round %d: %v, want %v", r, got, w)
			}
		}
	}
}

func TestMedian(t *testing.T) {
	s := func(v ...int) []time.Duration {
		out := make([]time.Duration, len(v))
		for i, x := range v {
			out[i] = time.Duration(x) * time.Second
		}
		return out
	}
	if Median(s(1, 2, 9)) != 2*time.Second || Median(s(1, 2, 4, 9)) != 3*time.Second || Median(nil) != 0 {
		t.Error("median wrong")
	}
}

func TestBenchRotatesMeasuresAndCleansUp(t *testing.T) {
	dst := t.TempDir()
	clock := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	var calls []string
	a := &fakeEngine{name: "A", took: []time.Duration{10 * time.Second, 12 * time.Second, 11 * time.Second}, clock: &clock, calls: &calls, files: 3}
	b := &fakeEngine{name: "B", took: []time.Duration{5 * time.Second}, clock: &clock, calls: &calls, files: 3}
	c := &fakeEngine{name: "C", took: []time.Duration{7 * time.Second}, clock: &clock, calls: &calls, files: 3, failAt: 2}
	var before []string
	res, root, err := Bench(context.Background(), Config{
		Src: "src", Dst: dst, Engines: []Engine{a, b, c, unavailable{}}, Rounds: 3,
		Expected:  Size{Files: 3, Bytes: 15},
		BeforeRun: func(e string, r int) error { before = append(before, e); return nil },
		Now:       func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ""); got != "ABCBCACAB" {
		t.Errorf("order %s, want ABC BCA CAB", got)
	}
	if len(before) != 9 {
		t.Errorf("BeforeRun called %d times", len(before))
	}
	if res[0].Median != 11*time.Second || res[0].Spread != 2*time.Second || res[0].OK != 3 {
		t.Errorf("A: %+v", res[0])
	}
	if res[2].OK != 2 || res[2].Median != 7*time.Second {
		t.Errorf("C (one failure): %+v", res[2])
	}
	if res[3].Skipped == "" {
		t.Error("unavailable engine not skipped")
	}
	if !strings.HasPrefix(filepath.Base(root), RootPrefix+"20261103-100000") {
		t.Errorf("root %s", root)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("bench root not deleted: %v", err)
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Errorf("destination not left as it was: %v", entries)
	}
	if !Beats(res[1], res[0]) || Beats(res[0], res[1]) {
		t.Error("B (5 s) should beat A (11 s ±2)")
	}
}

func TestIncompleteCopyIsAFailure(t *testing.T) {
	clock := time.Now()
	var calls []string
	e := &fakeEngine{name: "half", took: []time.Duration{time.Second}, clock: &clock, calls: &calls, files: 3, partial: true}
	res, _, err := Bench(context.Background(), Config{Src: "s", Dst: t.TempDir(), Engines: []Engine{e}, Rounds: 1,
		Expected: Size{Files: 3, Bytes: 15}, Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].OK != 0 || res[0].Runs[0].Err == nil || !strings.Contains(res[0].Runs[0].Err.Error(), "incomplete") {
		t.Errorf("%+v", res[0].Runs)
	}
}

func TestBenchRefusesAnExistingRoot(t *testing.T) {
	dst := t.TempDir()
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	existing := filepath.Join(dst, RootPrefix+"20260102-030405")
	os.Mkdir(existing, 0o755)
	keep := filepath.Join(existing, "somebody's file")
	os.WriteFile(keep, []byte("x"), 0o644)
	var calls []string
	e := &fakeEngine{name: "A", took: []time.Duration{time.Second}, clock: &clock, calls: &calls, files: 1}
	if _, _, err := Bench(context.Background(), Config{Src: "s", Dst: dst, Engines: []Engine{e}, Now: func() time.Time { return clock }}); err == nil {
		t.Fatal("a bench root that already exists must be refused")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("a file in a folder the bench didn't create was deleted: %v", err)
	}
}

func TestSafeRemoveRefusesPathsOutsideTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, RootPrefix+"20260101-000000")
	os.MkdirAll(filepath.Join(root, "drivedex-r1"), 0o755)
	victim := filepath.Join(base, "precious")
	os.WriteFile(victim, []byte("x"), 0o644)

	for _, bad := range []string{
		victim,
		filepath.Join(root, "..", "precious"),
		filepath.Join(root, "drivedex-r1", "..", "..", "precious"),
		base,
		"/",
	} {
		if err := SafeRemove(root, bad); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("SafeRemove(%s) = %v, want refusal", bad, err)
		}
	}
	if err := SafeRemove(base, filepath.Join(base, "precious")); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("a root without the bench prefix must be refused: %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim deleted: %v", err)
	}
	if err := SafeRemove(root, filepath.Join(root, "drivedex-r1")); err != nil {
		t.Errorf("deleting inside the root: %v", err)
	}
}

func TestRobocopyExitCodes(t *testing.T) {
	for code := -1; code <= 16; code++ {
		if got, want := RobocopyOK(code), code >= 0 && code <= 7; got != want {
			t.Errorf("RobocopyOK(%d) = %v", code, got)
		}
	}
	args := strings.Join(RobocopyArgs(`E:\Photos`, `D:\b\robocopy-mt-r1`, 16), " ")
	for _, want := range []string{`E:\Photos`, "/E", "/R:0", "/W:0", "/NFL", "/NDL", "/NJH", "/NJS", "/NP", "/MT:16"} {
		if !strings.Contains(args, want) {
			t.Errorf("robocopy args %q missing %s", args, want)
		}
	}
	if strings.Contains(strings.Join(RobocopyArgs("a", "b", 0), " "), "/MT") {
		t.Error("single-thread robocopy must not pass /MT")
	}
}

type fakeRunner struct {
	code int
	out  string
	got  []string
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (int, string, error) {
	f.got = append([]string{name}, args...)
	return f.code, f.out, nil
}

func TestEnginesUseTheirRunner(t *testing.T) {
	r := &fakeRunner{code: 3}
	if err := (Robocopy{Run: r, Threads: 16}).Copy(context.Background(), "s", "d"); err != nil {
		t.Errorf("robocopy exit 3 is success: %v", err)
	}
	r.code = 8
	if err := (Robocopy{Run: r}).Copy(context.Background(), "s", "d"); err == nil {
		t.Error("robocopy exit 8 is a failure")
	}
	r.code = 0
	d := DriveDex{Run: r, Exe: "drivedex.exe", V1: true}
	if err := d.Copy(context.Background(), "s", "d"); err != nil || d.Name() != "drivedex-v1" || !strings.Contains(strings.Join(r.got, " "), "copy s d --no-color --v1") {
		t.Errorf("drivedex-v1: %v %q", err, r.got)
	}
	r.out = "noise\nDONE 1234\n"
	e := NewExplorer(r, Size{Files: 1, Bytes: 1}, nil)
	if err := e.Copy(context.Background(), `E:\a'b`, `D:\x`); err != nil || e.LastElapsed() != 1234*time.Millisecond {
		t.Errorf("explorer: %v %v", err, e.LastElapsed())
	}
	if !strings.Contains(r.got[len(r.got)-1], `'E:\a''b'`) || !strings.Contains(r.got[len(r.got)-1], "1556") {
		t.Errorf("explorer script quoting/flags: %s", r.got[len(r.got)-1])
	}
	r.out, r.code = "TIMEOUT", 3
	if err := e.Copy(context.Background(), "s", "d"); err == nil || !strings.Contains(err.Error(), "stopwatch") {
		t.Errorf("explorer timeout: %v", err)
	}
}

func TestMarkdownRow(t *testing.T) {
	res := []Result{
		{Engine: "explorer", Skipped: "only on Windows"},
		{Engine: "robocopy-mt", Median: 20 * time.Second, Spread: time.Second, OK: 3},
		{Engine: "drivedex-v1", Median: 30 * time.Second, Spread: time.Second, OK: 3},
		{Engine: "drivedex", Median: 15 * time.Second, Spread: 2 * time.Second, OK: 3},
		{Engine: "adb-pull", Median: 40 * time.Second, Spread: 3 * time.Second, OK: 3},
	}
	var b bytes.Buffer
	WriteMarkdown(&b, Describe{Date: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), TestSet: "A", Source: "SanDisk | FAT32", Dest: "C: SSD", Cold: true, Rounds: 3},
		res, Size{Files: 5000, Bytes: 900 << 20})
	out := b.String()
	for _, want := range []string{"| 2026-11-03 | A | SanDisk / FAT32 | C: SSD | 5000 / 900 MB | - | 20.0 ±1.0 | 30.0 ±1.0 | 15.0 ±2.0 |", "cold", "adb-pull 40.0 ±3.0", "|---|"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}
	if v := Verdict(res); !strings.Contains(v, "2.00x as fast") {
		t.Errorf("verdict %q", v)
	}
	var tb bytes.Buffer
	WriteTable(&tb, res, Size{Files: 5000, Bytes: 900 << 20})
	if !strings.Contains(tb.String(), "skipped: only on Windows") || !strings.Contains(tb.String(), "333") {
		t.Errorf("table:\n%s", tb.String())
	}
}
