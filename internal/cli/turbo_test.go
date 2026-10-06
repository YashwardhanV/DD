package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/ntfs/ntfstest"
	"github.com/yashwardhanv/dd/internal/volume"
)

// ntfsDrive is a fake NTFS pen drive whose "root" is a real temp folder, so
// lookups of sizes and dates have real files to examine.
func ntfsDrive(root string) volume.Volume {
	return volume.Volume{
		Root: root, Label: "BACKUP", FileSystem: "NTFS", Serial: 0x0BADF00D,
		DriveType: volume.DriveFixed, TotalBytes: 500 << 30, FreeBytes: 100 << 30, Ready: true,
	}
}

// turboSetup wires a fake NTFS volume (with the given names also created as
// real files under a temp folder) into the CLI hooks.
type turboEnv struct {
	t     *testing.T
	dir   string
	fake  *ntfstest.Volume
	drive volume.Volume
	ids   map[string]uint64
}

func newTurbo(t *testing.T, elevated bool) *turboEnv {
	t.Helper()
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "indexes"))
	e := &turboEnv{t: t, dir: t.TempDir(), fake: ntfstest.New(), ids: map[string]uint64{}}
	e.drive = ntfsDrive(e.dir)

	e.ids["Docs"] = e.fake.Mkdir(0, "Docs")
	e.ids["cv"] = e.fake.Create(e.ids["Docs"], "résumé.pdf")
	e.ids["readme"] = e.fake.Create(0, "readme.txt")
	e.write(`Docs\résumé.pdf`, 2048)
	e.write(`readme.txt`, 5)

	plug(t, e.drive)
	setHooks(t, e.fake, elevated)
	return e
}

func setHooks(t *testing.T, fake *ntfstest.Volume, elevated bool) {
	t.Helper()
	oldOpen, oldElev := openNTFS, isElevated
	openNTFS = func(string) (*ntfs.Session, error) { return fake.Session(), nil }
	isElevated = func() bool { return elevated }
	t.Cleanup(func() { openNTFS, isElevated = oldOpen, oldElev })
}

