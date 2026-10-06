package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/phone/phonetest"
)

// fakeChecks makes the free-space / file-system queries answer as given.
func fakeChecks(t *testing.T, free uint64, fsName string) {
	t.Helper()
	old := copyChecks
	copyChecks = func() copier.Checks {
		return copier.Checks{
			FreeBytes:  func(string) (uint64, error) { return free, nil },
			FileSystem: func(string) (string, error) { return fsName, nil },
		}
	}
	t.Cleanup(func() { copyChecks = old })
}

func countFilesUnder(t *testing.T, root string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(root, func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func TestCopyDryRunWritesNothing(t *testing.T) {
	tree, res := withFixture(t)
	dst := filepath.Join(t.TempDir(), "never-created")

	out, err := run(t, "copy", tree, dst, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Copying 400 files", "with ", " workers…", "Would copy 400 files", "Nothing was written (dry run)", "  copy "} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output missing %q:\n%.600s", want, out)
		}
	}
	if strings.Count(out, "\n  copy ") != res.Files {
		t.Errorf("expected one 'copy' line per file")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a dry run must not even create the destination folder")
	}
}

func TestCopyThenRerunSkipsEverything(t *testing.T) {
	tree, res := withFixture(t)
	dst := filepath.Join(t.TempDir(), "backup")

	out, err := run(t, "copy", tree, dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Copying 400 files") || !strings.Contains(out, "Copied 400 files") || !strings.Contains(out, "0 skipped · 0 failed") {
		t.Errorf("first run:\n%s", out)
	}
	if got := countFilesUnder(t, filepath.Join(dst, filepath.Base(tree))); got != res.Files {
		t.Errorf("destination has %d files, want %d", got, res.Files)
	}
	if !strings.Contains(out, "Safely Remove") {
		t.Errorf("missing the safe-remove reminder:\n%s", out)
	}

	out, err = run(t, "copy", tree, dst)
	if err != nil || !strings.Contains(out, "400 files") || !strings.Contains(out, "400 already up to date") || !strings.Contains(out, "Everything is already there") {
		t.Errorf("second run should skip everything: %v\n%s", err, out)
	}

	// Delete a few copied files: only those come back.
	var gone int
	filepath.WalkDir(filepath.Join(dst, filepath.Base(tree)), func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() && gone < 3 && strings.HasSuffix(p, ".txt") {
			os.Remove(p)
			gone++
		}
		return nil
	})
	out, _ = run(t, "copy", tree, dst)
	if !strings.Contains(out, "Copied 3 files") || !strings.Contains(out, "397 skipped") {
		t.Errorf("resume run:\n%s", out)
	}
}

func TestCopyDryRunShowsOverwritesAndSkips(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	os.WriteFile(filepath.Join(src, "same.txt"), []byte("aaaa"), 0o644)
	os.WriteFile(filepath.Join(src, "new.txt"), []byte("bbbb"), 0o644)
	if _, err := run(t, "copy", src, dst); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(src, "new.txt"), []byte("bbbb changed and longer"), 0o644)
	os.WriteFile(filepath.Join(src, "fresh.txt"), []byte("cccc"), 0o644)

	out, _ := run(t, "copy", src, dst, "--dry-run", "--on-conflict", "overwrite")
	for _, want := range []string{"overwrite", "skip (already there)", "  copy ", "Would copy 1 files", "overwrite 1", "skip 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	out, _ = run(t, "copy", src, dst, "--dry-run", "--on-conflict", "skip")
	if !strings.Contains(out, "skip (exists)") {
		t.Errorf("--on-conflict skip:\n%s", out)
	}
}

func TestCopyFlagValidation(t *testing.T) {
	tree, _ := withFixture(t)
	dst := t.TempDir()
	for _, args := range [][]string{
		{"copy", tree, dst, "--on-conflict", "merge"},
		{"copy", tree, dst, "--workers", "zero"},
		{"copy", tree, dst, "--workers", "0"},
	} {
		_, err := run(t, args...)
		var ue *UserError
		if !errors.As(err, &ue) || ue.Hint == "" {
			t.Errorf("%v: want a friendly error with a hint, got %v", args, err)
		}
	}
	// A destination that is an existing FILE is refused.
	file := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	if _, err := run(t, "copy", tree, file); err == nil || !strings.Contains(err.Error(), "is a file") {
		t.Errorf("file destination: %v", err)
	}
	// Missing source.
	if _, err := run(t, "copy", filepath.Join(t.TempDir(), "missing"), dst); err == nil || !strings.Contains(err.Error(), "can't open") {
		t.Errorf("missing source: %v", err)
	}
	// Never write to a phone.
	if _, err := run(t, "copy", tree, "phone:/DCIM"); err == nil || !strings.Contains(err.Error(), "never writes to a phone") {
		t.Errorf("phone destination: %v", err)
	}
}

