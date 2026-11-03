package index

import (
	"sort"
	"testing"
)

func smallTree() *Index {
	ix := New()
	ix.Add(Entry{ID: 1, ParentID: 0, Name: "DCIM", Flags: FlagDir})
	ix.Add(Entry{ID: 2, ParentID: 1, Name: "Camera", Flags: FlagDir})
	ix.Add(Entry{ID: 3, ParentID: 2, Name: "a.jpg", Size: 10, ModTime: 100})
	ix.Add(Entry{ID: 4, ParentID: 2, Name: "b.jpg", Size: 20, ModTime: 200})
	ix.Add(Entry{ID: 5, ParentID: 0, Name: "Music", Flags: FlagDir})
	ix.Add(Entry{ID: 6, ParentID: 5, Name: "x.mp3", Size: 30, ModTime: 300})
	return ix
}

func TestWalkFolder(t *testing.T) {
	ix := smallTree()
	ch := ix.BuildChildren()
	var got []string
	ix.WalkFolder(ch, 1, func(slot int, rel string) bool { got = append(got, rel); return true })
	sort.Strings(got)
	want := []string{`Camera`, `Camera\a.jpg`, `Camera\b.jpg`}
	if len(got) != len(want) {
		t.Fatalf("%q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%q, want %q", got, want)
		}
	}
	n := 0
	ix.WalkFolder(ch, 0, func(int, string) bool { n++; return true })
	if n != 6 {
		t.Errorf("whole index: %d entries", n)
	}
	// A cycle (corrupt data) ends.
	ix.Parents[0] = 2
	ch = ix.BuildChildren()
	ix.WalkFolder(ch, 2, func(int, string) bool { n++; return n < 1000 })
	if n >= 1000 {
		t.Error("walk looped on a cycle")
	}
}

func TestUpsertPaths(t *testing.T) {
	ix := smallTree()
	n := ix.UpsertPaths([]PathUpdate{
		{Rel: "DCIM/Camera/a.jpg", Size: 11, ModTime: 101}, // changed
		{Rel: `dcim\camera\B.JPG`, Size: 20, ModTime: 200}, // same (case-insensitive): no change
		{Rel: "DCIM/Camera/c.jpg", Size: 5, ModTime: 500},  // new file
		{Rel: "Download/new/d.pdf", Size: 7, ModTime: 700}, // new folders
		{Rel: "Music/x.mp3/oops", Size: 1, ModTime: 1},     // a file is in the way
	})
	if n != 3 {
		t.Errorf("changed %d, want 3", n)
	}
	ch := ix.BuildChildren()
	paths := map[string]int64{}
	ix.WalkFolder(ch, 0, func(slot int, rel string) bool {
		if ix.Flags[slot]&FlagDir == 0 {
			paths[rel] = ix.Sizes[slot]
		}
		return true
	})
	want := map[string]int64{`DCIM\Camera\a.jpg`: 11, `DCIM\Camera\b.jpg`: 20, `DCIM\Camera\c.jpg`: 5, `Download\new\d.pdf`: 7, `Music\x.mp3`: 30}
	if len(paths) != len(want) {
		t.Fatalf("%v", paths)
	}
	for k, v := range want {
		if paths[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, paths[k], v, paths)
		}
	}
	seen := map[uint64]bool{}
	for _, id := range ix.IDs {
		if seen[id] {
			t.Fatalf("duplicate ID %d", id)
		}
		seen[id] = true
	}
}
