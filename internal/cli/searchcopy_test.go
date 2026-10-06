package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone/phonetest"
)

// runIn is run with something typed on the keyboard.
func runIn(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func readFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSearchCopyToCopiesEveryMatchingFileAndKeepsFolders(t *testing.T) {
	src := makeTree(t)
	dst := filepath.Join(t.TempDir(), "out")

	out, err := run(t, "--no-color", "search", "ext:jpg", "--copy-to", dst, "--limit", "1") // --limit must not limit the copy
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	label := sourceDirName(&index.Header{Label: filepath.Base(src), SourceID: "x"})
	for _, name := range []string{"IMG_20240101.jpg", "IMG_20240102.jpg"} {
		got := readFile(t, dst, label, "Photos", name)
		want := readFile(t, src, "Photos", name)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: copy differs from the original", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, label, "Photos", "IMG_small.png")); err == nil {
		t.Error("only the matching files may be copied")
	}
	// The source is untouched.
	if _, err := os.Stat(filepath.Join(src, "Photos", "IMG_20240101.jpg")); err != nil {
		t.Errorf("source file is gone: %v", err)
	}
}

func TestSearchCopyToSkipsFoldersAndSaysSo(t *testing.T) {
	makeTree(t)
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "--no-color", "search", "photos", "--copy-to", dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "1 matching folder was skipped: only files are copied") {
		t.Errorf("should mention the skipped folder:\n%s", out)
	}
}

func TestSearchCopyToNothingToCopy(t *testing.T) {
	makeTree(t)
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "search", "zzzznomatch", "--copy-to", dst)
	if err != nil || !strings.Contains(out, "Nothing to copy") {
		t.Errorf("err=%v\n%s", err, out)
	}
}

// More than 100 files: ask first, and "no" (or just Enter) copies nothing.
func TestSearchCopyToAsksBeyond100Files(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "indexes"))
	root := filepath.Join(t.TempDir(), "many")
	os.MkdirAll(root, 0o755)
	for i := 0; i < 120; i++ {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("report%03d.txt", i)), []byte("x"), 0o644)
	}
	var buf bytes.Buffer
	if err := indexRoot(context.Background(), &buf, root, index.FolderHeader(root), scanOptions{}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")

	out, err := runIn(t, "\n", "--no-color", "search", "report", "--copy-to", dst)
	if err != nil || !strings.Contains(out, "Copy 120 files") || !strings.Contains(out, "Cancelled. Nothing was copied.") {
		t.Fatalf("Enter must mean no: err=%v\n%s", err, out)
	}
	if _, err := os.Stat(dst); err == nil {
		entries, _ := os.ReadDir(dst)
		if len(entries) != 0 {
			t.Errorf("nothing may be copied after 'no': %v", entries)
		}
	}

	out, err = runIn(t, "y\n", "--no-color", "search", "report", "--copy-to", dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	label := sourceDirName(&index.Header{Label: filepath.Base(root), SourceID: "x"})
	entries, _ := os.ReadDir(filepath.Join(dst, label))
	if len(entries) != 120 {
		t.Errorf("after 'y' all 120 files should be copied, found %d", len(entries))
	}

	// --yes skips the question.
	dst2 := filepath.Join(t.TempDir(), "out2")
	out, err = runIn(t, "", "--no-color", "search", "report", "--copy-to", dst2, "--yes")
	if err != nil || strings.Contains(out, "[y/N]") {
		t.Errorf("--yes must not ask: err=%v\n%s", err, out)
	}
}

func TestSearchCopyToSkipsOfflineDrivesAndSaysSo(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))
	plug(t) // unplugged
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "--no-color", "search", "resume", "--copy-to", dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "aren't connected") && !strings.Contains(out, "isn't connected") && !strings.Contains(out, "on drives that aren't connected") {
		t.Errorf("should explain that offline files were skipped:\n%s", out)
	}
	if !strings.Contains(out, "SANDISK 32GB") || !strings.Contains(out, "Nothing to copy") {
		t.Errorf("should name the drive and say nothing was copied:\n%s", out)
	}
}

func TestSourceDirNameIsSafeOnWindows(t *testing.T) {
	cases := map[string]string{
		"SANDISK 32GB":        "SANDISK 32GB",
		`a<b>c:d"e/f\g|h?i*j`: "a_b_c_d_e_f_g_h_i_j",
		"  trailing. ":        "trailing",
		"":                    "vol-1",
		"...":                 "vol-1",
	}
	for label, want := range cases {
		if got := sourceDirName(&index.Header{Label: label, SourceID: "vol-1"}); got != want {
			t.Errorf("sourceDirName(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestSearchCopyToPullsPhoneFiles(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fs := phonetest.NewFS()
	fs.Add("/sdcard/DCIM/Camera/IMG_0001.jpg", "one", 1_700_000_001)
	fs.Add("/sdcard/DCIM/Camera/IMG_0002.jpg", "twotwo", 1_700_000_002)
	fs.Add("/sdcard/Download/notes.txt", "n", 1_700_000_003)
	useFS(t, fs)
	fakeDriveType(t, false, true)

	if out, err := run(t, "index", "phone"); err != nil {
		t.Fatalf("index phone: %v\n%s", err, out)
	}
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "--no-color", "search", "ext:jpg", "--copy-to", dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := string(readFile(t, dst, "Pixel 7", "DCIM", "Camera", "IMG_0002.jpg")); got != "twotwo" {
		t.Errorf("copied content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "Pixel 7", "Download", "notes.txt")); err == nil {
		t.Error("only matching files may be copied")
	}

	// Unplug the phone: its matches are skipped with an explanation, not an error.
	fs.Devices = nil
	out, err = run(t, "--no-color", "search", "ext:jpg", "--copy-to", filepath.Join(t.TempDir(), "out2"))
	if err != nil || !strings.Contains(out, "Pixel 7") || !strings.Contains(out, "Nothing to copy") {
		t.Errorf("offline phone: err=%v\n%s", err, out)
	}
}
