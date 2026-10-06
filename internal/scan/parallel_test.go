package scan

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

// scanWith runs any scanner into a fresh builder.
func scanWith(t *testing.T, s Scanner, ctx context.Context) (*index.Index, error) {
	t.Helper()
	b := index.NewBuilder(index.Header{})
	err := s.Scan(ctx, b, nil)
	return b.Index(), err
}

func TestParallelMatchesWalk(t *testing.T) {
	root, res := genTree(t, 1500)

	wix, err := scanWith(t, NewWalk(root), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := collectPaths(wix)

	for _, workers := range []int{1, 4, 16} {
		p := NewParallel(root, workers)
		pix, err := scanWith(t, p, context.Background())
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		got := collectPaths(pix)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("workers=%d: path sets differ (%d vs %d paths)", workers, len(got), len(want))
		}
		st := p.Stats()
		if int(st.Files) != res.Files || int(st.Dirs) != res.Dirs || st.Errors != 0 {
			t.Errorf("workers=%d: stats %+v, fixture made %d files / %d dirs", workers, st, res.Files, res.Dirs)
		}
	}
}

// Same metadata too, not just the same names.
func TestParallelEntriesCarrySizeTimeAndFlags(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a", "file.txt"))
	mustWrite(t, filepath.Join(root, ".hidden"))
	if err := os.WriteFile(filepath.Join(root, "sized.bin"), make([]byte, 1234), 0o644); err != nil {
		t.Fatal(err)
	}
	ix, err := scanWith(t, NewParallel(root, 3), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]index.Entry{}
	for slot := 0; slot < ix.Len(); slot++ {
		found[ix.RelPath(slot)] = ix.Entry(slot)
	}
	if e := found["sized.bin"]; e.Size != 1234 || e.ModTime == 0 || e.IsDir() {
		t.Errorf("sized.bin = %+v", e)
	}
	if e := found["a"]; !e.IsDir() || e.Size != 0 {
		t.Errorf("folder a = %+v", e)
	}
	if e := found[".hidden"]; e.Flags&index.FlagHidden == 0 && runtime.GOOS != "windows" {
		t.Errorf(".hidden should be flagged hidden: %+v", e)
	}
	if found[`a\file.txt`].ParentID != found["a"].ID {
		t.Error("child must point at its parent folder's ID")
	}
}

