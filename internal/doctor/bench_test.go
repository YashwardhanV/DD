package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func small() BenchOptions { return BenchOptions{Size: 1 << 20, Chunk: 256 << 10, MinFree: 1} }

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestBenchWritesMeasuresAndLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	var progress []int64
	opt := small()
	opt.Progress = func(done, total int64) { progress = append(progress, done) }
	res, err := Bench(context.Background(), dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Bytes != 1<<20 || res.MBps <= 0 || res.Elapsed <= 0 {
		t.Errorf("result = %+v", res)
	}
	if len(progress) != 4 || progress[3] != 1<<20 {
		t.Errorf("progress = %v, want 4 chunks ending at 1 MiB", progress)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("the test file must be deleted, found %v", got)
	}
}

func TestBenchLastChunkMayBeShort(t *testing.T) {
	dir := t.TempDir()
	opt := BenchOptions{Size: 1000, Chunk: 300, MinFree: 1}
	res, err := Bench(context.Background(), dir, opt)
	if err != nil || res.Bytes != 1000 {
		t.Errorf("res=%+v err=%v", res, err)
	}
}

func TestBenchRefusesWhenNotEnoughSpaceAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	opt := small()
	opt.MinFree = 600 << 20
	opt.FreeBytes = func(string) (uint64, error) { return 100 << 20, nil }
	_, err := Bench(context.Background(), dir, opt)
	if !errors.Is(err, ErrNotEnoughSpace) || !strings.Contains(err.Error(), "100 MB free") {
		t.Errorf("err = %v", err)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("nothing may be written when space is short: %v", got)
	}
}

func TestBenchFreeSpaceErrorIsReported(t *testing.T) {
	opt := small()
	opt.FreeBytes = func(string) (uint64, error) { return 0, errors.New("boom") }
	if _, err := Bench(context.Background(), t.TempDir(), opt); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
}

// A file that is already there is never touched - not overwritten, not deleted.
func TestBenchNeverTouchesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BenchFileName)
	if err := os.WriteFile(path, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Bench(context.Background(), dir, small())
	if !errors.Is(err, ErrBenchFileExists) {
		t.Fatalf("err = %v", err)
	}
	if b, rerr := os.ReadFile(path); rerr != nil || string(b) != "precious" {
		t.Errorf("the existing file was changed: %q %v", b, rerr)
	}
}

// Cancelling (Ctrl+C) half-way still removes the file.
func TestBenchCancelledStillDeletesTheFile(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	opt := small()
	opt.Progress = func(done, total int64) {
		if done >= 512<<10 {
			cancel()
		}
	}
	_, err := Bench(ctx, dir, opt)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("file left behind after cancel: %v", got)
	}
}

func TestBenchMissingFolderFails(t *testing.T) {
	_, err := Bench(context.Background(), filepath.Join(t.TempDir(), "nope"), small())
	if err == nil || !strings.Contains(err.Error(), "creating the test file") {
		t.Errorf("err = %v", err)
	}
}

func TestInterpretBoundaries(t *testing.T) {
	cases := []struct {
		mbps  float64
		level Level
		text  string
	}{
		{5, Fail, "very slow"},
		{24.9, Fail, "very slow"},
		{25, Warn, "USB 2.0-class"},
		{45, Warn, "USB 2.0-class"},
		{60, Warn, "USB 2.0-class"},
		{60.1, OK, "good"},
		{400, OK, "good"},
	}
	for _, c := range cases {
		l := Interpret(c.mbps)
		if l.Level != c.level || !strings.Contains(l.Text, c.text) {
			t.Errorf("%.1f MB/s: %+v, want level %v containing %q", c.mbps, l, c.level, c.text)
		}
		if c.level != OK && l.Tip == "" {
			t.Errorf("%.1f MB/s: a slow result needs a tip", c.mbps)
		}
	}
	if !strings.Contains(Interpret(10).Tip, "USB 3") {
		t.Error("slow tips should point at USB 3 ports")
	}
}
