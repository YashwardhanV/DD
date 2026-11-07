package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/manifest"
	"github.com/yashwardhanv/dd/internal/trash"
)

// syncEnv is a source folder "Photos" with a few files and a destination.
func syncEnv(t *testing.T) (src, dst string) {
	t.Helper()
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	base := t.TempDir()
	src, dst = filepath.Join(base, "Photos"), filepath.Join(base, "Backup")
	t0 := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	for i, rel := range []string{"a.jpg", `2024\b.jpg`, `2024\c.jpg`, `2025\d.jpg`} {
		writeAt(t, filepath.Join(src, osRel(rel)), "content of "+rel, t0.Add(time.Duration(i)*time.Minute))
	}
	return src, dst
}

// osRel turns a test path written with `\` into this OS's separators.
func osRel(rel string) string { return filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/")) }

func writeAt(t *testing.T, p, body string, mtime time.Time) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mtime, mtime)
}

func syncJSON(t *testing.T, args ...string) (copyJSON, string, error) {
	t.Helper()
	stdout, stderr, err := runIn2(t, "", append(append([]string{"sync"}, args...), "--json")...)
	var js copyJSON
	if jerr := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &js); jerr != nil {
		t.Fatalf("not JSON: %v\n%s\n%s", jerr, stdout, stderr)
	}
	return js, stderr, err
}

