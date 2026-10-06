package ntfs_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/ntfs/ntfstest"
	"github.com/yashwardhanv/dd/internal/scan"
)

// populated returns a fake drive with a small but realistic tree.
func populated() (*ntfstest.Volume, map[string]uint64) {
	v := ntfstest.New()
	ids := map[string]uint64{}
	ids["Photos"] = v.Mkdir(0, "Photos")
	ids["2024"] = v.Mkdir(ids["Photos"], "2024")
	ids["img"] = v.Create(ids["2024"], "img_001.jpg")
	ids["Docs"] = v.Mkdir(0, "Docs")
	ids["cv"] = v.Create(ids["Docs"], "résumé.pdf")
	ids["Archive"] = v.Mkdir(0, "Archive")
	ids["readme"] = v.Create(0, "readme.txt")
	return v, ids
}

func sortedPaths(ix *index.Index) []string {
	cache := map[uint64]string{}
	var out []string
	for slot := 0; slot < ix.Len(); slot++ {
		if ix.Flags[slot]&index.FlagDeleted == 0 {
			out = append(out, ix.RelPathCached(slot, cache))
		}
	}
	sort.Strings(out)
	return out
}

// fullIndex runs the MFT scanner over the fake drive and returns the saved-style index.
func fullIndex(t *testing.T, v *ntfstest.Volume) (*index.Index, *ntfs.MFTScanner) {
	t.Helper()
	sc := &ntfs.MFTScanner{Session: v.Session(), Root: `E:\`}
	b := index.NewBuilder(index.Header{SourceKind: index.KindVolume, SourceID: "vol-FAKE0001"})
	if err := sc.Scan(context.Background(), b, nil); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return b.Index(), sc
}

func TestMFTScanIndexesExactlyTheUserFiles(t *testing.T) {
	v, _ := populated()
	ix, sc := fullIndex(t, v)

	if got, want := sortedPaths(ix), v.Paths(); !reflect.DeepEqual(got, want) {
		t.Errorf("index differs from the drive:\n got %q\nwant %q", got, want)
	}
	for _, p := range sortedPaths(ix) {
		if strings.HasPrefix(p, "$") || strings.HasPrefix(strings.ToLower(p), "system volume") {
			t.Errorf("NTFS internals leaked into the index: %q", p)
		}
	}
	if sc.Name() != "mft" {
		t.Errorf("Name = %q", sc.Name())
	}
	st := sc.Stats()
	if st.Files != 3 || st.Dirs != 4 || sc.Build().Metafiles == 0 || sc.Build().Orphans == 0 {
		t.Errorf("stats = %+v build = %+v", st, sc.Build())
	}
	var _ scan.Scanner = sc
	var _ scan.StatsProvider = sc
}

func TestMFTScanRecordsTheJournalBookmark(t *testing.T) {
	v, _ := populated()
	ix, sc := fullIndex(t, v)
	if !sc.JournalActive() || ix.Header.UsnJournalID != v.JournalID || ix.Header.NextUsn == 0 {
		t.Errorf("header = %+v", ix.Header)
	}
	// An undrained drive without a journal still indexes (HighUsn = MaxInt64)... if NTFS allows it.
	v.JournalActive = false
	b := index.NewBuilder(index.Header{})
	err := (&ntfs.MFTScanner{Session: v.Session()}).Scan(context.Background(), b, nil)
	if !errors.Is(err, ntfs.ErrNoJournal) {
		t.Errorf("a drive whose enumeration reports a journal error must give ErrNoJournal, got %v", err)
	}
}

func TestEnumerationNeedsSeveralCallsAndStillGetsEverything(t *testing.T) {
	v, _ := populated()
	v.MaxRecordsPerCall = 3
	ix, _ := fullIndex(t, v)
	if got, want := sortedPaths(ix), v.Paths(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	if calls := v.Calls[ntfs.FsctlEnumUsnData]; calls < 5 {
		t.Errorf("expected many small enumeration calls, got %d", calls)
	}
}

func TestEnumerateMFTStopsAtEOFAndReportsProgress(t *testing.T) {
	v, _ := populated()
	var last int64
	calls := 0
	recs, err := ntfs.EnumerateMFT(context.Background(), v, math.MaxInt64, func(n int64) { calls++; last = n })
	if err != nil || len(recs) == 0 || calls == 0 || last != int64(len(recs)) {
		t.Errorf("recs=%d calls=%d last=%d err=%v", len(recs), calls, last, err)
	}
}

func TestEnumerateMFTCancellation(t *testing.T) {
	v, _ := populated()
	v.MaxRecordsPerCall = 1
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := ntfs.EnumerateMFT(ctx, v, math.MaxInt64, func(int64) {
		if calls++; calls == 2 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// A device that never makes progress (same "next" every time) must not hang us.
type stuckDevice struct{ calls int }

func (d *stuckDevice) IOControl(code uint32, in, out []byte) (int, error) {
	d.calls++
	if d.calls > 100 {
		return 0, errors.New("test: the loop never ended")
	}
	return 8, nil // "next FRN = 0, no records"
}

func TestEnumerateMFTGuardsAgainstNoProgress(t *testing.T) {
	d := &stuckDevice{}
	recs, err := ntfs.EnumerateMFT(context.Background(), d, math.MaxInt64, nil)
	if err != nil || len(recs) != 0 || d.calls > 2 {
		t.Errorf("recs=%d calls=%d err=%v", len(recs), d.calls, err)
	}
}

type shortDevice struct{}

func (shortDevice) IOControl(uint32, []byte, []byte) (int, error) { return 3, nil }

func TestShortAnswersAreErrorsNotPanics(t *testing.T) {
	if _, err := ntfs.EnumerateMFT(context.Background(), shortDevice{}, 0, nil); !errors.Is(err, ntfs.ErrBadRecord) {
		t.Errorf("enum: %v", err)
	}
	if _, err := ntfs.ReadJournal(context.Background(), shortDevice{}, 1, 0, func(ntfs.Record) {}); !errors.Is(err, ntfs.ErrBadRecord) {
		t.Errorf("read: %v", err)
	}
	if _, err := ntfs.QueryJournal(shortDevice{}); !errors.Is(err, ntfs.ErrBadRecord) {
		t.Errorf("query: %v", err)
	}
}

// ---- incremental refresh ---------------------------------------------------------

func refreshed(t *testing.T, v *ntfstest.Volume, ix *index.Index) ntfs.RefreshResult {
	t.Helper()
	res, err := ntfs.Refresh(context.Background(), v.Session(), ix)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return res
}

func TestRefreshReflectsCreateRenameMoveDelete(t *testing.T) {
	v, id := populated()
	ix, _ := fullIndex(t, v)
	bookmark := ix.Header.NextUsn

	v.Create(id["Docs"], "new file.txt")        // created
	v.Rename(id["readme"], "README (final).md") // renamed
	v.Move(id["Docs"], id["Archive"])           // a whole folder moved: Docs\* now lives under Archive
	v.Delete(id["img"])                         // deleted
	v.Touch(id["cv"])                           // plain data change: name unchanged
	extra := v.Mkdir(0, "Brand New")
	v.Create(extra, "inside.txt")

	res := refreshed(t, v, ix)

	if got, want := sortedPaths(ix), v.Paths(); !reflect.DeepEqual(got, want) {
		t.Errorf("after refresh the index must equal the drive:\n got %q\nwant %q", got, want)
	}
	// Spot checks of the interesting cases.
	got := strings.Join(sortedPaths(ix), "|")
	for _, want := range []string{`Archive\Docs\new file.txt`, `Archive\Docs\résumé.pdf`, `README (final).md`, `Brand New\inside.txt`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, gone := range []string{`img_001.jpg`, `readme.txt`} {
		if strings.Contains(got, gone) {
			t.Errorf("%q should be gone: %s", gone, got)
		}
	}
	if ix.Header.NextUsn <= bookmark {
		t.Errorf("bookmark must advance: %d -> %d", bookmark, ix.Header.NextUsn)
	}
	// 6 distinct entries changed: new file, readme, Docs, img, cv (touched), Brand New, inside.txt = 7
	if res.Changes != 7 || res.Records < res.Changes {
		t.Errorf("result = %+v, want 7 changes", res)
	}
}

func TestRefreshWithNoChangesIsCheapAndStable(t *testing.T) {
	v, _ := populated()
	ix, _ := fullIndex(t, v)
	before := sortedPaths(ix)
	res := refreshed(t, v, ix)
	if res.Changes != 0 || !reflect.DeepEqual(sortedPaths(ix), before) {
		t.Errorf("no-op refresh changed things: %+v", res)
	}
	// A second refresh right after the first also works (bookmark was saved correctly).
	v.Create(0, "later.txt")
	refreshed(t, v, ix)
	refreshed(t, v, ix)
	if got, want := sortedPaths(ix), v.Paths(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRefreshLoopsOverManyJournalBatches(t *testing.T) {
	v, _ := populated()
	ix, _ := fullIndex(t, v)
	v.MaxRecordsPerCall = 2 // forces many ReadJournal calls
	for i := 0; i < 40; i++ {
		v.Create(0, fmt.Sprintf("bulk %02d.txt", i))
	}
	refreshed(t, v, ix)
	if got, want := sortedPaths(ix), v.Paths(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %d paths, want %d", len(got), len(want))
	}
	if calls := v.Calls[ntfs.FsctlReadUsnJournal]; calls < 20 {
		t.Errorf("expected many journal reads, got %d", calls)
	}
}

func TestRefreshIgnoresSystemFolderChurn(t *testing.T) {
	v, _ := populated()
	ix, _ := fullIndex(t, v)
	// Windows writes to System Volume Information all the time.
	svi := v.Mkdir(0, "$RECYCLE.BIN")
	v.Create(svi, "S-1-5-21-0000")
	res := refreshed(t, v, ix)
	if res.Changes != 0 {
		t.Errorf("Changes = %d, want 0 (nothing visible changed)", res.Changes)
	}
	if got, want := sortedPaths(ix), v.Paths(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRefreshDemandsAFullRebuildWhenUnsafe(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(v *ntfstest.Volume, ix *index.Index)
		reason string
	}{
		{"journal was deleted and recreated", func(v *ntfstest.Volume, _ *index.Index) { v.ResetJournal() }, "reset"},
		{"journal history was overwritten", func(v *ntfstest.Volume, _ *index.Index) { v.Create(0, "x"); v.TrimJournal() }, "older than"},
		{"journal switched off", func(v *ntfstest.Volume, _ *index.Index) { v.JournalActive = false }, "not active"},
		{"index has no bookmark (built without a journal)", func(_ *ntfstest.Volume, ix *index.Index) { ix.Header.UsnJournalID = 0 }, "no journal bookmark"},
	}
	for _, c := range cases {
		v, _ := populated()
		ix, _ := fullIndex(t, v)
		c.break_(v, ix)
		_, err := ntfs.Refresh(context.Background(), v.Session(), ix)
		if !errors.Is(err, ntfs.ErrNeedFullRebuild) || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

func TestRefreshTurnsAMidReadTrimIntoAFullRebuild(t *testing.T) {
	// The bookmark looks valid when we check, but NTFS refuses the read.
	v, _ := populated()
	ix, _ := fullIndex(t, v)
	d := &trimOnRead{Volume: v}
	_, err := ntfs.Refresh(context.Background(), ntfs.NewSession(d, v.RootFRN, nil), ix)
	if !errors.Is(err, ntfs.ErrNeedFullRebuild) {
		t.Errorf("err = %v", err)
	}
}

type trimOnRead struct{ *ntfstest.Volume }

func (d *trimOnRead) IOControl(code uint32, in, out []byte) (int, error) {
	if code == ntfs.FsctlReadUsnJournal {
		return 0, fmt.Errorf("%w: simulated", ntfs.ErrJournalEntryDeleted)
	}
	return d.Volume.IOControl(code, in, out)
}

func TestRefreshCancellation(t *testing.T) {
	v, _ := populated()
	ix, _ := fullIndex(t, v)
	v.Create(0, "x")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ntfs.Refresh(ctx, v.Session(), ix); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestRefreshThenFullRebuildAgree(t *testing.T) {
	v, id := populated()
	ix, _ := fullIndex(t, v)
	v.Move(id["2024"], 0)
	v.Rename(id["Photos"], "Pictures")
	v.Delete(id["cv"])
	v.Create(id["Archive"], "a.zip")
	refreshed(t, v, ix)
	fresh, _ := fullIndex(t, v)
	if !reflect.DeepEqual(sortedPaths(ix), sortedPaths(fresh)) {
		t.Errorf("incremental and full rebuild disagree:\n incr %q\n full %q", sortedPaths(ix), sortedPaths(fresh))
	}
}

// ---- journal management ------------------------------------------------------------

func TestQueryAndCreateJournal(t *testing.T) {
	v := ntfstest.New()
	jd, err := ntfs.QueryJournal(v)
	if err != nil || jd.ID != v.JournalID || jd.NextUsn == 0 || jd.MaximumSize != 32<<20 {
		t.Errorf("jd = %+v err = %v", jd, err)
	}
	v.JournalActive = false
	if _, err := ntfs.QueryJournal(v); !errors.Is(err, ntfs.ErrJournalNotActive) {
		t.Errorf("inactive: %v", err)
	}
	if err := ntfs.CreateJournal(v, 32<<20, 4<<20); err != nil {
		t.Fatal(err)
	}
	if jd, err := ntfs.QueryJournal(v); err != nil || jd.ID == 0 {
		t.Errorf("after create: %+v %v", jd, err)
	}
	if v.Calls[ntfs.FsctlCreateUsnJournal] != 1 {
		t.Error("CreateJournal must send FSCTL_CREATE_USN_JOURNAL")
	}
}

func TestSessionCloseRunsTheCloser(t *testing.T) {
	v := ntfstest.New()
	s := v.Session()
	s.Close()
	s.Close() // closing twice must not crash (the fake counts calls)
	if v.Closed != 2 {
		t.Errorf("Closed = %d", v.Closed)
	}
	var nilSession *ntfs.Session
	nilSession.Close() // and a nil session is fine
}

// ---- metadata -----------------------------------------------------------------------

func TestFillMetadataFillsRealSizesAndDates(t *testing.T) {
	// A real folder on disk, indexed "NTFS style" (no metadata).
	dir := t.TempDir()
	write := func(rel string, size int) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/one.bin", 1111)
	write("a/b/two.bin", 22)
	write("three.bin", 333)

	ix := index.New()
	nm := index.FlagNoMeta
	ix.Add(index.Entry{ID: 1, Name: "a", Size: -1, Flags: nm | index.FlagDir})
	ix.Add(index.Entry{ID: 2, ParentID: 1, Name: "one.bin", Size: -1, Flags: nm})
	ix.Add(index.Entry{ID: 3, ParentID: 1, Name: "b", Size: -1, Flags: nm | index.FlagDir})
	ix.Add(index.Entry{ID: 4, ParentID: 3, Name: "two.bin", Size: -1, Flags: nm})
	ix.Add(index.Entry{ID: 5, Name: "three.bin", Size: -1, Flags: nm})
	ix.Add(index.Entry{ID: 6, Name: "vanished.bin", Size: -1, Flags: nm}) // not on disk

	var lastDone int64
	filled, missing, err := ntfs.FillMetadata(context.Background(), ix, dir, 3, func(done, total int64) { lastDone = done })
	if err != nil || filled != 5 || missing != 1 || lastDone != 6 {
		t.Fatalf("filled=%d missing=%d lastDone=%d err=%v", filled, missing, lastDone, err)
	}
	sizes := map[string]int64{}
	for s := 0; s < ix.Len(); s++ {
		sizes[ix.Names[s]] = ix.Sizes[s]
		if ix.Names[s] == "vanished.bin" {
			if ix.Flags[s]&index.FlagNoMeta == 0 || ix.Sizes[s] != -1 {
				t.Error("an unreadable entry must keep FlagNoMeta and unknown size")
			}
			continue
		}
		if ix.Flags[s]&index.FlagNoMeta != 0 || ix.MTimes[s] == 0 {
			t.Errorf("%s still lacks metadata", ix.Names[s])
		}
	}
	if sizes["one.bin"] != 1111 || sizes["two.bin"] != 22 || sizes["three.bin"] != 333 || sizes["a"] != 0 {
		t.Errorf("sizes = %v", sizes)
	}
}

func TestMFTScannerWithMetadata(t *testing.T) {
	v := ntfstest.New()
	v.Create(0, "file.txt") // does not exist on the real disk below
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0o644)

	sc := &ntfs.MFTScanner{Session: v.Session(), Root: dir, WithMetadata: true, Workers: 2}
	b := index.NewBuilder(index.Header{})
	if err := sc.Scan(context.Background(), b, func(scan.Progress) {}); err != nil {
		t.Fatal(err)
	}
	if filled, missing := sc.MetadataFilled(); filled != 1 || missing != 0 {
		t.Errorf("filled=%d missing=%d", filled, missing)
	}
	if ix := b.Index(); ix.Sizes[0] != 5 || ix.Flags[0]&index.FlagNoMeta != 0 {
		t.Errorf("entry = %+v", ix.Entry(0))
	}
}

func TestFillMetaOnOneEntry(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.txt"), []byte("1234567"), 0o644)
	ix := index.New()
	ix.Add(index.Entry{ID: 1, Name: "x.txt", Size: -1, Flags: index.FlagNoMeta})
	if !ix.FillMeta(0, filepath.Join(dir, "x.txt")) || ix.Sizes[0] != 7 || ix.Flags[0]&index.FlagNoMeta != 0 {
		t.Errorf("entry = %+v", ix.Entry(0))
	}
	if ix.FillMeta(0, filepath.Join(dir, "missing")) {
		t.Error("a missing file can't be filled")
	}
	if p := index.JoinRoot(dir, `a\b.txt`); p != filepath.Join(dir, "a", "b.txt") {
		t.Errorf("JoinRoot = %q", p)
	}
}

func TestFillMetadataCancellation(t *testing.T) {
	ix := index.New()
	for i := 0; i < 5000; i++ {
		ix.Add(index.Entry{ID: uint64(i + 1), Name: fmt.Sprintf("f%d", i), Size: -1, Flags: index.FlagNoMeta})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, _, err := ntfs.FillMetadata(ctx, ix, t.TempDir(), 4, nil); err == nil {
		t.Error("a cancelled context must stop the fill")
	}
	// nothing to do: instant, no error
	empty := index.New()
	if f, m, err := ntfs.FillMetadata(context.Background(), empty, "x", 4, nil); f != 0 || m != 0 || err != nil {
		t.Errorf("empty = %d %d %v", f, m, err)
	}
}
