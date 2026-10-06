package ntfs

import (
	"reflect"
	"sort"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
)

const root = uint64(0x0005000000000005) // the root folder's file reference number

func frn(record uint64) uint64 { return 1<<48 | record }

func rec(record, parentFRN uint64, name string, attrs uint32) Record {
	return Record{FRN: frn(record), ParentFRN: parentFRN, Name: name, Attrs: attrs}
}

func pathsOf(ix *index.Index) []string {
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

// ---- building an index from enumerated records --------------------------------

func TestAddRecordsFiltersMetafilesOrphansAndSystemFolders(t *testing.T) {
	recs := []Record{
		// NTFS metafiles - records below 24 - and a file inside one of them ($Extend).
		{FRN: 0x0001000000000000, ParentFRN: root, Name: "$MFT", Attrs: AttrHidden | AttrSystem},
		{FRN: 0x000B00000000000B, ParentFRN: root, Name: "$Extend", Attrs: AttrDirectory | AttrSystem},
		rec(30, 0x000B00000000000B, "$UsnJrnl", AttrSystem),
		// The root itself shows up in the MFT as record 5.
		{FRN: root, ParentFRN: root, Name: ".", Attrs: AttrDirectory},

		// Real content.
		rec(100, root, "Photos", AttrDirectory),
		rec(101, frn(100), "2024", AttrDirectory),
		rec(102, frn(101), "résumé.pdf", 0),
		rec(103, root, "readme.txt", AttrHidden),
		rec(104, root, "pagefile.sys", AttrHidden|AttrSystem),

		// A system folder to skip, with children and grandchildren that must vanish too.
		rec(200, root, "System Volume Information", AttrDirectory|AttrHidden|AttrSystem),
		rec(201, frn(200), "tracking.log", 0),
		rec(202, frn(200), "sub", AttrDirectory),
		rec(203, frn(202), "deep.dat", 0),
		rec(210, root, "$RECYCLE.BIN", AttrDirectory),
		rec(211, frn(210), "S-1-5-21", AttrDirectory),

		// A true orphan: its parent record doesn't exist at all.
		rec(300, frn(9999), "lost.txt", 0),
	}
	ix, st := BuildIndex(recs, root, nil, index.Header{})

	want := []string{"Photos", `Photos\2024`, `Photos\2024\résumé.pdf`, "pagefile.sys", "readme.txt"}
	if got := pathsOf(ix); !reflect.DeepEqual(got, want) {
		t.Errorf("paths:\n got %q\nwant %q", got, want)
	}
	// Metafiles: $MFT (record 0), $Extend (11) and the root's own "." record (5).
	if st.Files != 3 || st.Dirs != 2 || st.Metafiles != 3 {
		t.Errorf("stats = %+v", st)
	}
	// Orphans: $UsnJrnl (inside $Extend), three entries under System Volume Information,
	// one under $RECYCLE.BIN, and lost.txt whose parent doesn't exist.
	if st.Orphans != 6 {
		t.Errorf("orphans = %d, want 6", st.Orphans)
	}
}

func TestAddRecordsEntryFields(t *testing.T) {
	recs := []Record{
		rec(100, root, "Dir", AttrDirectory),
		rec(101, frn(100), "file.txt", AttrHidden),
		rec(102, root, "sys.bin", AttrSystem),
	}
	ix, _ := BuildIndex(recs, root, nil, index.Header{SourceID: "vol-X"})
	if ix.Header.SourceID != "vol-X" {
		t.Error("header must be kept")
	}
	by := map[string]index.Entry{}
	for s := 0; s < ix.Len(); s++ {
		by[ix.RelPath(s)] = ix.Entry(s)
	}

	dir, file := by["Dir"], by[`Dir\file.txt`]
	if dir.ID != frn(100) || dir.ParentID != 0 {
		t.Errorf("a folder directly in the root has parent 0 and ID = its FRN: %+v", dir)
	}
	if file.ID != frn(101) || file.ParentID != frn(100) {
		t.Errorf("file ID/parent = %+v", file)
	}
	for name, e := range by {
		if e.Size != -1 || e.ModTime != 0 || e.Flags&index.FlagNoMeta == 0 {
			t.Errorf("%s: USN records carry no size/date, want Size=-1 ModTime=0 FlagNoMeta: %+v", name, e)
		}
	}
	if dir.Flags&index.FlagDir == 0 || file.Flags&index.FlagDir != 0 {
		t.Error("FlagDir wrong")
	}
	if file.Flags&index.FlagHidden == 0 || by["sys.bin"].Flags&index.FlagSystem == 0 {
		t.Error("hidden/system flags not carried over")
	}
}

func TestAddRecordsSkipListIsCaseInsensitiveAndAtAnyDepth(t *testing.T) {
	recs := []Record{
		rec(100, root, "system volume information", AttrDirectory),
		rec(101, frn(100), "x", 0),
		rec(102, root, "Work", AttrDirectory),
		rec(103, frn(102), "$Recycle.Bin", AttrDirectory),
		rec(104, frn(103), "y", 0),
		rec(105, frn(102), "keep.txt", 0),
		// a FILE that happens to have a skip-list name is not a folder: kept
		rec(106, frn(102), "System Volume Information", 0),
	}
	ix, _ := BuildIndex(recs, root, nil, index.Header{})
	want := []string{"Work", `Work\System Volume Information`, `Work\keep.txt`}
	if got := pathsOf(ix); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	// A custom skip list REPLACES the default one: now "Work" (and everything in it)
	// is skipped, while System Volume Information is kept.
	ix, _ = BuildIndex(recs, root, []string{"Work"}, index.Header{})
	want = []string{"system volume information", `system volume information\x`}
	if got := pathsOf(ix); !reflect.DeepEqual(got, want) {
		t.Errorf("custom skip list: got %q want %q", got, want)
	}
}

func TestAddRecordsDeepChainsAndCycles(t *testing.T) {
	// A 3,000-folder deep chain resolves (each folder only resolved once).
	var recs []Record
	parent := root
	for i := 0; i < 3000; i++ {
		recs = append(recs, rec(uint64(100+i), parent, "d", AttrDirectory))
		parent = frn(uint64(100 + i))
	}
	// Shuffle order: children before parents must work too.
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	ix, st := BuildIndex(recs, root, nil, index.Header{})
	if ix.Len() != 3000 || st.Orphans != 0 {
		t.Errorf("deep chain: %d entries, %d orphans", ix.Len(), st.Orphans)
	}

	// Corrupt data: A inside B inside A. Both are dropped, and it terminates.
	cyc := []Record{rec(500, frn(501), "a", AttrDirectory), rec(501, frn(500), "b", AttrDirectory), rec(502, root, "ok", 0)}
	ix, st = BuildIndex(cyc, root, nil, index.Header{})
	if got := pathsOf(ix); !reflect.DeepEqual(got, []string{"ok"}) || st.Orphans != 2 {
		t.Errorf("cycle: %q, %+v", got, st)
	}
}

// ---- replaying the change journal -------------------------------------------------

// applyAll feeds journal records to an Applier.
func applyAll(ap *Applier, recs ...Record) {
	for _, r := range recs {
		ap.Apply(r)
	}
}

func journalRec(record, parentFRN uint64, name string, attrs, reason uint32) Record {
	r := rec(record, parentFRN, name, attrs)
	r.Reason = reason
	return r
}

// BUILD.md stage 6 test: create a file, rename it, move its parent folder into
// another folder, delete the file - checking RelPath and existence after each step.
func TestApplyDeltaCreateRenameMoveDelete(t *testing.T) {
	// Start: Docs\ and Archive\ in the root.
	ix, _ := BuildIndex([]Record{
		rec(100, root, "Docs", AttrDirectory),
		rec(101, root, "Archive", AttrDirectory),
	}, root, nil, index.Header{})
	ap := NewApplier(ix, root, nil)

	exists := func(want ...string) {
		t.Helper()
		sort.Strings(want)
		if got := pathsOf(ix); !reflect.DeepEqual(got, want) {
			t.Fatalf("paths:\n got %q\nwant %q", got, want)
		}
	}

	// 1. create Docs\report.txt
	applyAll(ap, journalRec(110, frn(100), "report.txt", 0, 0x100))
	exists("Archive", "Docs", `Docs\report.txt`)

	// 2. rename it (the journal records the OLD name first, then the NEW one)
	applyAll(ap,
		journalRec(110, frn(100), "report.txt", 0, 0x1000),
		journalRec(110, frn(100), "Final Report.txt", 0, 0x2000),
		journalRec(110, frn(100), "Final Report.txt", 0, 0x80002000),
	)
	exists("Archive", "Docs", `Docs\Final Report.txt`)
	if slot, _ := ix.Slot(frn(110)); ix.Lower[slot] != "final report.txt" {
		t.Error("the lowercase name used for searching must follow the rename")
	}

	// 3. move the PARENT folder: Docs goes into Archive. ONE record - the file's path follows.
	applyAll(ap, journalRec(100, frn(101), "Docs", AttrDirectory, 0x2000))
	exists("Archive", `Archive\Docs`, `Archive\Docs\Final Report.txt`)

	// 4. delete the file
	applyAll(ap, journalRec(110, frn(100), "Final Report.txt", 0, UsnReasonFileDelete|0x80000000))
	exists("Archive", `Archive\Docs`)
	if _, ok := ix.Slot(frn(110)); ok {
		t.Error("deleted file must not be found by ID")
	}

	// Seven journal records touched just two entries: the file (created, renamed,
	// deleted) and the folder (moved).
	if ap.Changes() != 2 {
		t.Errorf("Changes = %d, want 2 distinct entries", ap.Changes())
	}
}

func TestApplyDeltaIsIdempotent(t *testing.T) {
	ix, _ := BuildIndex([]Record{rec(100, root, "Docs", AttrDirectory)}, root, nil, index.Header{})
	r := journalRec(110, frn(100), "a.txt", 0, 0x100)
	ap := NewApplier(ix, root, nil)
	for i := 0; i < 3; i++ {
		ap.Apply(r)
	}
	if ix.Len() != 2 || ix.LiveLen() != 2 {
		t.Errorf("replaying the same record must not duplicate: len=%d", ix.Len())
	}
	// create + delete in one window, delete of an unknown file: both harmless
	ap.Apply(journalRec(110, frn(100), "a.txt", 0, UsnReasonFileDelete))
	ap.Apply(journalRec(110, frn(100), "a.txt", 0, UsnReasonFileDelete))
	ap.Apply(journalRec(999, frn(100), "never-seen", 0, UsnReasonFileDelete))
	if got := pathsOf(ix); !reflect.DeepEqual(got, []string{"Docs"}) {
		t.Errorf("got %q", got)
	}
}

func TestApplyDeltaIgnoresMetafilesAndSystemFolders(t *testing.T) {
	ix, _ := BuildIndex([]Record{rec(100, root, "Docs", AttrDirectory)}, root, nil, index.Header{})
	ap := NewApplier(ix, root, nil)
	applyAll(ap,
		// NTFS bookkeeping churns constantly
		Record{FRN: 0x0001000000000002, ParentFRN: root, Name: "$LogFile", Reason: 0x2},
		journalRec(30, frn(11), "$UsnJrnl", AttrSystem, 0x2),
		// System Volume Information is created and gets files: none of it may appear
		journalRec(200, root, "System Volume Information", AttrDirectory, 0x100),
		journalRec(201, frn(200), "tracking.log", 0, 0x100),
		journalRec(201, frn(200), "tracking.log", 0, 0x2),
	)
	if got := pathsOf(ix); !reflect.DeepEqual(got, []string{"Docs"}) {
		t.Errorf("got %q - system noise leaked into the index", got)
	}
	if ap.Changes() != 0 {
		t.Errorf("Changes = %d, want 0", ap.Changes())
	}
}

func TestApplyDeltaMovingIntoAHiddenFolderRemovesTheEntry(t *testing.T) {
	ix, _ := BuildIndex([]Record{
		rec(100, root, "Docs", AttrDirectory),
		rec(110, frn(100), "keep-me.txt", 0),
	}, root, nil, index.Header{})
	ap := NewApplier(ix, root, nil)
	// moved into the (unindexed) recycle bin: its new parent is unknown to the index
	applyAll(ap, journalRec(110, frn(9000), "keep-me.txt", 0, 0x2000))
	if got := pathsOf(ix); !reflect.DeepEqual(got, []string{"Docs"}) {
		t.Errorf("got %q", got)
	}
	if ap.Changes() != 1 {
		t.Errorf("Changes = %d, want 1", ap.Changes())
	}
}

func TestApplyDeltaRenamingToASkipNameRemovesTheFolder(t *testing.T) {
	ix, _ := BuildIndex([]Record{rec(100, root, "Docs", AttrDirectory), rec(101, frn(100), "x.txt", 0)}, root, nil, index.Header{})
	ap := NewApplier(ix, root, nil)
	applyAll(ap, journalRec(100, root, "$RECYCLE.BIN", AttrDirectory, 0x2000))
	// The folder is gone from the index; its child no longer has a parent either.
	if _, ok := ix.Slot(frn(100)); ok {
		t.Error("renamed-to-skip-name folder should be removed")
	}
}

func TestApplyDeltaFreeFunctionMatchesTheSpecSignature(t *testing.T) {
	ix := index.New()
	ApplyDelta(ix, journalRec(100, root, "a.txt", 0, 0x100), root)
	if ix.Len() != 1 || ix.RelPath(0) != "a.txt" {
		t.Errorf("len=%d", ix.Len())
	}
}