func TestParallelSkipsSystemFolders(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep.txt"))
	mustWrite(t, filepath.Join(root, "System Volume Information", "secret.dat"))
	mustWrite(t, filepath.Join(root, "sub", "$RECYCLE.BIN", "x.dat"))
	mustWrite(t, filepath.Join(root, "sub", "$Recycle.Bin", "y.dat"))
	mustWrite(t, filepath.Join(root, "sub", "ok.txt"))
	ix, err := scanWith(t, NewParallel(root, 4), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := collectPaths(ix)
	want := []string{"keep.txt", "sub", `sub\ok.txt`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
}

func TestParallelFindsLongPaths(t *testing.T) {
	root, res := genTree(t, 60)
	ix, err := scanWith(t, NewParallel(root, 8), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	long := 0
	for _, p := range collectPaths(ix) {
		if len(p) > 260 && strings.HasSuffix(p, ".txt") {
			long++
		}
	}
	if long != res.LongPathFiles || long < 5 {
		t.Errorf("long-path files found = %d, want %d (>=5)", long, res.LongPathFiles)
	}
}

// Unreadable folders are counted and skipped; the rest of the scan carries on.
func TestParallelCountsAndSurvivesErrors(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "good", "a.txt"))
	mustWrite(t, filepath.Join(root, "locked", "secret.txt"))
	mustWrite(t, filepath.Join(root, "locked", "deeper", "x.txt"))
	mustWrite(t, filepath.Join(root, "also good", "b.txt"))

	real := readDir
	readDir = func(path string) ([]fs.DirEntry, error) {
		if filepath.Base(path) == "locked" {
			return nil, errors.New("access denied")
		}
		return real(path)
	}
	defer func() { readDir = real }()

	p := NewParallel(root, 4)
	ix, err := scanWith(t, p, context.Background())
	if err != nil {
		t.Fatalf("an unreadable sub-folder must not abort the scan: %v", err)
	}
	st := p.Stats()
	if st.Errors != 1 || len(st.FirstErrors) != 1 || !strings.Contains(st.FirstErrors[0], "access denied") {
		t.Errorf("stats = %+v", st)
	}
	got := collectPaths(ix)
	want := []string{"also good", `also good\b.txt`, "good", `good\a.txt`, "locked"} // "locked" itself is listed; its contents are not
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
}

func TestParallelRemembersOnlyFirst20Errors(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 30; i++ {
		mustWrite(t, filepath.Join(root, "d"+string(rune('a'+i%26))+string(rune('a'+i/26)), "f.txt"))
	}
	real := readDir
	readDir = func(path string) ([]fs.DirEntry, error) {
		if path != root {
			return nil, errors.New("nope")
		}
		return real(path)
	}
	defer func() { readDir = real }()
	p := NewParallel(root, 4)
	if _, err := scanWith(t, p, context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := p.Stats(); st.Errors != 30 || len(st.FirstErrors) != 20 {
		t.Errorf("errors=%d remembered=%d, want 30/20", st.Errors, len(st.FirstErrors))
	}
}

func TestParallelRootProblemsAreFatal(t *testing.T) {
	if _, err := scanWith(t, NewParallel(filepath.Join(t.TempDir(), "missing"), 2), context.Background()); err == nil {
		t.Error("missing root should fail")
	}
	file := filepath.Join(t.TempDir(), "f.txt")
	mustWrite(t, file)
	if _, err := scanWith(t, NewParallel(file, 2), context.Background()); err == nil {
		t.Error("a file as root should fail")
	}
	root := t.TempDir()
	real := readDir
	readDir = func(path string) ([]fs.DirEntry, error) {
		if path == root {
			return nil, errors.New("root unreadable")
		}
		return real(path)
	}
	defer func() { readDir = real }()
	if _, err := scanWith(t, NewParallel(root, 2), context.Background()); err == nil || !strings.Contains(err.Error(), "root unreadable") {
		t.Errorf("unreadable root should be fatal, got %v", err)
	}
}

// waitForGoroutines polls until the goroutine count returns to the baseline.
func waitForGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d running, baseline was %d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParallelPreCancelledContextStopsWithoutLeaks(t *testing.T) {
	root, _ := genTree(t, 500)
	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { _, err := scanWith(t, NewParallel(root, 8), ctx); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not stop after cancellation (deadlock?)")
	}
	waitForGoroutines(t, baseline)
}

func TestParallelCancelDuringScanTerminatesWithoutLeaks(t *testing.T) {
	root, _ := genTree(t, 4000)
	baseline := runtime.NumGoroutine()

	for _, delay := range []time.Duration{0, 200 * time.Microsecond, time.Millisecond, 3 * time.Millisecond} {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(delay, cancel)
		done := make(chan error, 1)
		go func() { _, err := scanWith(t, NewParallel(root, 8), ctx); done <- err }()
		select {
		case err := <-done:
			// Either it finished before the cancel landed, or it was cancelled. Nothing else is acceptable.
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("delay %v: unexpected error %v", delay, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("delay %v: scan did not terminate", delay)
		}
		cancel()
	}
	waitForGoroutines(t, baseline)
}

func TestParallelDoesNotDeadlockOnWideAndDeepTrees(t *testing.T) {
	// A very wide folder (more entries than channel buffer * batch) and a deep chain, with few workers.
	root := t.TempDir()
	wide := filepath.Join(root, "wide")
	os.MkdirAll(wide, 0o755)
	for i := 0; i < 600; i++ {
		os.Mkdir(filepath.Join(wide, "d"+strconv.Itoa(i)), 0o755)
	}
	deep := root
	for i := 0; i < 60; i++ {
		deep = filepath.Join(deep, "level"+strconv.Itoa(i))
	}
	os.MkdirAll(deep, 0o755)

	for _, workers := range []int{1, 2} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			p := NewParallel(root, workers)
			if _, err := scanWith(t, p, context.Background()); err != nil {
				t.Error(err)
			}
			if got := p.Stats().Dirs; got != 1+600+60 {
				t.Errorf("workers=%d: dirs = %d, want %d", workers, got, 1+600+60)
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("workers=%d: scan hung", workers)
		}
	}
}

func TestParallelReportsProgress(t *testing.T) {
	root, _ := genTree(t, 100)
	calls := 0
	var last Progress
	err := NewParallel(root, 4).Scan(context.Background(), index.NewDiscardBuilder(), func(p Progress) { calls++; last = p })
	if err != nil || calls == 0 || last.Entries == 0 {
		t.Errorf("err=%v calls=%d last=%+v", err, calls, last)
	}
}

func TestAutoWorkers(t *testing.T) {
	if n, why := AutoWorkers(true, true); n != 2 || !strings.Contains(why, "hard disk") {
		t.Errorf("HDD: %d %q", n, why)
	}
	if n, _ := AutoWorkers(false, false); n != 4 {
		t.Errorf("unknown: %d, want 4", n)
	}
	n, why := AutoWorkers(false, true)
	want := 2 * runtime.NumCPU()
	if want > 16 {
		want = 16
	}
	if n != want || !strings.Contains(why, "flash") {
		t.Errorf("flash: %d %q, want %d", n, why, want)
	}
}