func TestCopyWorkerChoice(t *testing.T) {
	tree, _ := withFixture(t)
	cases := []struct {
		penalty, known bool
		args           []string
		want           string
	}{
		{true, true, nil, "(2 workers: a spinning hard disk is involved)"},
		{false, true, nil, "(8 workers: both ends are flash/SSD)"},
		{false, false, nil, "(4 workers: drive type unknown)"},
		{true, true, []string{"--workers", "5"}, "(5 workers: set with --workers)"},
	}
	for _, c := range cases {
		fakeDriveType(t, c.penalty, c.known)
		out, err := run(t, append([]string{"copy", tree, t.TempDir()}, c.args...)...)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%v: want %q: %v\n%.400s", c.args, c.want, err, out)
		}
	}
}

func TestCopyRefusesWhenThereIsNotEnoughSpace(t *testing.T) {
	tree, _ := withFixture(t)
	fakeChecks(t, 1000, "NTFS") // 1,000 bytes free
	dst := filepath.Join(t.TempDir(), "d")
	_, err := run(t, "copy", tree, dst)
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "enough free space") || !errors.Is(err, copier.ErrNotEnoughSpace) {
		t.Fatalf("got %v", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("nothing may be created when the pre-check fails")
	}
}

func TestCopyRefusesBigFilesOnFAT32(t *testing.T) {
	src := t.TempDir()
	// A sparse 4 GiB file: instant to create and costs no disk space.
	big := filepath.Join(src, "movie.mkv")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4 << 30); err != nil {
		f.Close()
		t.Skipf("can't create a sparse 4 GiB file here: %v", err)
	}
	f.Close()
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())

	fakeChecks(t, 1<<60, "FAT32")
	_, err = run(t, "copy", src, t.TempDir())
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "FAT32") || !strings.Contains(ue.Hint, "exFAT") {
		t.Errorf("FAT32: %v", err)
	}
	// Same file, exFAT destination: fine (dry-run to avoid copying 4 GiB).
	fakeChecks(t, 1<<60, "exFAT")
	if out, err := run(t, "copy", src, t.TempDir(), "--dry-run"); err != nil || !strings.Contains(out, "movie.mkv") {
		t.Errorf("exFAT: %v\n%s", err, out)
	}
}

func TestCopyFailuresAreListedAndExitNonZero(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "good.txt"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(src, "blocked.txt"), []byte("2"), 0o644)
	dst := t.TempDir()
	os.MkdirAll(filepath.Join(dst, filepath.Base(src), "blocked.txt"), 0o755) // a folder is in the way

	out, err := run(t, "copy", src, dst)
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "1 file could not be copied") || !strings.Contains(ue.Hint, "run the same command again") {
		t.Fatalf("want a non-zero exit with a hint, got %v", err)
	}
	for _, want := range []string{"Copied 1 files", "1 failed", "These files could not be copied:", "blocked.txt"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestCopyStoppedByCtrlC(t *testing.T) {
	tree, _ := withFixture(t)
	dst := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	// The pre-check runs after planning and before copying: cancel there.
	old := copyChecks
	copyChecks = func() copier.Checks { cancel(); return copier.Checks{} }
	defer func() { copyChecks = old }()

	root := NewRootCmd("test")
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"copy", tree, dst})
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("Ctrl+C is not an error: %v", err)
	}
	if !strings.Contains(out.String(), "Stopped. Run the same command again to resume.") {
		t.Errorf("output:\n%s", out.String())
	}
	// Cancelled before planning finished: a different message, still not an error.
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	root = NewRootCmd("test")
	out.Reset()
	root.SetOut(&out)
	root.SetArgs([]string{"copy", tree, t.TempDir()})
	if err := root.ExecuteContext(cancelled); err != nil || !strings.Contains(out.String(), "Stopped. Nothing was copied.") {
		t.Errorf("err=%v out=%s", err, out.String())
	}
}

func TestCopyVerifyReportsSuccess(t *testing.T) {
	tree, _ := withFixture(t)
	out, err := run(t, "copy", tree, t.TempDir(), "--verify")
	if err != nil || !strings.Contains(out, "Verified:") {
		t.Errorf("%v\n%s", err, out)
	}
}

func TestCopySingleFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "report.pdf")
	os.WriteFile(src, []byte("PDF"), 0o644)
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "copy", src, dst)
	if err != nil || !strings.Contains(out, "Copied 1 files") {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "report.pdf")); string(got) != "PDF" {
		t.Errorf("content %q", got)
	}
}

func TestCopyEmptySource(t *testing.T) {
	out, err := run(t, "copy", t.TempDir(), t.TempDir())
	if err != nil || !strings.Contains(out, "Nothing to copy") {
		t.Errorf("%v\n%s", err, out)
	}
}

// ---- phones ------------------------------------------------------------------------

func useFS(t *testing.T, fs *phonetest.FS) {
	t.Helper()
	old := openADB
	openADB = func() (*phone.ADB, error) { return fs.ADB(), nil }
	t.Cleanup(func() { openADB = old })
}

