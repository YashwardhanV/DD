package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/volume"
)

// sourceListEnv is a fake NTFS drive (a real temp folder) with the volume
// lookups pointed at it, ready for copy --source-list tests.
func sourceListEnv(t *testing.T, elevated, indexed bool) *turboEnv {
	t.Helper()
	e := newTurbo(t, elevated)
	volumeForPath = func(string) (volume.Volume, error) { return e.drive, nil }
	t.Cleanup(func() { volumeForPath = volume.ForPath })
	if indexed {
		if out, err := e.index(scanOptions{}); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	return e
}

func TestSourceListAutoFallsBackWhenNotIndexed(t *testing.T) {
	e := sourceListEnv(t, true, false)
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "copy", e.dir, dst, "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "(index not usable: the drive hasn't been indexed)") {
		t.Errorf("want the fallback reason:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dst, filepath.Base(e.dir), "readme.txt")); err != nil {
		t.Errorf("the copy must still happen: %v", err)
	}
}

func TestSourceListAutoNeedsAdministrator(t *testing.T) {
	e := sourceListEnv(t, true, true)
	isElevated = func() bool { return false }
	out, err := run(t, "copy", e.dir, filepath.Join(t.TempDir(), "out"), "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "(index not usable: not running as Administrator)") {
		t.Errorf("want the Administrator reason:\n%s", out)
	}
}

func TestSourceListIndexFailsInsteadOfListing(t *testing.T) {
	e := sourceListEnv(t, true, true)
	isElevated = func() bool { return false }
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "copy", e.dir, dst, "--yes", "--source-list", "index")
	if err == nil || !strings.Contains(err.Error(), "not running as Administrator") {
		t.Fatalf("want an error naming the reason, got %v\n%s", err, out)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("nothing may be written when the index can't be used")
	}
}

func TestSourceListScanAlwaysLists(t *testing.T) {
	e := sourceListEnv(t, true, true)
	out, err := run(t, "copy", e.dir, filepath.Join(t.TempDir(), "out"), "--yes", "--source-list", "scan")
	if err != nil || !strings.Contains(out, "(--source-list scan)") || strings.Contains(out, "Planned from the saved index") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestSourceListRejectsUnknownValues(t *testing.T) {
	e := sourceListEnv(t, true, false)
	for _, args := range [][]string{{"--source-list", "mft"}, {"--read-order", "random"}} {
		out, err := run(t, append([]string{"copy", e.dir, filepath.Join(t.TempDir(), "out")}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "isn't valid") {
			t.Errorf("%v: want a validation error, got %v\n%s", args, err, out)
		}
	}
}

// TestINV5_PlanFromIndexAfterEdits: the saved index is used only after the
// change journal brought it up to date, so an edit made after indexing is
// copied with its new size.
func TestINV5_PlanFromIndexAfterEdits(t *testing.T) {
	e := sourceListEnv(t, true, true)
	e.write("readme.txt", 50)
	st, _ := os.Stat(index.JoinRoot(e.dir, "readme.txt"))
	e.fake.Write(e.ids["readme"], 50, st.ModTime().UnixNano())

	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "copy", e.dir, dst, "--yes", "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Planned from the saved index of") {
		t.Errorf("want the index plan line:\n%s", out)
	}
	dst = filepath.Join(dst, filepath.Base(e.dir)) // copy puts the folder inside the destination
	if got, err := os.Stat(filepath.Join(dst, "readme.txt")); err != nil || got.Size() != 50 {
		t.Errorf("readme.txt after the edit: %v %v", got, err)
	}
	if got, err := os.Stat(filepath.Join(dst, "Docs", "résumé.pdf")); err != nil || got.Size() != 2048 {
		t.Errorf("résumé.pdf: %v %v", got, err)
	}
	var js copyJSON
	start := strings.Index(out, "{")
	if start < 0 || json.Unmarshal([]byte(out[start:strings.LastIndex(out, "}")+1]), &js) != nil || js.PlannedFrom != "index" {
		t.Errorf("--json planned_from should be index:\n%s", out)
	}
}

func TestSourceListSubfolderFromIndex(t *testing.T) {
	e := sourceListEnv(t, true, true)
	dst := filepath.Join(t.TempDir(), "out")
	out, err := run(t, "copy", filepath.Join(e.dir, "Docs"), dst, "--yes")
	if err != nil || !strings.Contains(out, "Planned from the saved index of") {
		t.Fatalf("%v\n%s", err, out)
	}
	entries, _ := os.ReadDir(filepath.Join(dst, "Docs"))
	if len(entries) != 1 || entries[0].Name() != "résumé.pdf" {
		t.Errorf("only the folder's own file should be copied, got %v", entries)
	}
}
