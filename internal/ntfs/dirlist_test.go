package ntfs_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/ntfs/ntfstest"
	"github.com/yashwardhanv/dd/internal/scan"
)

const root = `E:\`

// turboIndex indexes the fake volume from its MFT (names only).
func turboIndex(t *testing.T, v *ntfstest.Volume) *index.Index {
	t.Helper()
	b := index.NewBuilder(index.Header{SourceKind: index.KindVolume, SourceID: "vol-1", FileSystem: "NTFS", Scanner: "mft"})
	sc := &ntfs.MFTScanner{Session: v.Session(), SkipDirs: scan.DefaultSkipDirs}
	if err := sc.Scan(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	ix := b.Index()
	ix.Header.SkipRules = scan.SkipRules(nil)
	return ix
}

func slotOf(t *testing.T, ix *index.Index, id uint64) int {
	t.Helper()
	s, ok := ix.Slot(id)
	if !ok {
		t.Fatalf("id %x not in the index", id)
	}
	return s
}

func TestFillMetadataByDirListsEachFolderOnce(t *testing.T) {
	v := ntfstest.New()
	photos := v.Mkdir(0, "Photos")
	a := v.Create(photos, "a.jpg")
	v.SetMeta(a, 1234, 1_700_000_000_123_456_700)
	locked := v.Mkdir(0, "Locked")
	hidden := v.Create(locked, "secret.txt")
	v.SetMeta(hidden, 5, 1)
	v.SetUnreadable(locked)
	top := v.Create(0, "top.txt")
	v.SetMeta(top, 9, 1_600_000_000e9)

	ix := turboIndex(t, v)
	lister := v.Lister(root)
	st, err := ntfs.FillMetadataByDir(context.Background(), ix, root, lister, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := slotOf(t, ix, a)
	if ix.Sizes[s] != 1234 || ix.ModTimeNs(s) != 1_700_000_000_123_456_700 || ix.Flags[s]&index.FlagNoMeta != 0 {
		t.Errorf("a.jpg: size %d time %d flags %b", ix.Sizes[s], ix.ModTimeNs(s), ix.Flags[s])
	}
	if s := slotOf(t, ix, hidden); ix.Flags[s]&index.FlagNoMeta == 0 {
		t.Error("a file in a folder that can't be opened must keep FlagNoMeta")
	}
	if st.Unreadble != 1 || st.Missing != 1 {
		t.Errorf("stats %+v, want 1 unreadable folder and 1 entry missing", st)
	}
	if ix.Header.MetaComplete || !ix.Header.ExactTimes() {
		t.Errorf("header: complete %v exact %v", ix.Header.MetaComplete, ix.Header.ExactTimes())
	}
	// Root, Photos and Locked: three listings for four entries - one per folder.
	if lister.ListCalls != 3 {
		t.Errorf("%d folder listings, want 3", lister.ListCalls)
	}
}

func TestRefreshTouchedAfterAJournalReplay(t *testing.T) {
	v := ntfstest.New()
	docs := v.Mkdir(0, "Docs")
	f := v.Create(docs, "report.docx")
	v.SetMeta(f, 100, 1_700_000_000e9)
	ix := turboIndex(t, v)
	ntfs.FillMetadataByDir(context.Background(), ix, root, v.Lister(root), 2, nil)

	// Edit the file and add 40 more in another folder.
	v.Write(f, 250, 1_700_000_500e9)
	bulk := v.Mkdir(0, "Bulk")
	for i := 0; i < 40; i++ {
		id := v.Create(bulk, "f"+string(rune('a'+i%26))+string(rune('0'+i/26)))
		v.SetMeta(id, int64(i), 1_700_000_900e9)
	}
	res, err := ntfs.Refresh(context.Background(), v.Session(), ix)
	if err != nil {
		t.Fatal(err)
	}
	lister := v.Lister(root)
	calls := 0
	_ = calls
	st, err := ntfs.RefreshTouched(context.Background(), ix, root, res.Touched, lister, 2)
	if err != nil {
		t.Fatal(err)
	}
	if s := slotOf(t, ix, f); ix.Sizes[s] != 250 || ix.MTimes[s] != 1_700_000_500 {
		t.Errorf("edited file: size %d time %d", ix.Sizes[s], ix.MTimes[s])
	}
	// Bulk has 40 changed entries: listed once. Docs and the file in it, and
	// Bulk itself: looked up one by one.
	if lister.ListCalls != 1 {
		t.Errorf("%d listings, want 1 (only the folder with 32+ changes)", lister.ListCalls)
	}
	if st.Missing != 0 || !ix.Header.MetaComplete {
		t.Errorf("after refresh: %+v complete %v", st, ix.Header.MetaComplete)
	}
}

// ---- freshness ------------------------------------------------------------------

type freshCase struct {
	v   *ntfstest.Volume
	ix  *index.Index
	env ntfs.FreshEnv
}

func newFresh(t *testing.T) *freshCase {
	t.Helper()
	v := ntfstest.New()
	d := v.Mkdir(0, "Photos")
	v.SetMeta(v.Create(d, "1.jpg"), 10, 1_700_000_000e9)
	ix := turboIndex(t, v)
	ntfs.FillMetadataByDir(context.Background(), ix, root, v.Lister(root), 2, nil)
	jd, _ := ntfs.QueryJournal(v)
	ix.Header.UsnJournalID, ix.Header.NextUsn = jd.ID, jd.NextUsn
	saved := 0
	return &freshCase{v: v, ix: ix, env: ntfs.FreshEnv{
		Mounted: true, SerialMatches: true, FileSystem: "NTFS", Elevated: true, Root: root,
		Open:   func(string) (*ntfs.Session, error) { return v.Session(), nil },
		Lister: v.Lister(root),
		Save:   func(*index.Index) error { saved++; return nil },
	}}
}

func TestFreshWhenNothingChanged(t *testing.T) {
	c := newFresh(t)
	res, err := ntfs.Fresh(context.Background(), c.ix, c.env)
	if err != nil || !res.Fresh || res.Why != "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestFreshPicksUpChangesFromTheJournal(t *testing.T) {
	c := newFresh(t)
	photos, _ := c.v.FRNOf("Photos")
	n := c.v.Create(photos, "2.jpg")
	c.v.SetMeta(n, 20, 1_700_000_100e9)
	c.v.Write(n, 20, 1_700_000_100e9)
	res, err := ntfs.Fresh(context.Background(), c.ix, c.env)
	if err != nil || !res.Fresh || res.Changes == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	s, ok := c.ix.Slot(n)
	if !ok || c.ix.Sizes[s] != 20 || c.ix.Flags[s]&index.FlagNoMeta != 0 {
		t.Errorf("new file not in the refreshed index with its size")
	}
}

// TestFreshFailurePaths: each condition that fails says why, in words the
// copy command prints ("index not usable: ...").
func TestFreshFailurePaths(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(c *freshCase)
		why    string
	}{
		{"folder index", func(c *freshCase) { c.ix.Header.SourceKind = index.KindFolder }, "not of a whole drive"},
		{"not NTFS now", func(c *freshCase) { c.env.FileSystem = "exFAT" }, "not NTFS"},
		{"unplugged", func(c *freshCase) { c.env.Mounted = false }, "not the one connected"},
		{"other drive", func(c *freshCase) { c.env.SerialMatches = false }, "not the one connected"},
		{"walker index", func(c *freshCase) { c.ix.Header.Scanner = "parallel" }, "turbo mode"},
		{"not admin", func(c *freshCase) { c.env.Elevated = false }, "not running as Administrator"},
		{"can't open", func(c *freshCase) {
			c.env.Open = func(string) (*ntfs.Session, error) { return nil, errors.New("access denied") }
		}, "can't be read"},
		{"no journal", func(c *freshCase) { c.v.JournalActive = false }, "no active change journal"},
		{"journal reset", func(c *freshCase) { c.v.ResetJournal() }, "was reset"},
		{"journal wrapped", func(c *freshCase) {
			photos, _ := c.v.FRNOf("Photos")
			c.v.Create(photos, "x.jpg")
			c.v.TrimJournal()
		}, "no longer reaches back"},
		{"old index", func(c *freshCase) { c.ix.Header.SkipRules = nil }, "older DriveDex"},
		{"extra skips", func(c *freshCase) { c.ix.Header.SkipRules = append(c.ix.Header.SkipRules, "node_modules") }, `"node_modules"`},
		{"save fails", func(c *freshCase) { c.env.Save = func(*index.Index) error { return errors.New("disk full") } }, "couldn't be saved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newFresh(t)
			tc.break_(c)
			res, err := ntfs.Fresh(context.Background(), c.ix, c.env)
			if err != nil {
				t.Fatal(err)
			}
			if res.Fresh || !strings.Contains(res.Why, tc.why) {
				t.Errorf("Fresh = %v, why %q; want it to say %q", res.Fresh, res.Why, tc.why)
			}
		})
	}
}

func TestFreshLooksUpAFewMissingEntriesButNotMany(t *testing.T) {
	c := newFresh(t)
	for s := 0; s < c.ix.Len(); s++ {
		c.ix.Flags[s] |= index.FlagNoMeta
	}
	res, _ := ntfs.Fresh(context.Background(), c.ix, c.env)
	if !res.Fresh {
		t.Fatalf("two entries without metadata are looked up: %+v", res)
	}
	for s := 0; s < c.ix.Len(); s++ {
		if c.ix.Flags[s]&index.FlagNoMeta != 0 {
			t.Errorf("%s still has no metadata", c.ix.Names[s])
		}
	}

	c = newFresh(t)
	photos, _ := c.v.FRNOf("Photos")
	for i := 0; i < 1001; i++ {
		c.ix.Add(index.Entry{ID: uint64(1<<40 + i), ParentID: photos, Name: "n", Size: -1, Flags: index.FlagNoMeta})
	}
	res, _ = ntfs.Fresh(context.Background(), c.ix, c.env)
	if res.Fresh || !strings.Contains(res.Why, "missing for many files") {
		t.Errorf("%+v", res)
	}
}

func TestDirInfoRoundTrip(t *testing.T) {
	in := []ntfs.DirRecord{
		{FileID: 0x0005000000001234, Name: "résumé.pdf", Size: 1 << 40, MTimeNs: 1_700_000_000_123_456_700, Attrs: 0x20},
		{FileID: 7, Name: "link", Attrs: ntfs.AttrReparse, ReparseTag: 0xA000000C},
		{FileID: 8, Name: "😀", Attrs: ntfs.AttrDirectory},
	}
	out, err := ntfs.ParseDirInfo(nil, ntfs.EncodeDirInfo(in))
	if err != nil || len(out) != 3 {
		t.Fatalf("%v %+v", err, out)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("record %d: %+v, want %+v", i, out[i], in[i])
		}
	}
}

func FuzzParseDirInfo(f *testing.F) {
	f.Add(ntfs.EncodeDirInfo([]ntfs.DirRecord{{FileID: 1, Name: "a"}, {FileID: 2, Name: "bb", Attrs: ntfs.AttrReparse}}))
	f.Add([]byte{})
	f.Add(make([]byte, 103))
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := ntfs.ParseDirInfo(nil, b)
		if err == nil {
			for _, r := range recs {
				if r.Name == "." || r.Name == ".." {
					t.Fatal("dot entries must be skipped")
				}
			}
		}
	})
}
