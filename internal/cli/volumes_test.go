package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/volume"
)

// fakeDrive pretends a pen drive is plugged in at the given letter.
func fakeDrive(letter string) volume.Volume {
	return volume.Volume{
		Root: letter + `\`, Label: "SANDISK 32GB", FileSystem: "exFAT", Serial: 0x1A2B3C4D,
		DriveType: volume.DriveRemovable, TotalBytes: 32 << 30, FreeBytes: 10 << 30, Ready: true,
	}
}

// plug sets which drives "Windows" reports for the rest of the test.
func plug(t *testing.T, vols ...volume.Volume) {
	t.Helper()
	old := listVolumes
	listVolumes = func() ([]volume.Volume, error) { return vols, nil }
	t.Cleanup(func() { listVolumes = old })
}

// indexAsDrive indexes the fixture tree as if it were the pen drive.
func indexAsDrive(t *testing.T, tree string, v volume.Volume) {
	t.Helper()
	var out strings.Builder
	if err := indexRoot(context.Background(), &out, tree, volumeHeader(v), scanOptions{}); err != nil {
		t.Fatalf("indexRoot: %v\n%s", err, out.String())
	}
}

func TestVolumeIndexSurvivesLetterChangeAndUnplug(t *testing.T) {
	tree, _ := withFixture(t)

	// 1. Index the pen drive while it is E:.
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))

	out, err := run(t, "search", "resume.pdf", "--limit", "1")
	if err != nil || !strings.Contains(out, "E:") || strings.Contains(out, "offline") {
		t.Fatalf("while plugged in at E: want an E: path, got err=%v\n%s", err, out)
	}

	// 2. Unplug it: the result is still found, marked offline, with the label.
	plug(t)
	out, _ = run(t, "search", "resume.pdf", "--limit", "1")
	if !strings.Contains(out, "[SANDISK 32GB · offline · last seen E:] \\") {
		t.Errorf("unplugged drive should show as offline with label:\n%s", out)
	}
	if !strings.Contains(out, "1 of ") {
		t.Errorf("offline results must still be found:\n%s", out)
	}

	// 3. Plug it in again, but now Windows gave it the letter G:.
	plug(t, fakeDrive("G:"))
	out, _ = run(t, "search", "resume.pdf", "--limit", "1")
	if !strings.HasPrefix(out, "G:") || strings.Contains(out, "offline") || strings.Contains(out, "E:") {
		t.Errorf("replugged at G: should show G: (found by serial), got:\n%s", out)
	}
}

func TestReindexUnderNewLetterReplacesTheSameIndex(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))
	plug(t, fakeDrive("G:"))
	indexAsDrive(t, tree, fakeDrive("G:"))

	out, _ := run(t, "list")
	if n := strings.Count(out, "vol-1A2B3C4D"); n != 1 {
		t.Errorf("same serial must give one index, list shows %d:\n%s", n, out)
	}
	if !strings.Contains(out, `G:\`) || !strings.Contains(out, "online") {
		t.Errorf("last mount should now be G:\\ and online:\n%s", out)
	}
}

func TestSearchDriveFilter(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))
	// A second, folder index that also contains Resume.pdf.
	if _, err := run(t, "index", tree); err != nil {
		t.Fatal(err)
	}

	out, _ := run(t, "search", "resume.pdf", "--drive", "E:", "--limit", "100")
	if !strings.Contains(out, "in 1 index") {
		t.Errorf("--drive E: should search only the E: index:\n%s", out)
	}
	out, _ = run(t, "search", "resume.pdf", "--drive", "e", "--limit", "1")
	if !strings.Contains(out, "in 1 index") {
		t.Errorf("--drive accepts a bare letter:\n%s", out)
	}
	if _, err := run(t, "search", "x", "--drive", "Q:"); err == nil || !strings.Contains(err.Error(), "isn't connected") {
		t.Errorf("unknown drive should explain itself, got %v", err)
	}
	if _, err := run(t, "search", "x", "--drive", "notaletter"); err == nil {
		t.Error("junk --drive should be rejected")
	}
}

func TestStaleIndexTip(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))

	// Fresh index: no tip.
	indexAsDrive(t, tree, fakeDrive("E:"))
	out, _ := run(t, "search", "resume")
	if strings.Contains(out, "tip:") {
		t.Errorf("fresh index should not trigger a tip:\n%s", out)
	}

	// Age the index by rewriting its header: 3 days old.
	store, _ := openStore()
	ix, err := store.Load("vol-1A2B3C4D")
	if err != nil {
		t.Fatal(err)
	}
	ix.Header.UpdatedAt = time.Now().Add(-72 * time.Hour)
	if _, err := store.Save(ix); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, "search", "resume")
	want := "tip: E:\\ was indexed 3 days ago — run 'drivedex index E:' to refresh"
	if !strings.Contains(out, want) {
		t.Errorf("missing stale tip %q in:\n%s", want, out)
	}

	// Unplugged: nothing to refresh, so no tip.
	plug(t)
	out, _ = run(t, "search", "resume")
	if strings.Contains(out, "tip:") {
		t.Errorf("offline drive should not trigger a tip:\n%s", out)
	}
}

func TestDrivesTable(t *testing.T) {
	tree, _ := withFixture(t)
	empty := volume.Volume{Root: `H:\`, DriveType: volume.DriveRemovable} // empty card reader
	plug(t, fakeDrive("E:"), empty, volume.Volume{Root: `C:\`, Label: "Windows", FileSystem: "NTFS", Serial: 0xC0FFEE, DriveType: volume.DriveFixed, TotalBytes: 500 << 30, FreeBytes: 100 << 30, Ready: true})
	indexAsDrive(t, tree, fakeDrive("E:"))

	out, err := run(t, "drives")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DRIVE", "INDEXED?", "SANDISK 32GB", "exFAT", "Removable", "32.0 GB", "10.0 GB", "yes (just now,", "(not ready)", "Windows", "NTFS", "Fixed", "no"} {
		if !strings.Contains(out, want) {
			t.Errorf("drives output missing %q:\n%s", want, out)
		}
	}
}

func TestDrivesOnUnsupportedPlatform(t *testing.T) {
	old := listVolumes
	listVolumes = func() ([]volume.Volume, error) { return nil, errors.ErrUnsupported }
	defer func() { listVolumes = old }()
	if _, err := run(t, "drives"); err == nil || !strings.Contains(err.Error(), "only works on Windows") {
		t.Errorf("got %v", err)
	}
}

func TestListShowsStatusAndSizes(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))
	if _, err := run(t, "index", tree); err != nil { // folder source
		t.Fatal(err)
	}

	out, _ := run(t, "list")
	for _, want := range []string{"ID", "vol-1A2B3C4D", "SANDISK 32GB", "volume", "folder", "exFAT", "online", `E:\`} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q:\n%s", want, out)
		}
	}
	plug(t)
	out, _ = run(t, "list")
	if !strings.Contains(out, "offline") {
		t.Errorf("unplugged volume should be offline:\n%s", out)
	}
}

func TestListWhenEmpty(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "none"))
	out, err := run(t, "list")
	if err != nil || !strings.Contains(out, "Nothing indexed yet") {
		t.Errorf("err=%v out=%q", err, out)
	}
}

func TestForgetByIDLetterAndLabel(t *testing.T) {
	tree, _ := withFixture(t)
	indexExists := func() bool { s, _ := openStore(); return s.Exists("vol-1A2B3C4D") }

	for _, target := range []string{"vol-1A2B3C4D", "vol-1a2b3c4d", "E:", "e", "SANDISK 32GB", "sandisk 32gb"} {
		plug(t, fakeDrive("E:"))
		indexAsDrive(t, tree, fakeDrive("E:"))
		out, err := run(t, "forget", target, "--yes")
		if err != nil || !strings.Contains(out, "The drive itself was not touched") {
			t.Fatalf("forget %q: err=%v\n%s", target, err, out)
		}
		if indexExists() {
			t.Errorf("forget %q left the index behind", target)
		}
	}
}

func TestForgetByLastSeenLetterWhenUnplugged(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))
	plug(t) // unplugged
	if _, err := run(t, "forget", "E:", "--yes"); err != nil {
		t.Fatal(err)
	}
	if s, _ := openStore(); s.Exists("vol-1A2B3C4D") {
		t.Error("index should be gone")
	}
}

func TestForgetOnlyDeletesIndexNeverTheDrive(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))

	before := countFiles(t, tree)
	if _, err := run(t, "forget", "vol-1A2B3C4D", "--yes"); err != nil {
		t.Fatal(err)
	}
	if after := countFiles(t, tree); after != before || before == 0 {
		t.Errorf("drive contents changed: %d files before, %d after", before, after)
	}
}

func TestForgetConfirmationPrompt(t *testing.T) {
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))
	store, _ := openStore()

	// Answering "n" (or just pressing Enter) keeps the index.
	for _, answer := range []string{"n\n", "\n", ""} {
		var out strings.Builder
		if err := runForget(&out, strings.NewReader(answer), "E:", false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "[y/N]") || !strings.Contains(out.String(), "Cancelled") || !store.Exists("vol-1A2B3C4D") {
			t.Errorf("answer %q should cancel:\n%s", answer, out.String())
		}
	}
	var out strings.Builder
	if err := runForget(&out, strings.NewReader("y\n"), "E:", false); err != nil {
		t.Fatal(err)
	}
	if store.Exists("vol-1A2B3C4D") {
		t.Error("answering y should delete")
	}
}

func TestForgetErrors(t *testing.T) {
	tree, _ := withFixture(t)
	if _, err := run(t, "forget", "nothing-like-this", "--yes"); err == nil || !strings.Contains(err.Error(), "no saved index matches") {
		t.Errorf("unknown target: %v", err)
	}
	// Two indexes with the same label -> ambiguous.
	plug(t)
	h1 := volumeHeader(fakeDrive("E:"))
	h2 := volumeHeader(fakeDrive("F:"))
	h2.SourceID = "vol-DEADBEEF"
	for _, h := range []index.Header{h1, h2} {
		var out strings.Builder
		if err := indexRoot(context.Background(), &out, tree, h, scanOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run(t, "forget", "SANDISK 32GB", "--yes"); err == nil || !strings.Contains(err.Error(), "matches 2 indexes") {
		t.Errorf("ambiguous target: %v", err)
	}
}

func TestForgetCanRemoveAnUnreadableFile(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	store, _ := openStore()
	bad := filepath.Join(store.Dir, "broken.ddx")
	if err := os.WriteFile(bad, []byte("garbage garbage garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "forget", "broken", "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bad); err == nil {
		t.Error("unreadable index should have been deleted")
	}
}

func TestHeaderForDriveRoot(t *testing.T) {
	old := volumeForPath
	defer func() { volumeForPath = old }()

	volumeForPath = func(string) (volume.Volume, error) { return fakeDrive("E:"), nil }
	h, err := headerFor("E:", `E:\`)
	if err != nil || h.SourceKind != index.KindVolume || h.SourceID != "vol-1A2B3C4D" || h.LastMount != `E:\` || h.Label != "SANDISK 32GB" || h.FileSystem != "exFAT" || h.TotalBytes != 32<<30 {
		t.Errorf("volume header = %+v, %v", h, err)
	}

	// A folder on the drive stays a folder source.
	h, _ = headerFor(`E:\Photos`, `E:\Photos`)
	if h.SourceKind != index.KindFolder {
		t.Errorf("folder arg gave %+v", h)
	}

	// Not-ready drive -> friendly error.
	volumeForPath = func(string) (volume.Volume, error) {
		return volume.Volume{Root: `H:\`, DriveType: volume.DriveRemovable}, nil
	}
	if _, err := headerFor("H:", `H:\`); err == nil || !strings.Contains(err.Error(), "isn't ready") {
		t.Errorf("not-ready drive: %v", err)
	}

	// Unsupported platform -> folder source.
	volumeForPath = func(string) (volume.Volume, error) { return volume.Volume{}, errors.ErrUnsupported }
	if h, err := headerFor("E:", `E:\`); err != nil || h.SourceKind != index.KindFolder {
		t.Errorf("unsupported platform: %+v %v", h, err)
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(root, func(_ string, d os.DirEntry, _ error) error { n++; return nil })
	return n
}
