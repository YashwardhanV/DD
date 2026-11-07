package copier

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/manifest"
	"github.com/yashwardhanv/dd/internal/trash"
)

const testSrcID = "dir-test"

// tree returns rel path -> content of every file under root (skipping the
// manifest folder).
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == manifest.DirName {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

func contents(m map[string]string) map[string]int {
	c := map[string]int{}
	for _, v := range m {
		c[v]++
	}
	return c
}

// put writes root\rel (rel may use `\` on any OS, as the tests write it).
func put(t *testing.T, root, rel, body string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(strings.ReplaceAll(rel, `\`, "/")))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mtime, mtime)
}

// crash points for syncOnce.
type crash struct {
	afterCopies  int64 // cancel after this many files are in place (0 = never)
	afterRemoves int64 // cancel after this many removals (0 = never)
}

// syncOnce runs the sync steps the CLI runs (manifest, compare, PlanSync,
// moves, copies, removals, adoption), "crashing" at the given point: the
// context is cancelled and the manifest abandoned without compacting, the
// trash run left without its run.json.
func syncOnce(t *testing.T, src, dst string, cr crash) (crashed bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := PlanLocal(ctx, src, dst, OnConflictOverwrite, 4)
	if err != nil {
		t.Fatal(err)
	}
	p.FixNames(NameOptions{})
	s, err := manifest.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	m := s.Manifest()
	if _, err := p.Compare(ctx, CompareOptions{Mode: CompareFast, Manifest: m, SrcID: testSrcID}); err != nil {
		t.Fatal(err)
	}
	sp, err := PlanSync(ctx, p, SyncOptions{Manifest: m, SrcID: testSrcID, DetectMoves: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := trash.Begin(dst, "sync", src, time.Now())
	record := func(it *Item) {
		s.Put(manifest.Record{Rel: it.Rel, SrcID: testSrcID, SrcSize: it.SrcSize, SrcMTimeNs: it.SrcMTimeNs,
			DstSize: it.Dst.Size, DstMTimeNs: it.Dst.MTimeNs, Hash: it.Hash, HashKind: manifest.HashXXH3})
	}
	sp.ApplyMoves(func(it *Item, from string) {
		rec, ok := m.Get(from)
		s.Delete(from)
		if ok {
			rec.Rel, rec.DstSize, rec.DstMTimeNs = it.Rel, it.Dst.Size, it.Dst.MTimeNs
			s.Put(rec)
		}
	})

	var copies atomic.Int64
	afterCopy = func(string) {
		if cr.afterCopies > 0 && copies.Add(1) == cr.afterCopies {
			cancel()
		}
	}
	defer func() { afterCopy = nil }()
	res, err := Run(ctx, p, Options{Workers: 2, Hash: true, Trash: tr, Record: record})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cancelled {
		s.Abandon()
		return true
	}
	var removes atomic.Int64
	counting := trasherFunc(func(rel, why string) error {
		if err := tr.Move(rel, why); err != nil {
			return err
		}
		if cr.afterRemoves > 0 && removes.Add(1) == cr.afterRemoves {
			cancel()
		}
		return nil
	})
	if _, _, err := sp.ApplyRemovals(ctx, counting, func(rel string) { s.Delete(rel) }); err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		s.Abandon()
		return true
	}
	for _, rel := range sp.Gone {
		s.Delete(rel)
	}
	for i := range p.Items {
		it := &p.Items[i]
		if _, ok := m.Get(it.Rel); !ok && it.Decision == SkipSame {
			s.Put(manifest.Record{Rel: it.Rel, SrcID: testSrcID, SrcSize: it.Size, SrcMTimeNs: it.MTimeNs(),
				DstSize: it.Dst.Size, DstMTimeNs: it.Dst.MTimeNs, Flags: manifest.FlagAdopted})
		}
	}
	tr.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return false
}

type trasherFunc func(rel, why string) error

func (f trasherFunc) Move(rel, why string) error { return f(rel, why) }

// trashContents is every file content in the destination's trash.
func trashContents(t *testing.T, dst string) map[string]int {
	c := map[string]int{}
	filepath.WalkDir(trash.Dir(dst), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() != trash.RunFile {
			b, _ := os.ReadFile(p)
			c[string(b)]++
		}
		return nil
	})
	return c
}

// TestINV3_CrashedSyncConvergesAndLosesNothing: a sync stopped at random
// points (during the copies or the removals, the manifest left unflushed)
// and run again ends in an exact mirror, and every file the mirror held
// before is still in the mirror or in the trash.
func TestINV3_CrashedSyncConvergesAndLosesNothing(t *testing.T) {
	var crashes atomic.Int64
	defer func() {
		if crashes.Load() < 6 {
			t.Errorf("only %d of 12 runs crashed: the test isn't testing much", crashes.Load())
		}
	}()
	for seed := int64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			base := t.TempDir()
			src, dst := filepath.Join(base, "Photos"), filepath.Join(base, "backup")
			t0 := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
			var names []string
			for i := 0; i < 40; i++ {
				rel := filepath.Join(fmt.Sprintf("d%d", i%5), fmt.Sprintf("f%02d.jpg", i))
				names = append(names, rel)
				put(t, src, rel, fmt.Sprintf("v1 of %s %d", rel, rng.Int()), t0.Add(time.Duration(i)*time.Minute))
			}
			if syncOnce(t, src, dst, crash{}) {
				t.Fatal("the first sync must not crash")
			}

			// Change the source: edit, delete, add, move.
			for i, rel := range names {
				switch rng.Intn(6) {
				case 0:
					put(t, src, rel, fmt.Sprintf("v2 of %s %d", rel, rng.Int()), t0.Add(time.Hour+time.Duration(i)*time.Minute))
				case 1:
					os.Remove(filepath.Join(src, rel))
				case 2:
					to := filepath.Join(src, "moved", filepath.Base(rel))
					os.MkdirAll(filepath.Dir(to), 0o755)
					os.Rename(filepath.Join(src, rel), to)
				}
			}
			for i := 0; i < 5; i++ {
				put(t, src, filepath.Join("new", fmt.Sprintf("n%d.jpg", i)), fmt.Sprintf("new %d %d", i, rng.Int()), t0)
			}

			before := contents(tree(t, filepath.Join(dst, "Photos")))
			cr := crash{afterCopies: int64(1 + rng.Intn(8))}
			if rng.Intn(2) == 0 {
				cr = crash{afterRemoves: int64(1 + rng.Intn(3))}
			}
			if syncOnce(t, src, dst, cr) {
				crashes.Add(1)
			}
			if syncOnce(t, src, dst, crash{}) {
				t.Fatal("the second run must finish")
			}

			want, got := tree(t, src), tree(t, filepath.Join(dst, "Photos"))
			if fmt.Sprint(sorted(want)) != fmt.Sprint(sorted(got)) {
				t.Fatalf("not a mirror:\nsource %v\nmirror %v", sorted(want), sorted(got))
			}
			kept := trashContents(t, dst)
			now := contents(got)
			for c, n := range before {
				if now[c]+kept[c] < n {
					t.Errorf("lost a file: %q", c)
				}
			}
		})
	}
}

func sorted(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func syncPlanFor(t *testing.T, src, dst string, m *manifest.Manifest) *SyncPlan {
	t.Helper()
	p, err := PlanLocal(context.Background(), src, dst, OnConflictOverwrite, 2)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := PlanSync(context.Background(), p, SyncOptions{Manifest: m, SrcID: testSrcID, DetectMoves: true})
	if err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestSyncAdoptsAnExistingFolderWithoutDeleting(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "Docs"), filepath.Join(base, "out")
	t0 := time.Now().Add(-time.Hour)
	put(t, src, "a.txt", "a", t0)
	put(t, dst, `Docs\mine.txt`, "someone else's", t0)
	sp := syncPlanFor(t, src, dst, manifest.New())
	if !sp.Adopt || len(sp.Removals) != 0 {
		t.Fatalf("adopt %v, removals %v", sp.Adopt, sp.Removals)
	}
}

func TestSyncRemovesOnlyRecordedFilesOfThisSource(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "Docs"), filepath.Join(base, "out")
	t0 := time.Now().Add(-time.Hour)
	put(t, src, "keep.txt", "k", t0)
	for _, rel := range []string{`Docs\keep.txt`, `Docs\gone.txt`, `Docs\other.txt`, `Docs\stranger.txt`} {
		put(t, dst, rel, rel, t0)
	}
	m := manifest.New()
	m.Put(manifest.Record{Rel: `Docs\keep.txt`, SrcID: testSrcID})
	m.Put(manifest.Record{Rel: `Docs\gone.txt`, SrcID: testSrcID, DstSize: 12})
	m.Put(manifest.Record{Rel: `Docs\other.txt`, SrcID: "vol-OTHER"})
	m.Put(manifest.Record{Rel: `Docs\vanished.txt`, SrcID: testSrcID}) // deleted from the mirror by hand
	sp := syncPlanFor(t, src, dst, m)
	if sp.Adopt || len(sp.Removals) != 1 || sp.Removals[0].Rel != filepath.Join("Docs", "gone.txt") {
		t.Fatalf("removals = %+v (adopt %v)", sp.Removals, sp.Adopt)
	}
	if len(sp.Gone) != 1 {
		t.Errorf("gone = %v", sp.Gone)
	}
}

func TestSyncMassDeleteGuard(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "Docs"), filepath.Join(base, "out")
	t0 := time.Now().Add(-time.Hour)
	put(t, src, "one.txt", "1", t0)
	m := manifest.New()
	for i := 0; i < 4; i++ {
		rel := fmt.Sprintf(`Docs\f%d.txt`, i)
		put(t, dst, rel, "x", t0)
		m.Put(manifest.Record{Rel: rel, SrcID: testSrcID})
	}
	if sp := syncPlanFor(t, src, dst, m); !sp.MassDelete || len(sp.Removals) != 4 {
		t.Fatalf("4 of 4 mirrored files removed must trip the guard: %+v", sp.Removals)
	}
}

func TestSyncPairsMovesByNameSizeAndTimeOnlyWhenUnique(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "Docs"), filepath.Join(base, "out")
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	put(t, src, `new\a.txt`, "aaa", t0) // moved from Docs\a.txt
	put(t, src, `x\dup.txt`, "dd", t0)  // two candidates: ambiguous
	put(t, src, `y\dup.txt`, "dd", t0)  //
	put(t, dst, `Docs\a.txt`, "aaa", t0)
	put(t, dst, `Docs\dup.txt`, "dd", t0)
	m := manifest.New()
	m.Put(manifest.Record{Rel: `Docs\a.txt`, SrcID: testSrcID, SrcSize: 3, SrcMTimeNs: t0.UnixNano(), DstSize: 3})
	m.Put(manifest.Record{Rel: `Docs\dup.txt`, SrcID: testSrcID, SrcSize: 2, SrcMTimeNs: t0.UnixNano(), DstSize: 2})
	sp := syncPlanFor(t, src, dst, m)
	if len(sp.Moves) != 1 || sp.Moves[0].MoveFrom != filepath.Join("Docs", "a.txt") {
		t.Fatalf("moves = %v", sp.Moves)
	}
	if len(sp.Removals) != 1 || !strings.HasSuffix(sp.Removals[0].Rel, "dup.txt") {
		t.Fatalf("the ambiguous dup.txt is removed and both new ones copied: %+v", sp.Removals)
	}
	n, err := sp.ApplyMoves(nil)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "Docs", "new", "a.txt")); string(b) != "aaa" {
		t.Error("the move must be a rename on the destination")
	}
}

func TestSyncRefusesAnEmptySource(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "Empty")
	os.MkdirAll(src, 0o755)
	p, _ := PlanLocal(context.Background(), src, filepath.Join(base, "out"), OnConflictOverwrite, 2)
	if _, err := PlanSync(context.Background(), p, SyncOptions{Manifest: manifest.New(), SrcID: testSrcID}); err != ErrEmptySource {
		t.Fatalf("err = %v", err)
	}
}

func TestCompareStrictHasNoDaylightSavingTolerance(t *testing.T) {
	fat := fsinfo.ForFileSystem("FAT32")
	p := &Plan{OnConflict: OnConflictNewer, SrcFS: fsinfo.ForFileSystem("NTFS"), DstFS: fat, Items: []Item{{
		Job:      Job{Rel: "a.jpg", Size: 10, ModTime: 1_700_003_600},
		Decision: SkipSame, DSTShift: true, Dst: DstState{Exists: true, Size: 10, MTimeNs: 1_700_000_000e9},
	}}}
	if _, err := p.Compare(context.Background(), CompareOptions{Mode: CompareStrict}); err != nil {
		t.Fatal(err)
	}
	if p.Items[0].Decision != Overwrite {
		t.Errorf("strict: decision %v, want overwrite", p.Items[0].Decision)
	}
}

func TestCompareChecksum(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "S"), filepath.Join(base, "D")
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	put(t, src, "same.txt", "abc", t0)
	put(t, dst, `S\same.txt`, "abc", t0.Add(-time.Hour)) // older, same content: no need to overwrite
	put(t, src, "edited.txt", "new", t0)
	put(t, dst, `S\edited.txt`, "old", t0) // same size and time, other content
	p, _ := PlanLocal(context.Background(), src, dst, OnConflictNewer, 2)
	rep, err := p.Compare(context.Background(), CompareOptions{Mode: CompareChecksum})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Items {
		switch filepath.Base(it.Rel) {
		case "same.txt":
			if it.Decision != SkipSame || it.Why != WhyChecksum {
				t.Errorf("same.txt: %v %v", it.Decision, it.Why)
			}
		case "edited.txt":
			if it.Decision != Overwrite || !it.Changed {
				t.Errorf("edited.txt: %v changed=%v", it.Decision, it.Changed)
			}
		}
	}
	if rep.BySum != 1 || rep.Changed != 1 || rep.Hashed != 4 {
		t.Errorf("report = %+v", rep)
	}
}

// TestINV1_JournalCatchesAnEditWithTheOldTime: the journal comparison
// re-copies a file whose size and time look unchanged when the journal
// recorded a write, and confirms the others.
func TestINV1_JournalCatchesAnEditWithTheOldTime(t *testing.T) {
	t0 := int64(1_700_000_000e9)
	m := manifest.New()
	items := []Item{
		{Job: Job{Rel: "edited.txt", Size: 5, ModTimeNs: t0, SrcFileID: 11}, Decision: SkipSame, Dst: DstState{Exists: true, Size: 5, MTimeNs: t0}},
		{Job: Job{Rel: "same.txt", Size: 5, ModTimeNs: t0, SrcFileID: 12}, Decision: SkipSame, Dst: DstState{Exists: true, Size: 5, MTimeNs: t0}},
		{Job: Job{Rel: "touchedcopy.txt", Size: 5, ModTimeNs: t0, SrcFileID: 13}, Decision: SkipSame, Dst: DstState{Exists: true, Size: 5, MTimeNs: t0 + 1}},
	}
	for _, it := range items {
		m.Put(manifest.Record{Rel: it.Rel, SrcID: testSrcID, SrcFileID: it.SrcFileID, SrcSize: 5, SrcMTimeNs: t0, DstSize: 5, DstMTimeNs: t0, JournalID: 7, SrcUSN: 100})
	}
	p := &Plan{OnConflict: OnConflictOverwrite, Items: items}
	rep, err := p.Compare(context.Background(), CompareOptions{Mode: CompareAuto, Manifest: m, SrcID: testSrcID,
		Journal: &JournalView{ID: 7, Next: 500, Since: 100, Touched: map[uint64]int64{11: 200}}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Items[0].Decision != Overwrite || !p.Items[0].Changed {
		t.Errorf("edited.txt must be copied again: %+v", p.Items[0].Decision)
	}
	if p.Items[1].Decision != SkipSame || p.Items[1].Why != WhyJournal {
		t.Errorf("same.txt: %v %v", p.Items[1].Decision, p.Items[1].Why)
	}
	if !p.Items[2].DstModified || p.Items[2].Why == WhyJournal {
		t.Errorf("a destination that differs from its record is not journal-proven")
	}
	if rep.ByJournal != 1 || rep.Changed != 1 {
		t.Errorf("report = %+v", rep)
	}
	// A wrapped journal: nothing is journal-proven, and the report says why.
	p = &Plan{OnConflict: OnConflictOverwrite, Items: []Item{items[1]}}
	p.Items[0].Why = WhySizeTime
	rep, _ = p.Compare(context.Background(), CompareOptions{Mode: CompareJournal, Manifest: m, SrcID: testSrcID, Journal: &JournalView{ID: 7}})
	if rep.JournalNote == "" || p.Items[0].Why == WhyJournal {
		t.Errorf("wrapped journal: %+v", rep)
	}
}

// TestJournalJudgesEachFileByItsOwnBookmark: a change the journal saw before
// a file's own bookmark (it was copied after that change) doesn't make it
// "changed", even when an older record pulls the journal window back.
func TestJournalJudgesEachFileByItsOwnBookmark(t *testing.T) {
	t0 := int64(1_700_000_000e9)
	m := manifest.New()
	items := []Item{
		{Job: Job{Rel: "old.txt", Size: 5, ModTimeNs: t0, SrcFileID: 21}, Decision: SkipSame, Dst: DstState{Exists: true, Size: 5, MTimeNs: t0}},
		{Job: Job{Rel: "copiedlater.txt", Size: 5, ModTimeNs: t0, SrcFileID: 22}, Decision: SkipSame, Dst: DstState{Exists: true, Size: 5, MTimeNs: t0}},
	}
	m.Put(manifest.Record{Rel: "old.txt", SrcID: testSrcID, SrcFileID: 21, SrcSize: 5, SrcMTimeNs: t0, DstSize: 5, DstMTimeNs: t0, JournalID: 7, SrcUSN: 100})
	m.Put(manifest.Record{Rel: "copiedlater.txt", SrcID: testSrcID, SrcFileID: 22, SrcSize: 5, SrcMTimeNs: t0, DstSize: 5, DstMTimeNs: t0, JournalID: 7, SrcUSN: 300})
	p := &Plan{OnConflict: OnConflictNewer, Items: items}
	jv := &JournalView{ID: 7, Next: 500, Since: 100, Touched: map[uint64]int64{22: 250}} // created at 250, copied at 300
	if _, err := p.Compare(context.Background(), CompareOptions{Mode: CompareAuto, Manifest: m, SrcID: testSrcID, Journal: jv}); err != nil {
		t.Fatal(err)
	}
	for _, it := range p.Items {
		if it.Decision != SkipSame || it.Why != WhyJournal {
			t.Errorf("%s: %v %v, want skipped as unchanged (journal)", it.Rel, it.Decision, it.Why)
		}
	}
}

// TestCompareNeverOverridesOnConflictSkip: --on-conflict skip means existing
// files are never replaced, whatever the journal or a checksum says.
func TestCompareNeverOverridesOnConflictSkip(t *testing.T) {
	t0 := int64(1_700_000_000e9)
	m := manifest.New()
	m.Put(manifest.Record{Rel: "a.txt", SrcID: testSrcID, SrcFileID: 31, SrcSize: 5, SrcMTimeNs: t0, DstSize: 5, DstMTimeNs: t0, JournalID: 7, SrcUSN: 100})
	for _, it := range []Item{
		{Job: Job{Rel: "a.txt", Size: 5, ModTimeNs: t0, SrcFileID: 31}, Decision: SkipSame, Dst: DstState{Exists: true, Size: 5, MTimeNs: t0}},
		{Job: Job{Rel: "a.txt", Size: 5, ModTimeNs: t0 + 1e9, SrcFileID: 31}, Decision: SkipPolicy, Dst: DstState{Exists: true, Size: 5, MTimeNs: t0}},
	} {
		for _, mode := range []string{CompareAuto, CompareChecksum} {
			p := &Plan{OnConflict: OnConflictSkip, Items: []Item{it}}
			jv := &JournalView{ID: 7, Next: 500, Since: 100, Touched: map[uint64]int64{31: 200}}
			if _, err := p.Compare(context.Background(), CompareOptions{Mode: mode, Manifest: m, SrcID: testSrcID, Journal: jv}); err != nil {
				t.Fatal(err)
			}
			if !p.Items[0].Decision.Skipped() {
				t.Errorf("%s, %v: became %v", mode, it.Decision, p.Items[0].Decision)
			}
		}
	}
}

func TestOverwriteKeepsTheOldVersionInTheTrash(t *testing.T) {
	base := t.TempDir()
	src, dst := filepath.Join(base, "S"), filepath.Join(base, "D")
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	put(t, dst, `S\a.txt`, "old", t0)
	put(t, src, "a.txt", "newer", t0.Add(time.Hour))
	p, _ := PlanLocal(context.Background(), src, dst, OnConflictNewer, 2)
	tr := trash.Begin(dst, "copy", src, time.Now())
	if _, err := Run(context.Background(), p, Options{Trash: tr}); err != nil {
		t.Fatal(err)
	}
	tr.Close()
	if b, _ := os.ReadFile(filepath.Join(dst, "S", "a.txt")); string(b) != "newer" {
		t.Errorf("copy: %q", b)
	}
	if c := trashContents(t, dst); c["old"] != 1 {
		t.Errorf("the old version must be in the trash: %v", c)
	}
}

func TestManifestFolderInTheSourceIsNotCopied(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "Backup")
	put(t, src, "a.txt", "a", time.Now())
	put(t, src, `.drivedex\manifest.ddm`, "x", time.Now())
	p, _ := PlanLocal(context.Background(), src, filepath.Join(base, "out"), "", 2)
	if len(p.Items) != 1 {
		t.Errorf("planned %d files, want only a.txt", len(p.Items))
	}
}
