package index

import "testing"

// small builds:  docs/ (1), docs/a.txt (2), docs/sub/ (3), docs/sub/b.txt (4), top.txt (5)
func small() *Index {
	ix := New()
	ix.Add(Entry{ID: 1, ParentID: 0, Name: "docs", Flags: FlagDir})
	ix.Add(Entry{ID: 2, ParentID: 1, Name: "a.txt", Size: 10})
	ix.Add(Entry{ID: 3, ParentID: 1, Name: "sub", Flags: FlagDir})
	ix.Add(Entry{ID: 4, ParentID: 3, Name: "B.TXT", Size: 20})
	ix.Add(Entry{ID: 5, ParentID: 0, Name: "top.txt", Size: 5})
	return ix
}

func slotOf(t *testing.T, ix *Index, id uint64) int {
	t.Helper()
	s, ok := ix.Slot(id)
	if !ok {
		t.Fatalf("id %d not found", id)
	}
	return s
}

func TestRelPath(t *testing.T) {
	ix := small()
	cases := map[uint64]string{1: "docs", 2: `docs\a.txt`, 4: `docs\sub\B.TXT`, 5: "top.txt"}
	cache := map[uint64]string{}
	for id, want := range cases {
		slot := slotOf(t, ix, id)
		if got := ix.RelPath(slot); got != want {
			t.Errorf("RelPath(%d) = %q, want %q", id, got, want)
		}
		if got := ix.RelPathCached(slot, cache); got != want {
			t.Errorf("RelPathCached(%d) = %q, want %q", id, got, want)
		}
	}
}

func TestRelPathOrphan(t *testing.T) {
	ix := New()
	ix.Add(Entry{ID: 10, ParentID: 999, Name: "lost.txt"}) // parent 999 does not exist
	if got := ix.RelPath(0); got != `?\lost.txt` {
		t.Errorf("orphan path = %q, want %q", got, `?\lost.txt`)
	}
}

func TestRelPathCycleDoesNotHang(t *testing.T) {
	ix := New()
	ix.Add(Entry{ID: 1, ParentID: 2, Name: "a", Flags: FlagDir})
	ix.Add(Entry{ID: 2, ParentID: 1, Name: "b", Flags: FlagDir})
	if got := ix.RelPath(0); got == "" {
		t.Error("expected some path for cyclic data")
	}
}

func TestLowerIsMaintained(t *testing.T) {
	ix := small()
	if ix.Lower[3] != "b.txt" {
		t.Errorf("Lower = %q, want b.txt", ix.Lower[3])
	}
	ix.Names = append([]string(nil), ix.Names...)
	ix.fillLower()
	if ix.Lower[3] != "b.txt" {
		t.Errorf("fillLower = %q, want b.txt", ix.Lower[3])
	}
}

func TestUpsertDeleteCompact(t *testing.T) {
	ix := small()

	// Rename file 2 and move it under "sub" - one Upsert does both.
	ix.Upsert(Entry{ID: 2, ParentID: 3, Name: "Renamed.txt", Size: -1, Flags: FlagNoMeta})
	if got := ix.RelPath(slotOf(t, ix, 2)); got != `docs\sub\Renamed.txt` {
		t.Errorf("after rename+move = %q", got)
	}
	if ix.Lower[slotOf(t, ix, 2)] != "renamed.txt" {
		t.Error("Upsert did not refresh Lower")
	}

	// Upsert of an unknown ID adds it.
	ix.Upsert(Entry{ID: 6, ParentID: 0, Name: "new.txt"})
	if ix.Len() != 6 {
		t.Errorf("Len = %d, want 6", ix.Len())
	}

	// Delete tombstones; Compact removes.
	if !ix.Delete(5) {
		t.Error("Delete(5) = false")
	}
	if ix.Delete(5) {
		t.Error("second Delete(5) should be false")
	}
	if ix.Len() != 6 || ix.LiveLen() != 5 {
		t.Errorf("Len/LiveLen = %d/%d, want 6/5", ix.Len(), ix.LiveLen())
	}
	if _, ok := ix.Slot(5); ok {
		t.Error("deleted ID still resolvable")
	}
	ix.Compact()
	if ix.Len() != 5 || ix.LiveLen() != 5 {
		t.Errorf("after Compact Len/LiveLen = %d/%d, want 5/5", ix.Len(), ix.LiveLen())
	}
	// Paths still resolve after slots moved.
	if got := ix.RelPath(slotOf(t, ix, 4)); got != `docs\sub\B.TXT` {
		t.Errorf("after compact = %q", got)
	}
	// Recreating a deleted ID works.
	ix.Upsert(Entry{ID: 5, ParentID: 0, Name: "top.txt"})
	if ix.Len() != 6 {
		t.Errorf("recreated entry not added, Len = %d", ix.Len())
	}
}

func TestDiscardBuilderCountsOnly(t *testing.T) {
	b := NewDiscardBuilder()
	b.Add(Entry{ID: 1, Name: "x"})
	b.Add(Entry{ID: 2, Name: "y"})
	if b.Len() != 2 || b.Index().Len() != 0 {
		t.Errorf("Len=%d stored=%d, want 2 and 0", b.Len(), b.Index().Len())
	}
}
