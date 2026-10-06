package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

// makeTree writes a small folder with known names, sizes and dates, indexes it,
// and returns its path.
func makeTree(t *testing.T) string {
	t.Helper()
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "indexes"))
	root := filepath.Join(t.TempDir(), "tree")
	files := map[string]struct {
		size int
		age  time.Duration
	}{
		`Photos\IMG_20240101.jpg`:     {3 << 20, 400 * 24 * time.Hour},
		`Photos\IMG_20240102.jpg`:     {1 << 20, 10 * 24 * time.Hour},
		`Photos\IMG_small.png`:        {10 << 10, 2 * 24 * time.Hour},
		`Docs\Invoice 2024.pdf`:       {500 << 10, 5 * 24 * time.Hour},
		`Docs\Invoice draft 2024.pdf`: {400 << 10, 5 * 24 * time.Hour},
		`Docs\notes.txt`:              {100, 90 * 24 * time.Hour},
		`Docs\WhatsApp\chat.txt`:      {2000, time.Hour},
	}
	for rel, f := range files {
		p := filepath.Join(root, filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/")))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, f.size), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-f.age)
		os.Chtimes(p, mt, mt)
	}
	var out bytes.Buffer
	if err := indexRoot(context.Background(), &out, root, index.FolderHeader(root), scanOptions{}); err != nil {
		t.Fatalf("indexRoot: %v\n%s", err, out.String())
	}
	return root
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestSearchQuerySyntax(t *testing.T) {
	makeTree(t)
	cases := []struct {
		query string
		want  []string // base names expected (any order)
	}{
		{`invoice !draft`, []string{"Invoice 2024.pdf"}},
		{`"invoice 2024"`, []string{"Invoice 2024.pdf"}},
		{`ext:jpg,png`, []string{"IMG_20240101.jpg", "IMG_20240102.jpg", "IMG_small.png"}},
		{`type:dir photos`, []string{"Photos"}},
		{`size:>2mb`, []string{"IMG_20240101.jpg"}},
		{`size:1mb..2mb ext:jpg`, []string{"IMG_20240102.jpg"}},
		{`modified:<3d type:file`, []string{"IMG_small.png", "chat.txt"}},
		{`modified:>2000-01-01 notes`, []string{"notes.txt"}},
		{`path:whatsapp`, []string{"WhatsApp", "chat.txt"}},
		{`re:^IMG_\d{8}`, []string{"IMG_20240101.jpg", "IMG_20240102.jpg"}},
		{`img size:<100kb`, []string{"IMG_small.png"}},
	}
	for _, c := range cases {
		out, err := run(t, "--no-color", "search", "--paths-only", c.query)
		if err != nil {
			t.Errorf("%q: %v\n%s", c.query, err, out)
			continue
		}
		var got []string
		for _, l := range lines(out) {
			got = append(got, filepath.Base(strings.ReplaceAll(l, `\`, "/")))
		}
		if !sameSet(got, c.want) {
			t.Errorf("%q: got %q, want %q", c.query, got, c.want)
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestSearchSyntaxErrorsAreFriendly(t *testing.T) {
	makeTree(t)
	for _, q := range []string{"size:", "size:>big", "modified:<soon", "type:banana", "re:(", "ext:"} {
		_, err := run(t, "search", q)
		var ue *UserError
		if err == nil || !asUserError(err, &ue) || ue.Hint == "" {
			t.Errorf("%q: want a friendly error with a hint, got %v", q, err)
		}
	}
	_, err := run(t, "search", "--files", "--dirs", "x")
	if err == nil {
		t.Error("--files with --dirs should be rejected")
	}
	if _, err := run(t, "search", "!draft"); err != nil {
		t.Errorf("an exclusion on its own is a valid search: %v", err)
	}
}

func TestSearchJSONIsValidAndComplete(t *testing.T) {
	root := makeTree(t)
	out, err := run(t, "search", "ext:jpg", "size:>2mb", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var results []struct {
		Path     string  `json:"path"`
		Name     string  `json:"name"`
		Size     *int64  `json:"size"`
		Modified *string `json:"modified"`
		SourceID string  `json:"source_id"`
		Label    string  `json:"label"`
		Online   bool    `json:"online"`
	}
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	r := results[0]
	if r.Name != "IMG_20240101.jpg" || r.Size == nil || *r.Size != 3<<20 || !r.Online || r.SourceID == "" {
		t.Errorf("result = %+v", r)
	}
	if !strings.HasPrefix(r.Path, root) {
		t.Errorf("path %q should be the real path under %q", r.Path, root)
	}
	if r.Modified == nil {
		t.Error("modified should be set")
	} else if _, err := time.Parse(time.RFC3339, *r.Modified); err != nil {
		t.Errorf("modified %q isn't RFC 3339", *r.Modified)
	}
}

func TestSearchJSONWithNoResultsIsAnEmptyArrayAndNothingElse(t *testing.T) {
	makeTree(t)
	out, err := run(t, "search", "zzzznomatch", "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("err=%v out=%q, want exactly []", err, out)
	}
	// Even with no index at all, JSON mode still prints valid JSON.
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "none"))
	root := NewRootCmd("test")
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"search", "x", "--json"})
	if err := root.Execute(); err != nil || strings.TrimSpace(stdout.String()) != "[]" {
		t.Errorf("err=%v stdout=%q", err, stdout.String())
	}
	if !strings.Contains(stderr.String(), "Nothing indexed yet") {
		t.Errorf("the human message belongs on stderr, got %q", stderr.String())
	}
}

func TestSearchPathsOnlyKeepsHumanTextOut(t *testing.T) {
	makeTree(t)
	root := NewRootCmd("test")
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"search", "invoice", "--paths-only"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, l := range lines(stdout.String()) {
		if strings.Contains(l, "matches") || strings.Contains(l, "tip:") || strings.Contains(l, "  ") && strings.Contains(l, "KB") {
			t.Errorf("--paths-only printed something that isn't a path: %q", l)
		}
	}
	if len(lines(stdout.String())) != 2 {
		t.Errorf("want 2 paths, got %q", stdout.String())
	}
}

func TestSearchFlagCombinationsAreRejected(t *testing.T) {
	makeTree(t)
	for _, args := range [][]string{
		{"search", "x", "--json", "--paths-only"},
		{"search", "x", "--copy-to", "/tmp/x", "--json"},
		{"search", "x", "--copy-to", "/tmp/x", "--paths-only"},
	} {
		if _, err := run(t, args...); err == nil {
			t.Errorf("%v should be rejected", args)
		}
	}
}

func TestSearchDefaultLimitComesFromConfig(t *testing.T) {
	makeTree(t)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("DRIVEDEX_CONFIG", cfg)
	if err := os.WriteFile(cfg, []byte("default_limit = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "search", "--paths-only", "img")
	if err != nil || len(lines(out)) != 2 {
		t.Errorf("default_limit = 2 should show 2 results, got err=%v\n%s", err, out)
	}
	// An explicit --limit still wins.
	out, _ = run(t, "search", "--paths-only", "img", "--limit", "3")
	if len(lines(out)) != 3 {
		t.Errorf("--limit 3 should win over the config, got\n%s", out)
	}
	// 0 = everything.
	os.WriteFile(cfg, []byte("default_limit = 0\n"), 0o644)
	out, _ = run(t, "search", "--paths-only", "img")
	if len(lines(out)) != 3 {
		t.Errorf("default_limit = 0 should show all 3, got\n%s", out)
	}
}

func TestSearchShowHiddenSetting(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "indexes"))
	root := filepath.Join(t.TempDir(), "tree")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "visible.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, ".hidden.txt"), []byte("x"), 0o644)
	var buf bytes.Buffer
	if err := indexRoot(context.Background(), &buf, root, index.FolderHeader(root), scanOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("DRIVEDEX_CONFIG", cfg)

	os.WriteFile(cfg, []byte("show_hidden = true\n"), 0o644)
	out, _ := run(t, "search", "--paths-only", "txt")
	if len(lines(out)) != 2 {
		t.Errorf("show_hidden = true: want 2, got\n%s", out)
	}
	os.WriteFile(cfg, []byte("show_hidden = false\n"), 0o644)
	out, _ = run(t, "search", "--paths-only", "txt")
	got := lines(out)
	// (Outside Windows a leading dot marks a file hidden; on Windows it is an attribute.)
	if runtime.GOOS != "windows" && (len(got) != 1 || !strings.HasSuffix(got[0], "visible.txt")) {
		t.Errorf("show_hidden = false: want only visible.txt, got\n%s", out)
	}
}

func TestSkipDirsExtraKeepsFoldersOutOfTheIndex(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "indexes"))
	root := filepath.Join(t.TempDir(), "tree")
	os.MkdirAll(filepath.Join(root, "node_modules", "pkg"), 0o755)
	os.WriteFile(filepath.Join(root, "node_modules", "pkg", "index.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "app.js"), []byte("x"), 0o644)

	cfg := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("DRIVEDEX_CONFIG", cfg)
	os.WriteFile(cfg, []byte(`skip_dirs_extra = ["node_modules"]`+"\n"), 0o644)

	if out, err := run(t, "index", root); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, _ := run(t, "search", "--paths-only", "js")
	if got := lines(out); len(got) != 1 || !strings.HasSuffix(got[0], "app.js") {
		t.Errorf("node_modules must be skipped, got\n%s", out)
	}
}
