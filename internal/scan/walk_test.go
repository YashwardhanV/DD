package scan

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/fixture"
	"github.com/yashwardhanv/dd/internal/index"
)

// genTree makes a small fixture tree and returns its root and what was created.
func genTree(t *testing.T, files int) (string, fixture.Result) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tree")
	res, err := fixture.Generate(root, fixture.Options{Files: files, Depth: 3, Fanout: 4, Seed: 7})
	if err != nil {
		t.Fatalf("generating fixture: %v", err)
	}
	return root, res
}

// collectPaths returns every relative path in an index, sorted.
func collectPaths(ix *index.Index) []string {
	cache := map[uint64]string{}
	paths := make([]string, 0, ix.Len())
	for slot := 0; slot < ix.Len(); slot++ {
		paths = append(paths, ix.RelPathCached(slot, cache))
	}
	sort.Strings(paths)
	return paths
}

func TestWalkCountsMatchFixture(t *testing.T) {
	root, res := genTree(t, 300)

	w := NewWalk(root)
	b := index.NewBuilder(index.Header{})
	if err := w.Scan(context.Background(), b, nil); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	st := w.Stats()
	if int(st.Files) != res.Files || int(st.Dirs) != res.Dirs {
		t.Errorf("scan found %d files / %d dirs, fixture made %d / %d", st.Files, st.Dirs, res.Files, res.Dirs)
	}
	if st.Errors != 0 {
		t.Errorf("unexpected errors: %v", st.FirstErrors)
	}
	if int(b.Len()) != res.Files+res.Dirs || b.Index().Len() != res.Files+res.Dirs {
		t.Errorf("builder holds %d entries, want %d", b.Len(), res.Files+res.Dirs)
	}
}

func TestWalkPathsAreRebuiltCorrectly(t *testing.T) {
	root, _ := genTree(t, 120)
	b := index.NewBuilder(index.Header{})
	if err := NewWalk(root).Scan(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	// Every path rebuilt from parent IDs must really exist on disk.
	cache := map[uint64]string{}
	ix := b.Index()
	for slot := 0; slot < ix.Len(); slot++ {
		rel := ix.RelPathCached(slot, cache)
		if strings.HasPrefix(rel, "?") {
			t.Fatalf("orphan path %q", rel)
		}
		// RelPath always uses `\`; translate to this OS's separator for the check.
		onDisk := strings.ReplaceAll(rel, `\`, string(filepath.Separator))
		if _, err := os.Lstat(filepath.Join(root, onDisk)); err != nil {
			t.Fatalf("rebuilt path %q does not exist: %v", rel, err)
		}
	}
}

func TestWalkSkipsSystemFolders(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep.txt"))
	mustWrite(t, filepath.Join(root, "System Volume Information", "secret.dat"))
	mustWrite(t, filepath.Join(root, "sub", "$RECYCLE.BIN", "x.dat"))
	mustWrite(t, filepath.Join(root, "sub", "$Recycle.Bin", "y.dat"))
	mustWrite(t, filepath.Join(root, "sub", "system volume information", "z.dat")) // case-insensitive
	mustWrite(t, filepath.Join(root, "sub", "ok.txt"))

	b := index.NewBuilder(index.Header{})
	if err := NewWalk(root).Scan(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	got := collectPaths(b.Index())
	want := []string{"keep.txt", "sub", `sub\ok.txt`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("paths = %q, want %q", got, want)
	}
}

func TestWalkFindsLongPaths(t *testing.T) {
	root, res := genTree(t, 50)
	if res.LongPathFiles < 5 {
		t.Fatalf("fixture only made %d long-path files, want >= 5", res.LongPathFiles)
	}
	b := index.NewBuilder(index.Header{})
	if err := NewWalk(root).Scan(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	long := 0
	for _, p := range collectPaths(b.Index()) {
		if len(p) > 260 && strings.HasSuffix(p, ".txt") {
			long++
		}
	}
	if long != res.LongPathFiles {
		t.Errorf("found %d files with paths over 260 chars, want %d", long, res.LongPathFiles)
	}
}

func TestWalkStopsWhenCancelled(t *testing.T) {
	root, res := genTree(t, 400)

	ctx, cancel := context.WithCancel(context.Background())
	w := NewWalk(root)
	seen := 0
	w.Visit = func(string, index.Entry) {
		if seen++; seen == 20 {
			cancel() // the user presses Ctrl+C part-way through
		}
	}
	err := w.Scan(ctx, index.NewDiscardBuilder(), nil)
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := int(w.Stats().Entries()); got >= res.Files+res.Dirs {
		t.Errorf("scan visited all %d entries despite cancellation", got)
	}
}

func TestWalkRootProblems(t *testing.T) {
	if err := NewWalk(filepath.Join(t.TempDir(), "missing")).Scan(context.Background(), index.NewDiscardBuilder(), nil); err == nil {
		t.Error("scanning a missing folder should fail")
	}
	file := filepath.Join(t.TempDir(), "f.txt")
	mustWrite(t, file)
	if err := NewWalk(file).Scan(context.Background(), index.NewDiscardBuilder(), nil); err == nil {
		t.Error("scanning a file should fail")
	}
}

func TestWalkReportsProgress(t *testing.T) {
	root, _ := genTree(t, 50)
	var last Progress
	calls := 0
	err := NewWalk(root).Scan(context.Background(), index.NewDiscardBuilder(), func(p Progress) { calls++; last = p })
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 || last.Entries == 0 {
		t.Errorf("expected at least the final progress report, got %d calls, last=%+v", calls, last)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
