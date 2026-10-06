package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/fixture"
	"github.com/yashwardhanv/dd/internal/ui"
)

// run executes the drivedex CLI in-process and returns what it printed.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// withFixture makes a small test tree and a private index folder, so tests
// never touch the real saved indexes.
func withFixture(t *testing.T) (tree string, res fixture.Result) {
	t.Helper()
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "indexes"))
	tree = filepath.Join(t.TempDir(), "tree")
	res, err := fixture.Generate(tree, fixture.Options{Files: 400, Depth: 3, Fanout: 4, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	return tree, res
}

func TestScanCommandSummary(t *testing.T) {
	tree, res := withFixture(t)
	out, err := run(t, "scan", tree, "--count-only")
	if err != nil {
		t.Fatal(err)
	}
	want := "400 files"
	if res.Files != 400 || !strings.Contains(out, want) {
		t.Errorf("summary missing %q (fixture made %d files):\n%s", want, res.Files, out)
	}
	if !strings.Contains(out, "0 errors") {
		t.Errorf("expected 0 errors:\n%s", out)
	}
}

func TestScanLimitPrintsFewPathsButCountsAll(t *testing.T) {
	tree, _ := withFixture(t)
	out, err := run(t, "scan", tree, "--limit", "3")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 { // 3 paths + summary
		t.Errorf("got %d lines, want 4:\n%s", len(lines), out)
	}
	if !strings.Contains(out, "400 files") {
		t.Errorf("summary should still count everything:\n%s", out)
	}
}

func TestIndexThenSearch(t *testing.T) {
	tree, _ := withFixture(t)

	out, err := run(t, "index", tree)
	if err != nil || !strings.Contains(out, "Indexed ") || !strings.Contains(out, "saved (") {
		t.Fatalf("index failed: %v\n%s", err, out)
	}

	out, err = run(t, "search", "resume")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(out), "resume.pdf") {
		t.Errorf("search resume should find Resume.pdf:\n%s", out)
	}
	if !strings.Contains(out, "matches ·") || !strings.Contains(out, "searched") || !strings.Contains(out, "in 1 index") {
		t.Errorf("missing footer:\n%s", out)
	}

	// Unicode, case-insensitive.
	out, _ = run(t, "search", "RÉSUMÉ")
	if !strings.Contains(out, "résumé.pdf") {
		t.Errorf("search RÉSUMÉ should find résumé.pdf:\n%s", out)
	}

	// --limit and --files/--dirs.
	out, _ = run(t, "search", "foto", "jpg", "--limit", "3")
	if n := strings.Count(out, ".jpg"); n != 3 {
		t.Errorf("--limit 3 printed %d results:\n%s", n, out)
	}
	out, _ = run(t, "search", "empty folder", "--dirs")
	if !strings.Contains(out, "Empty Folder") {
		t.Errorf("--dirs should find the empty folders:\n%s", out)
	}
	out, _ = run(t, "search", "empty folder", "--files")
	if !strings.Contains(out, "0 of 0 matches") {
		t.Errorf("--files must exclude folders:\n%s", out)
	}
	if _, err := run(t, "search", "x", "--files", "--dirs"); err == nil {
		t.Error("--files with --dirs should be rejected")
	}
}

func TestReindexKeepsOneIndex(t *testing.T) {
	tree, _ := withFixture(t)
	for i := 0; i < 2; i++ {
		if _, err := run(t, "index", tree); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := run(t, "search", "resume")
	if !strings.Contains(out, "in 1 index") {
		t.Errorf("re-indexing should replace, not duplicate:\n%s", out)
	}
}

func TestSearchWithNothingIndexed(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "none"))
	out, err := run(t, "search", "anything")
	if err != nil || !strings.Contains(out, "Nothing indexed yet") {
		t.Errorf("want friendly hint, got err=%v out=%q", err, out)
	}
}

func TestBadPathsGiveFriendlyErrors(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	for _, cmd := range []string{"scan", "index"} {
		_, err := run(t, cmd, filepath.Join(t.TempDir(), "does-not-exist"))
		var ue *UserError
		if err == nil || !asUserError(err, &ue) || ue.Hint == "" {
			t.Errorf("%s: want a UserError with a hint, got %v", cmd, err)
		}
	}
}

func asUserError(err error, target **UserError) bool {
	ue, ok := err.(*UserError)
	if ok {
		*target = ue
	}
	return ok
}

// fmtCount formats n the way the CLI prints it, for assertions.
func fmtCount(n int) string { return ui.Thousands(int64(n)) }
