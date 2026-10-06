package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/phone/phonetest"
)

// Regression tests for problems found while stress testing.

// Copying a folder into one of its own sub-folders used to "work" - and then
// every re-run copied the previous copy again, one level deeper each time.
func TestCopyRefusesADestinationInsideTheSource(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	src := filepath.Join(t.TempDir(), "Photos")
	if err := os.MkdirAll(filepath.Join(src, "2024"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "2024", "a.jpg"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dst := range []string{src, filepath.Join(src, "Backup"), filepath.Join(src, "2024", "deeper")} {
		out, err := run(t, "--no-color", "copy", src, dst)
		if err == nil || !strings.Contains(err.Error(), "into itself") {
			t.Errorf("copy %s -> %s: want a refusal, got err=%v\n%s", src, dst, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(src, "Backup")); err == nil {
		t.Error("a refused copy must not create anything")
	}
	// A sibling whose name merely starts the same is fine.
	sibling := src + "Backup"
	if out, err := run(t, "--no-color", "copy", src, sibling); err != nil {
		t.Fatalf("copy to a sibling folder: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(sibling, "Photos", "2024", "a.jpg")); err != nil {
		t.Errorf("sibling copy missing: %v", err)
	}
}

// Two sources with the same label (very common: "USB DRIVE") used to share
// one destination folder in search --copy-to, so same-named files collided.
func TestSearchCopyToKeepsSameLabelSourcesApart(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	base := t.TempDir()
	a, b := filepath.Join(base, "one", "USB"), filepath.Join(base, "two", "USB")
	for dir, content := range map[string]string{a: "from one", b: "from two"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := run(t, "index", dir); err != nil {
			t.Fatalf("index %s: %v\n%s", dir, err, out)
		}
	}
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "--no-color", "search", "report", "--copy-to", dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var found []string
	filepath.Walk(dst, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			found = append(found, string(b))
		}
		return nil
	})
	if len(found) != 2 {
		t.Fatalf("want both report.txt files copied separately, found %q\n%s", found, out)
	}
	// Running it again is a no-op (the folder names are stable).
	out, err = run(t, "--no-color", "search", "report", "--copy-to", dst)
	if err != nil || strings.Contains(out, "Copied 1") {
		t.Errorf("second run should skip everything: err=%v\n%s", err, out)
	}
}

// Android allows names Windows can't store. A ':' is the nasty one: on NTFS
// "a:b.jpg" writes a hidden alternate data stream of a file called "a".
func TestPhoneCopyMakesNamesSafeForWindows(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fs := phonetest.NewFS()
	fs.Add("/sdcard/Notes/meeting 10:30.txt", "colon", 1_700_000_001)
	fs.Add("/sdcard/Notes/what?.txt", "question", 1_700_000_002)
	fs.Add("/sdcard/Notes/plain.txt", "plain", 1_700_000_003)
	fs.Add("/sdcard/Notes/con.txt", "device name", 1_700_000_004)
	useFS(t, fs)
	fakeDriveType(t, false, true)

	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "--no-color", "copy", "phone:/Notes", dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for name, want := range map[string]string{
		"meeting 10_30.txt": "colon", "what_.txt": "question", "plain.txt": "plain", "_con.txt": "device name",
	} {
		if got := string(readFile(t, dst, "Notes", name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// A second run resumes: everything is already there.
	out, err = run(t, "--no-color", "copy", "phone:/Notes", dst)
	if err != nil || !strings.Contains(out, "Everything is already there") {
		t.Errorf("re-run: err=%v\n%s", err, out)
	}
}
