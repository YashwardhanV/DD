package cli

import (
	"os"
	"path/filepath"
	"strings"
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
