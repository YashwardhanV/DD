package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCopyPlansFromAFreshFolderIndex(t *testing.T) {
	tree, res := withFixture(t)
	fakeDriveType(t, false, true) // a flash drive (the CI machine's real disk could report a seek penalty)
	if out, err := run(t, "index", tree); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	os.WriteFile(filepath.Join(tree, "added-after-indexing.txt"), []byte("new"), 0o644)
	var victim string
	filepath.WalkDir(tree, func(p string, d os.DirEntry, _ error) error {
		if victim == "" && d != nil && !d.IsDir() && !strings.Contains(p, "added-after") {
			victim = p
		}
		return nil
	})
	os.Remove(victim)

	dst := filepath.Join(t.TempDir(), "backup")
	out, err := run(t, "copy", tree, dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Planned from the index of tree (made just now)",
		"Copied 400 files", // 399 planned + 1 found
		"1 file was found while copying",
		"1 file had been deleted since the index was made (vanished)",
		"Index of tree updated: 1 added or changed, 1 removed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if got := countFilesUnder(t, filepath.Join(dst, "tree")); got != res.Files {
		t.Errorf("destination has %d files, want %d", got, res.Files)
	}
	if out, _ := run(t, "search", "added-after-indexing"); !strings.Contains(out, "added-after-indexing.txt") {
		t.Errorf("the refreshed index should find the new file:\n%s", out)
	}

	// --fresh / --v1 list the source like v1 and say nothing about an index.
	out, _ = run(t, "copy", tree, filepath.Join(t.TempDir(), "b2"), "--fresh")
	if strings.Contains(out, "Planned from the index") || !strings.Contains(out, "Copied 400 files") {
		t.Errorf("--fresh:\n%s", out)
	}
	// --plan index with no index covering the source explains why.
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "x.txt"), []byte("x"), 0o644)
	if _, err := run(t, "copy", other, t.TempDir(), "--plan", "index"); err == nil || !strings.Contains(err.Error(), "no saved index covers it") {
		t.Errorf("--plan index without an index: %v", err)
	}
}

func TestCopyFastPathFlags(t *testing.T) {
	tree, res := withFixture(t)
	for _, args := range [][]string{
		{"--verify"}, {"--unbuffered", "on", "--verify"}, {"--v1", "--verify"},
	} {
		out, err := run(t, append([]string{"copy", tree, filepath.Join(t.TempDir(), "o")}, args...)...)
		if err != nil || !strings.Contains(out, "Verified:") {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
	dst := filepath.Join(t.TempDir(), "brand-new")
	out, err := run(t, "copy", tree, dst, "--experimental-direct")
	if err != nil || countFilesUnder(t, filepath.Join(dst, "tree")) != res.Files {
		t.Errorf("direct: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dst, ".drivedex-resume")); !os.IsNotExist(err) {
		t.Error("the resume journal must be gone after a complete copy")
	}
	if _, err := run(t, "copy", tree, t.TempDir(), "--unbuffered", "sometimes"); err == nil {
		t.Error("--unbuffered sometimes must be refused")
	}
}

func TestCopyAutotuneOnlyForAutomaticWorkerCounts(t *testing.T) {
	tree, _ := withFixture(t)
	fakeDriveType(t, false, true)
	out, err := run(t, "copy", tree, filepath.Join(t.TempDir(), "a"))
	if err != nil || !strings.Contains(out, "(8 workers: both ends are flash/SSD) - adjusts itself between 1 and 16 while copying") {
		t.Errorf("auto: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"--workers", "8"}, {"--v1"}} {
		out, _ := run(t, append([]string{"copy", tree, filepath.Join(t.TempDir(), "b")}, args...)...)
		if strings.Contains(out, "adjusts itself") {
			t.Errorf("%v must not autotune:\n%s", args, out)
		}
	}
	fakeDriveType(t, true, true) // a hard disk: at most 4
	out, _ = run(t, "copy", tree, filepath.Join(t.TempDir(), "c"))
	if !strings.Contains(out, "adjusts itself between 1 and 4") {
		t.Errorf("hard disk bounds:\n%s", out)
	}
}

func TestCopyDiskOrderOnAnNTFSHardDisk(t *testing.T) {
	tree, res := withFixture(t)
	fakeDriveType(t, true, true) // a spinning disk
	fakeChecks(t, 1<<40, "NTFS")
	old := firstLCN
	var asked atomic.Int64
	firstLCN = func(p string) (int64, error) { asked.Add(1); return int64(len(p)), nil }
	t.Cleanup(func() { firstLCN = old })

	out, err := run(t, "copy", tree, filepath.Join(t.TempDir(), "a"))
	if err != nil || !strings.Contains(out, "in the order they sit on the disk") || !strings.Contains(out, "between 1 and 2") {
		t.Errorf("auto on an NTFS hard disk: %v\n%s", err, out)
	}
	if asked.Load() != int64(res.Files) {
		t.Errorf("asked for %d files' clusters, want %d", asked.Load(), res.Files)
	}
	out, _ = run(t, "copy", tree, filepath.Join(t.TempDir(), "b"), "--order", "size")
	if strings.Contains(out, "order they sit") {
		t.Errorf("--order size:\n%s", out)
	}
	fakeChecks(t, 1<<40, "exFAT")
	out, _ = run(t, "copy", tree, filepath.Join(t.TempDir(), "c"))
	if strings.Contains(out, "order they sit") {
		t.Errorf("auto on exFAT must keep size order:\n%s", out)
	}
	firstLCN = func(string) (int64, error) { return 0, errors.New("nope") }
	out, _ = run(t, "copy", tree, filepath.Join(t.TempDir(), "d"), "--order", "disk")
	if !strings.Contains(out, "Disk order not used") || !strings.Contains(out, "Copied 400 files") {
		t.Errorf("--order disk where it can't work:\n%s", out)
	}
	if _, err := run(t, "copy", tree, t.TempDir(), "--order", "random"); err == nil {
		t.Error("--order random must be refused")
	}
}