func TestParsePhoneSource(t *testing.T) {
	cases := []struct {
		in     string
		isPh   bool
		serial string
		path   string
		bad    bool
	}{
		{"phone:/DCIM/Camera", true, "", "/DCIM/Camera", false},
		{"PHONE:/DCIM", true, "", "/DCIM", false},
		{`phone:\DCIM\Camera`, true, "", `\DCIM\Camera`, false},
		{"phone:R58M1234ABC:/DCIM/Camera", true, "R58M1234ABC", "/DCIM/Camera", false},
		{"phone:192.168.1.5:5555:/DCIM", true, "192.168.1.5:5555", "/DCIM", false},
		{"phone:DCIM/Camera", true, "", "", true}, // ambiguous: needs a leading slash
		{"phone:", true, "", "", true},
		{"phone::/x", true, "", "", true},
		{`E:\Photos`, false, "", "", false},
		{"phonebook", false, "", "", false},
	}
	for _, c := range cases {
		spec, isPh, err := parsePhoneSource(c.in)
		if isPh != c.isPh || (err != nil) != c.bad || spec.serial != c.serial || spec.path != c.path {
			t.Errorf("%q: got %+v isPhone=%v err=%v", c.in, spec, isPh, err)
		}
	}
}

func TestCopyFromPhone(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fs := phonetest.NewFS()
	for i := 0; i < 120; i++ {
		fs.Add("/sdcard/DCIM/Camera/IMG_"+strings.Repeat("0", 3)+string(rune('A'+i%26))+string(rune('a'+i/26))+".jpg", strings.Repeat("x", 20+i), 1_700_000_000+int64(i))
	}
	useFS(t, fs)
	fakeDriveType(t, false, true)
	dst := filepath.Join(t.TempDir(), "PhoneBackup")

	out, err := run(t, "copy", "phone:/DCIM/Camera", dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Copying 120 files") || !strings.Contains(out, "Copied 120 files") {
		t.Errorf("output:\n%s", out)
	}
	if got := countFilesUnder(t, filepath.Join(dst, "Camera")); got != 120 {
		t.Errorf("copied %d files", got)
	}
	if fs.PullCount() != 2 {
		t.Errorf("120 files in one folder = 2 pulls (100 + 20), got %d", fs.PullCount())
	}

	// Resume after "unplug/replug": run again, nothing is pulled.
	before := fs.PullCount()
	out, err = run(t, "copy", "phone:/DCIM/Camera", dst)
	if err != nil || !strings.Contains(out, "Everything is already there") || fs.PullCount() != before {
		t.Errorf("resume: %v\n%s", err, out)
	}
}

func TestCopyFromPhoneWithSerialAndSingleFile(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fs := phonetest.NewFS()
	fs.Devices = []string{phonetest.Pixel, "ZX1G22ABCD  device usb:1-3 product:starlte model:Galaxy_S9 device:starlte transport_id:7"}
	fs.Add("/sdcard/Download/report.pdf", "PDFDATA", 1_700_000_000)
	useFS(t, fs)

	// Two phones, no serial: refuse to guess.
	if _, err := run(t, "copy", "phone:/Download/report.pdf", t.TempDir()); err == nil || !strings.Contains(err.Error(), "several phones") {
		t.Errorf("want 'several phones', got %v", err)
	}
	dst := t.TempDir()
	out, err := run(t, "copy", "phone:R58M1234ABC:/Download/report.pdf", dst)
	if err != nil || !strings.Contains(out, "Copied 1 files") {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "report.pdf")); string(got) != "PDFDATA" {
		t.Errorf("content %q", got)
	}
}

func TestCopyFromPhoneErrors(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fs := phonetest.NewFS()
	fs.Add("/sdcard/a.txt", "x", 1)
	useFS(t, fs)

	_, err := run(t, "copy", "phone:/Nope", t.TempDir())
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "doesn't exist on the phone") || !strings.Contains(ue.Hint, "phone:/DCIM/Camera") {
		t.Errorf("missing path: %v", err)
	}
	if _, err := run(t, "copy", "phone:DCIM", t.TempDir()); err == nil || !strings.Contains(err.Error(), "can't understand the phone path") {
		t.Errorf("bad syntax: %v", err)
	}
	fs.Devices = nil
	if _, err := run(t, "copy", "phone:/a.txt", t.TempDir()); err == nil || !strings.Contains(err.Error(), "no Android phone") {
		t.Errorf("no phone: %v", err)
	}
	old := openADB
	openADB = func() (*phone.ADB, error) { return nil, adbNotFound(phone.ErrADBNotFound) }
	defer func() { openADB = old }()
	if _, err := run(t, "copy", "phone:/a.txt", t.TempDir()); err == nil || !strings.Contains(err.Error(), "adb") {
		t.Errorf("no adb: %v", err)
	}
}

func TestCopyVerifyIsNotAvailableForPhones(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fs := phonetest.NewFS()
	fs.Add("/sdcard/a.txt", "x", 1)
	useFS(t, fs)
	out, err := run(t, "copy", "phone:/a.txt", t.TempDir(), "--verify")
	if err != nil || !strings.Contains(out, "--verify isn't available for phone copies") {
		t.Errorf("%v\n%s", err, out)
	}
}

func TestCopyHelpMentionsTheSafetyAdvice(t *testing.T) {
	out, err := run(t, "copy", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Safely Remove", "phone:/DCIM/Camera", "RAM cache", "--dry-run", "--on-conflict"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}
}