func (e *turboEnv) write(rel string, size int) {
	e.t.Helper()
	p := index.JoinRoot(e.dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// index runs the index flow for the fake drive, returning what it printed.
func (e *turboEnv) index(opts scanOptions) (string, error) {
	e.t.Helper()
	var out strings.Builder
	err := indexRoot(context.Background(), &out, e.dir, volumeHeader(e.drive), opts)
	return out.String(), err
}

func (e *turboEnv) saved() *index.Index {
	e.t.Helper()
	store, _ := openStore()
	ix, err := store.Load(volume.SourceID(e.drive))
	if err != nil {
		e.t.Fatalf("no saved index: %v", err)
	}
	return ix
}

func TestTurboFullIndexThenIncrementalRefresh(t *testing.T) {
	e := newTurbo(t, true)

	// 1. First time: a full read of the master file table.
	out, err := e.index(scanOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Using NTFS turbo mode", "Indexed ", "names only"} {
		if !strings.Contains(out, want) {
			t.Errorf("full index output missing %q:\n%s", want, out)
		}
	}
	ix := e.saved()
	if ix.Header.Scanner != "mft" || ix.Header.UsnJournalID != e.fake.JournalID || ix.Header.NextUsn == 0 {
		t.Errorf("header = %+v", ix.Header)
	}
	if ix.Len() != 3 { // Docs, résumé.pdf, readme.txt - and nothing from NTFS internals
		t.Errorf("index has %d entries, want 3", ix.Len())
	}

	// 2. Search shows REAL sizes even though the index stored none: they are
	// looked up lazily for the displayed results.
	res, _ := run(t, "search", "readme")
	if !strings.Contains(res, "readme.txt") || !strings.Contains(res, "5 B") {
		t.Errorf("lazy metadata should show readme.txt's size:\n%s", res)
	}
	res, _ = run(t, "search", "résumé")
	if !strings.Contains(res, "2.0 KB") {
		t.Errorf("lazy metadata for résumé.pdf:\n%s", res)
	}

	// 3. Change the drive: create, rename, move and delete.
	e.fake.Create(0, "brand new.txt")
	e.fake.Rename(e.ids["readme"], "README final.md")
	e.fake.Delete(e.ids["cv"])
	out, err = e.index(scanOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Refreshed ") || !strings.Contains(out, "from change journal — 3 changes applied in ") {
		t.Errorf("want an incremental refresh of 3 changes:\n%s", out)
	}
	if strings.Contains(out, "Using NTFS turbo mode") {
		t.Errorf("a refresh must not re-read the master file table:\n%s", out)
	}
	res, _ = run(t, "search", "brand")
	if !strings.Contains(res, "brand new.txt") {
		t.Errorf("created file missing:\n%s", res)
	}
	res, _ = run(t, "search", "readme")
	if !strings.Contains(res, "README final.md") || strings.Contains(res, "readme.txt") {
		t.Errorf("renamed file wrong:\n%s", res)
	}
	res, _ = run(t, "search", "résumé")
	if !strings.Contains(res, "0 of 0 matches") {
		t.Errorf("deleted file still found:\n%s", res)
	}

	// 4. --full forces a complete re-read even though a refresh would work.
	out, _ = e.index(scanOptions{full: true})
	if !strings.Contains(out, "Using NTFS turbo mode") || strings.Contains(out, "Refreshed") {
		t.Errorf("--full:\n%s", out)
	}
}

func TestTurboRefreshFallsBackToFullRebuildWhenJournalWasReset(t *testing.T) {
	e := newTurbo(t, true)
	if _, err := e.index(scanOptions{}); err != nil {
		t.Fatal(err)
	}
	e.fake.ResetJournal()
	e.fake.Create(0, "after reset.txt")
	out, err := e.index(scanOptions{})
	if err != nil || !strings.Contains(out, "Can't refresh from the change journal (the change journal was reset") || !strings.Contains(out, "Using NTFS turbo mode") {
		t.Fatalf("%v\n%s", err, out)
	}
	if e.saved().Header.UsnJournalID != e.fake.JournalID {
		t.Error("the new journal ID must be stored")
	}
	if res, _ := run(t, "search", "after reset"); !strings.Contains(res, "after reset.txt") {
		t.Errorf("full rebuild must include the new file:\n%s", res)
	}
}

func TestTurboWithMetadataStoresRealSizes(t *testing.T) {
	e := newTurbo(t, true)
	out, err := e.index(scanOptions{withMetadata: true})
	if err != nil || !strings.Contains(out, "Sizes and dates read for 3 entries (0 could not be read)") {
		t.Fatalf("%v\n%s", err, out)
	}
	ix := e.saved()
	got := map[string]int64{}
	for s := 0; s < ix.Len(); s++ {
		got[ix.Names[s]] = ix.Sizes[s]
		if ix.Flags[s]&index.FlagNoMeta != 0 {
			t.Errorf("%s still has FlagNoMeta after --with-metadata", ix.Names[s])
		}
	}
	if got["readme.txt"] != 5 || got["résumé.pdf"] != 2048 {
		t.Errorf("sizes = %v", got)
	}
	// --with-metadata never takes the refresh shortcut (a refresh would lose the metadata).
	e.fake.Create(0, "x.txt")
	if out, _ := e.index(scanOptions{withMetadata: true}); strings.Contains(out, "Refreshed") {
		t.Errorf("--with-metadata must do a full read:\n%s", out)
	}
}

func TestLazyMetadataIsSkippedForOfflineDrives(t *testing.T) {
	e := newTurbo(t, true)
	if _, err := e.index(scanOptions{}); err != nil {
		t.Fatal(err)
	}
	plug(t) // unplugged
	out, _ := run(t, "search", "readme")
	if !strings.Contains(out, "offline") || !strings.Contains(out, "?  -") {
		t.Errorf("an unplugged drive can't be asked for sizes; they show as unknown:\n%s", out)
	}
}

func TestNTFSWithoutAdministratorFallsBackWithOneTip(t *testing.T) {
	e := newTurbo(t, false)
	fakeDriveType(t, false, false)
	out, err := e.index(scanOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := strings.Count(out, "Administrator terminal for NTFS turbo mode"); n != 1 {
		t.Errorf("want the tip exactly once, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "Using 4 workers") || e.saved().Header.Scanner != "parallel" {
		t.Errorf("should fall back to the parallel scanner:\n%s", out)
	}
	if e.fake.Calls[ntfs.FsctlEnumUsnData] != 0 {
		t.Error("must not touch the MFT without Administrator rights")
	}
	// The fallback scan reads the real folder; the real files are indexed with sizes.
	if res, _ := run(t, "search", "readme"); !strings.Contains(res, "5 B") {
		t.Errorf("%s", res)
	}
}

func TestExplicitMFTModeErrors(t *testing.T) {
	// Not Administrator.
	e := newTurbo(t, false)
	_, err := e.index(scanOptions{mode: "mft"})
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "needs an Administrator terminal") || !strings.Contains(ue.Hint, "--mode parallel") {
		t.Errorf("not elevated: %v", err)
	}

	// A plain folder can't use turbo mode.
	tree, _ := withFixture(t)
	setHooks(t, e.fake, true)
	var out strings.Builder
	err = indexRoot(context.Background(), &out, tree, index.FolderHeader(tree), scanOptions{mode: "mft"})
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "whole NTFS drives") {
		t.Errorf("folder source: %v", err)
	}

	// A non-NTFS drive can't either.
	err = indexRoot(context.Background(), &out, tree, volumeHeader(fakeDrive("E:")), scanOptions{mode: "mft"})
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "whole NTFS drives") {
		t.Errorf("exFAT drive: %v", err)
	}
}

func TestNonNTFSDrivesNeverGetTheTipOrTurbo(t *testing.T) {
	tree, _ := withFixture(t)
	setHooks(t, ntfstest.New(), true)
	fakeDriveType(t, false, true)
	var out strings.Builder
	if err := indexRoot(context.Background(), &out, tree, volumeHeader(fakeDrive("E:")), scanOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "turbo") || strings.Contains(out.String(), "Administrator") {
		t.Errorf("exFAT drive output mentions turbo mode:\n%s", out.String())
	}
}

func TestDriveWithoutJournalFallsBackAndExplainsHowToEnableTurbo(t *testing.T) {
	e := newTurbo(t, true)
	fakeDriveType(t, false, true)
	e.fake.JournalActive = false

	out, err := e.index(scanOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "no NTFS change journal") || !strings.Contains(out, "drivedex journal enable") || !strings.Contains(out, "Using ") {
		t.Errorf("fallback explanation missing:\n%s", out)
	}
	if e.saved().Header.Scanner != "parallel" {
		t.Errorf("scanner = %s", e.saved().Header.Scanner)
	}

	// Explicit --mode mft on such a drive is an error with the fix.
	_, err = e.index(scanOptions{mode: "mft"})
	var ue *UserError
	if !errors.As(err, &ue) || !errors.Is(err, ntfs.ErrNoJournal) || !strings.Contains(ue.Hint, "drivedex journal enable E:") && !strings.Contains(ue.Hint, "drivedex journal enable") {
		t.Errorf("explicit mode: %v", err)
	}
}

func TestOpenDeniedFallsBackQuietlyInAutoMode(t *testing.T) {
	e := newTurbo(t, true) // we *think* we're admin, but Windows refuses the volume handle
	fakeDriveType(t, false, true)
	openNTFS = func(string) (*ntfs.Session, error) { return nil, ntfs.ErrNeedAdmin }
	out, err := e.index(scanOptions{})
	if err != nil || !strings.Contains(out, "Administrator terminal for NTFS turbo mode") || e.saved().Header.Scanner != "parallel" {
		t.Errorf("%v\n%s", err, out)
	}
	if _, err := e.index(scanOptions{mode: "mft"}); err == nil || !strings.Contains(err.Error(), "needs an Administrator terminal") {
		t.Errorf("explicit mode should fail loudly: %v", err)
	}
}

func TestTurboCancelledMidScanSavesNothing(t *testing.T) {
	e := newTurbo(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder
	if err := indexRoot(ctx, &out, e.dir, volumeHeader(e.drive), scanOptions{}); err != nil {
		t.Fatal(err)
	}
	store, _ := openStore()
	if store.Exists(volume.SourceID(e.drive)) || !strings.Contains(out.String(), "Nothing was saved") {
		t.Errorf("cancelled scan must not save:\n%s", out.String())
	}
}

// ---- journal command ------------------------------------------------------------------

func TestJournalStatus(t *testing.T) {
	e := newTurbo(t, true)
	volumeForPath = func(string) (volume.Volume, error) { return e.drive, nil }
	t.Cleanup(func() { volumeForPath = volume.ForPath })

	out, err := run(t, "journal", "status", "e:")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"is active", "ID:", "0x01D9A0B1C2D3E4F5", "32.0 MB", "4.0 MB", "Next USN:", "Lowest valid:"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	if e.fake.Closed != 1 {
		t.Errorf("the volume handle must be closed, Closed = %d", e.fake.Closed)
	}

	e.fake.JournalActive = false
	out, _ = run(t, "journal", "status", "E:")
	if !strings.Contains(out, "is NOT active") || !strings.Contains(out, "drivedex journal enable E:") {
		t.Errorf("inactive:\n%s", out)
	}
}

func TestJournalEnable(t *testing.T) {
	e := newTurbo(t, true)
	volumeForPath = func(string) (volume.Volume, error) { return e.drive, nil }
	t.Cleanup(func() { volumeForPath = volume.ForPath })
	e.fake.JournalActive = false

	// Declining changes nothing.
	for _, answer := range []string{"n\n", "\n", ""} {
		var out strings.Builder
		if err := runJournalEnable(&out, strings.NewReader(answer), "E:", false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "adds a small change log (32 MB)") || !strings.Contains(out.String(), "Continue? y/N") || !strings.Contains(out.String(), "Cancelled") {
			t.Errorf("answer %q:\n%s", answer, out.String())
		}
		if e.fake.JournalActive || e.fake.Calls[ntfs.FsctlCreateUsnJournal] != 0 {
			t.Fatal("declining must not create the journal")
		}
	}
	// Accepting creates it.
	var out strings.Builder
	if err := runJournalEnable(&out, strings.NewReader("y\n"), "E:", false); err != nil {
		t.Fatal(err)
	}
	if !e.fake.JournalActive || !strings.Contains(out.String(), "switched on") {
		t.Errorf("not enabled:\n%s", out.String())
	}
	// Already active: nothing to do, no prompt.
	out.Reset()
	if err := runJournalEnable(&out, strings.NewReader(""), "E:", false); err != nil || !strings.Contains(out.String(), "already active") {
		t.Errorf("%v\n%s", err, out.String())
	}
	// --yes skips the question.
	e.fake.JournalActive = false
	if _, err := run(t, "journal", "enable", "E:", "--yes"); err != nil || !e.fake.JournalActive {
		t.Errorf("--yes: %v", err)
	}
}

func TestJournalCommandErrors(t *testing.T) {
	e := newTurbo(t, false)
	volumeForPath = func(string) (volume.Volume, error) { return e.drive, nil }
	t.Cleanup(func() { volumeForPath = volume.ForPath })

	check := func(args []string, want string) {
		t.Helper()
		_, err := run(t, args...)
		var ue *UserError
		if !errors.As(err, &ue) || !strings.Contains(ue.Msg+" "+ue.Hint, want) {
			t.Errorf("%v: got %v, want something about %q", args, err, want)
		}
	}
	check([]string{"journal", "status", "E:"}, "Administrator terminal") // not elevated
	check([]string{"journal", "status", "notadrive"}, "isn't a drive letter")

	setHooks(t, e.fake, true)
	volumeForPath = func(string) (volume.Volume, error) { return fakeDrive("E:"), nil } // exFAT
	check([]string{"journal", "status", "E:"}, "not NTFS")
	volumeForPath = func(string) (volume.Volume, error) { return volume.Volume{Root: `H:\`}, nil } // not ready
	check([]string{"journal", "enable", "H:"}, "isn't ready")
	volumeForPath = func(string) (volume.Volume, error) { return volume.Volume{}, errors.ErrUnsupported }
	check([]string{"journal", "status", "E:"}, "only be checked on Windows")
}