func TestSyncMirrorsMovesAndRemovesToTheTrash(t *testing.T) {
	src, dst := syncEnv(t)
	js, out, err := syncJSON(t, src, dst, "--yes")
	if err != nil || js.Written != 4 {
		t.Fatalf("first sync: %v %+v\n%s", err, js, out)
	}
	if m, err := manifest.Read(dst); err != nil || m.Len() != 4 {
		t.Fatalf("manifest after the first sync: %v", err)
	}

	// Move one file, delete one, add one.
	os.MkdirAll(filepath.Join(src, "moved"), 0o755)
	os.Rename(filepath.Join(src, "a.jpg"), filepath.Join(src, "moved", "a.jpg"))
	os.Remove(filepath.Join(src, "2024", "c.jpg"))
	writeAt(t, filepath.Join(src, "new.jpg"), "new", time.Now())

	js, out, err = syncJSON(t, src, dst, "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if js.Moved != 1 || js.Written != 1 || js.Removed != 1 || js.Trashed != 1 {
		t.Errorf("moved %d, written %d, removed %d, trashed %d; want 1 each\n%s", js.Moved, js.Written, js.Removed, js.Trashed, out)
	}
	mirror := filepath.Join(dst, "Photos")
	if !exists(filepath.Join(mirror, "moved", "a.jpg")) || exists(filepath.Join(mirror, "a.jpg")) {
		t.Error("a.jpg should have been moved on the destination")
	}
	if exists(filepath.Join(mirror, "2024", "c.jpg")) {
		t.Error("c.jpg should have left the mirror")
	}
	runs, _ := trash.List(dst)
	if len(runs) != 1 || runs[0].Files() != 1 || runs[0].Entries[0].Why != trash.Removed {
		t.Fatalf("trash = %+v", runs)
	}

	// trash list / restore through the CLI.
	out, err = run(t, "trash", "list", dst)
	if err != nil || !strings.Contains(out, runs[0].ID) || !strings.Contains(out, "1 removed") {
		t.Errorf("trash list: %v\n%s", err, out)
	}
	if out, err := run(t, "trash", "restore", dst, runs[0].ID); err != nil || !strings.Contains(out, "Restored 1 file") {
		t.Errorf("trash restore: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(mirror, "2024", "c.jpg")); string(b) != `content of 2024\c.jpg` {
		t.Errorf("restored content = %q", b)
	}

	// A third run with nothing changed in the source does nothing. (The
	// restored c.jpg isn't DriveDex's any more: it is left alone.)
	js, out, err = syncJSON(t, src, dst, "--yes")
	if err != nil || js.Written != 0 || js.Removed != 0 || !strings.Contains(out, "up to date") {
		t.Errorf("third sync: %v %+v\n%s", err, js, out)
	}
	if !exists(filepath.Join(mirror, "2024", "c.jpg")) {
		t.Error("a file DriveDex has no record of must never be removed")
	}
}

func TestSyncAdoptModeDeletesNothing(t *testing.T) {
	src, dst := syncEnv(t)
	writeAt(t, filepath.Join(dst, "Photos", "old-stuff.txt"), "mine", time.Now())
	out, err := run(t, "sync", src, dst, "--yes")
	if err != nil || !strings.Contains(out, "This first sync copies and records only") {
		t.Fatalf("%v\n%s", err, out)
	}
	if !exists(filepath.Join(dst, "Photos", "old-stuff.txt")) {
		t.Error("adopt mode must delete nothing")
	}
}

func TestSyncRefusesAnEmptySource(t *testing.T) {
	_, dst := syncEnv(t)
	empty := filepath.Join(t.TempDir(), "Photos")
	os.MkdirAll(empty, 0o755)
	_, err := run(t, "sync", empty, dst, "--yes")
	var ue *UserError
	if !errors.As(err, &ue) || ue.Code != exitBlocked || !strings.Contains(ue.Msg, "no files") {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncMassDeleteGuard(t *testing.T) {
	src, dst := syncEnv(t)
	if _, err := run(t, "sync", src, dst, "--yes"); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a.jpg", `2024\b.jpg`, `2024\c.jpg`} {
		os.Remove(filepath.Join(src, osRel(rel)))
	}
	out, err := run(t, "sync", src, dst, "--yes")
	var ue *UserError
	if !errors.As(err, &ue) || ue.Code != exitBlocked || !strings.Contains(out, "would remove 3 of the 4 files") {
		t.Fatalf("3 of 4 removed must be blocked: %v\n%s", err, out)
	}
	if !exists(filepath.Join(dst, "Photos", "a.jpg")) {
		t.Fatal("a blocked sync must change nothing")
	}
	if out, err := run(t, "sync", src, dst, "--yes", "--allow-mass-delete"); err != nil {
		t.Fatalf("with --allow-mass-delete: %v\n%s", err, out)
	}
	if exists(filepath.Join(dst, "Photos", "a.jpg")) {
		t.Error("allowed: the files should be in the trash")
	}
}

func TestSyncNeedsYesWhenItCantAsk(t *testing.T) {
	src, dst := syncEnv(t)
	run(t, "sync", src, dst, "--yes")
	os.Remove(filepath.Join(src, "a.jpg"))
	_, err := run(t, "sync", src, dst) // stdin is not a console here
	var ue *UserError
	if !errors.As(err, &ue) || ue.Code != exitBlocked || !strings.Contains(ue.Msg, "can't ask") {
		t.Fatalf("err = %v", err)
	}
	// Answering n in an interactive run changes nothing either.
	out, _, err := runIn2(t, "n\n", "sync", src, dst)
	if err == nil || !strings.Contains(out, "Go ahead?") || !exists(filepath.Join(dst, "Photos", "a.jpg")) {
		t.Fatalf("answering n: %v\n%s", err, out)
	}
}

func TestSyncDryRunListsEverythingAndChangesNothing(t *testing.T) {
	src, dst := syncEnv(t)
	run(t, "sync", src, dst, "--yes")
	os.Remove(filepath.Join(src, "a.jpg"))
	out, err := run(t, "sync", src, dst, "--dry-run")
	if err != nil || !strings.Contains(out, "remove (to the trash)") || !strings.Contains(out, "Nothing was changed (dry run)") {
		t.Fatalf("%v\n%s", err, out)
	}
	if !exists(filepath.Join(dst, "Photos", "a.jpg")) {
		t.Error("a dry run must not remove anything")
	}
}

func TestCopyKeepsReplacedFilesAndReportsMoves(t *testing.T) {
	src, dst := syncEnv(t)
	if out, err := run(t, "copy", src, dst); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m, _ := manifest.Read(dst)
	rec, ok := m.Get(filepath.Join("Photos", "a.jpg"))
	if !ok || rec.HashKind != manifest.HashXXH3 || rec.SrcSize != int64(len("content of a.jpg")) {
		t.Fatalf("record = %+v, %v", rec, ok)
	}

	writeAt(t, filepath.Join(src, "a.jpg"), "edited", time.Now())
	os.Rename(filepath.Join(src, "2025", "d.jpg"), filepath.Join(src, "d.jpg"))
	out, err := run(t, "copy", src, dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "1 file was moved in the source since last time") {
		t.Errorf("want the moved-files note:\n%s", out)
	}
	if !strings.Contains(out, "in the trash: drivedex trash list") {
		t.Errorf("want the trash note:\n%s", out)
	}
	runs, _ := trash.List(dst)
	if len(runs) != 1 || runs[0].Entries[0].Why != trash.Replaced {
		t.Fatalf("the old a.jpg must be in the trash: %+v", runs)
	}
	if !exists(filepath.Join(dst, "Photos", "2025", "d.jpg")) {
		t.Error("copy never removes the old copy of a moved file")
	}
}

func TestTrashEmptyNeedsConfirmation(t *testing.T) {
	src, dst := syncEnv(t)
	run(t, "copy", src, dst)
	writeAt(t, filepath.Join(src, "a.jpg"), "edited", time.Now())
	run(t, "copy", src, dst)
	_, err := run(t, "trash", "empty", dst)
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "can't ask") {
		t.Fatalf("err = %v", err)
	}
	if out, err := run(t, "trash", "empty", dst, "--yes"); err != nil || !strings.Contains(out, "Deleted 1 trash run") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestBlockedCopyLeavesNoManifest(t *testing.T) {
	src, dst := syncEnv(t)
	fakeChecks(t, 10, "NTFS")
	run(t, "copy", src, dst)
	if exists(manifest.Dir(dst)) {
		t.Error("a copy that pre-flight stopped must not create the manifest folder")
	}
}

// TestINV1_CompareJournalCatchesAnEditThatKeptItsTime goes through the fake
// NTFS drive: the copy is planned from the saved index, the records carry the
// journal position, and an edit that put the old time back is copied again.
func TestINV1_CompareJournalCatchesAnEditThatKeptItsTime(t *testing.T) {
	e := sourceListEnv(t, true, true)
	dst := filepath.Join(t.TempDir(), "out")
	if out, err := run(t, "copy", e.dir, dst, "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	// Edit readme.txt (same length) and put its old time back; the journal
	// sees the write.
	p := index.JoinRoot(e.dir, "readme.txt")
	st, _ := os.Stat(p)
	os.WriteFile(p, []byte("HELLO"), 0o644)
	os.Chtimes(p, st.ModTime(), st.ModTime())
	e.fake.Write(e.ids["readme"], 5, st.ModTime().UnixNano())

	out, err := run(t, "copy", e.dir, dst, "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "1 unchanged (journal)") || !strings.Contains(out, "1 changed") {
		t.Errorf("want the journal report line:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, filepath.Base(e.dir), "readme.txt")); string(b) != "HELLO" {
		t.Errorf("the edit must be copied: %q", b)
	}
	// --compare fast can't see it (and says nothing about the journal).
	os.WriteFile(p, []byte("WORLD"), 0o644)
	os.Chtimes(p, st.ModTime(), st.ModTime())
	e.fake.Write(e.ids["readme"], 5, st.ModTime().UnixNano())
	out, _ = run(t, "copy", e.dir, dst, "--yes", "--compare", "fast")
	if !strings.Contains(out, "Everything is already there") {
		t.Errorf("fast mode is size-and-time only:\n%s", out)
	}
}

func TestCompareJournalWithoutAdministratorSaysWhy(t *testing.T) {
	src, dst := syncEnv(t)
	out, err := run(t, "copy", src, dst, "--compare", "journal", "--dry-run")
	if err != nil || !strings.Contains(out, "no readable NTFS change journal") {
		t.Errorf("%v\n%s", err, out)
	}
}
