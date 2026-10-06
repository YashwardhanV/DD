package index

import (
	"path/filepath"
	"reflect"
	"testing"
)

// tree:  Photos/ (1)  Photos/2024/ (2)  Photos/2024/a.jpg (3)  Photos/old.jpg (4)
//
//	Docs/ (5)  Docs/Photos/ (6)  Docs/Photos/b.jpg (7)  top.jpg (8)
func scopeTree() *Index {
	ix := New()
	ix.Header = Header{SourceID: "vol-AAAAAAAA", SourceKind: KindVolume}
	ix.Add(Entry{ID: 1, Name: "Photos", Flags: FlagDir})
	ix.Add(Entry{ID: 2, ParentID: 1, Name: "2024", Flags: FlagDir})
	ix.Add(Entry{ID: 3, ParentID: 2, Name: "a.jpg"})
	ix.Add(Entry{ID: 4, ParentID: 1, Name: "old.jpg"})
	ix.Add(Entry{ID: 5, Name: "Docs", Flags: FlagDir})
	ix.Add(Entry{ID: 6, ParentID: 5, Name: "Photos", Flags: FlagDir})
	ix.Add(Entry{ID: 7, ParentID: 6, Name: "b.jpg"})
	ix.Add(Entry{ID: 8, Name: "top.jpg"})
	return ix
}

func TestFindFolder(t *testing.T) {
	ix := scopeTree()
	cases := map[string]struct {
		id uint64
		ok bool
	}{
		"": {0, true}, `\`: {0, true}, "Photos": {1, true}, "photos": {1, true}, `Photos\2024`: {2, true},
		"PHOTOS/2024": {2, true}, `Docs\Photos`: {6, true},
		"Nope": {0, false}, `Photos\Nope`: {0, false}, "top.jpg": {0, false}, // a file is not a folder
	}
	for rel, want := range cases {
		id, ok := ix.FindFolder(rel)
		if id != want.id || ok != want.ok {
			t.Errorf("FindFolder(%q) = %d,%v want %d,%v", rel, id, ok, want.id, want.ok)
		}
	}
	ix.Delete(2)
	if _, ok := ix.FindFolder(`Photos\2024`); ok {
		t.Error("deleted folders can't be found")
	}
}

func TestUnder(t *testing.T) {
	ix := scopeTree()
	slot := func(id uint64) int { s, _ := ix.Slot(id); return s }
	yes := [][2]uint64{{3, 1}, {3, 2}, {4, 1}, {7, 5}, {7, 6}, {2, 1}}
	no := [][2]uint64{{3, 5}, {4, 2}, {7, 1}, {8, 1}, {1, 1}, {1, 5}}
	for _, c := range yes {
		if !ix.Under(slot(c[0]), c[1]) {
			t.Errorf("entry %d should be under folder %d", c[0], c[1])
		}
	}
	for _, c := range no {
		if ix.Under(slot(c[0]), c[1]) {
			t.Errorf("entry %d should NOT be under folder %d", c[0], c[1])
		}
	}
	if !ix.Under(slot(8), 0) {
		t.Error("folder 0 means the whole source")
	}
	// A cycle must terminate.
	cyc := New()
	cyc.Add(Entry{ID: 1, ParentID: 2, Name: "a"})
	cyc.Add(Entry{ID: 2, ParentID: 1, Name: "b"})
	if cyc.Under(0, 99) {
		t.Error("cycle")
	}
}

func TestScopeMatcherRestrictsResults(t *testing.T) {
	ix := scopeTree()
	other := New()
	other.Header = Header{SourceID: "vol-BBBBBBBB", SourceKind: KindVolume}
	other.Add(Entry{ID: 1, Name: "jpg-elsewhere.jpg"})
	ixs := []*Index{ix, other}

	search := func(folder uint64, source string) []string {
		m := &ScopeMatcher{Inner: NewTermMatcher("jpg"), SourceID: source, FolderID: folder}
		return hitNames(ixs, Search(ixs, m, SearchOptions{Limit: 10}))
	}
	if got := search(1, "vol-AAAAAAAA"); !reflect.DeepEqual(got, []string{"a.jpg", "old.jpg"}) {
		t.Errorf("scope Photos: %q", got)
	}
	if got := search(6, "vol-AAAAAAAA"); !reflect.DeepEqual(got, []string{"b.jpg"}) {
		t.Errorf("scope Docs\\Photos: %q", got)
	}
	if got := search(0, "vol-AAAAAAAA"); len(got) != 4 {
		t.Errorf("whole source: %q", got)
	}
	if got := search(0, "vol-BBBBBBBB"); !reflect.DeepEqual(got, []string{"jpg-elsewhere.jpg"}) {
		t.Errorf("other source: %q", got)
	}
}

func TestLocateScope(t *testing.T) {
	vol := scopeTree()
	folderSrc := New()
	folderSrc.Header = Header{SourceID: "dir-1", SourceKind: KindFolder, RootPath: filepath.Join(string(filepath.Separator), "data", "stuff")}
	folderSrc.Add(Entry{ID: 1, Name: "Inner", Flags: FlagDir})
	phone := New()
	phone.Header = Header{SourceID: "adb-X", SourceKind: KindPhone}

	root := filepath.Join(string(filepath.Separator), "mnt", "e")
	env := Env{Mounts: map[string]string{"vol-AAAAAAAA": root}}
	ixs := []*Index{vol, folderSrc, phone}

	got, ok := LocateScope(ixs, env, filepath.Join(root, "Photos", "2024"))
	if !ok || got.Index != vol || got.FolderID != 2 || got.Rel != `Photos\2024` {
		t.Errorf("volume folder: %+v %v", got, ok)
	}
	got, ok = LocateScope(ixs, env, root)
	if !ok || got.Index != vol || got.FolderID != 0 || got.Rel != "" {
		t.Errorf("whole drive: %+v %v", got, ok)
	}
	got, ok = LocateScope(ixs, env, filepath.Join(folderSrc.Header.RootPath, "inner"))
	if !ok || got.Index != folderSrc || got.FolderID != 1 {
		t.Errorf("folder source (case-insensitive): %+v %v", got, ok)
	}
	// Not covered by any index / folder missing from the index / unplugged volume.
	for _, p := range []string{filepath.Join(string(filepath.Separator), "elsewhere"), filepath.Join(root, "Missing"), filepath.Join(root, "Photos", "nope")} {
		if _, ok := LocateScope(ixs, env, p); ok {
			t.Errorf("%q should not be locatable", p)
		}
	}
	if _, ok := LocateScope(ixs, Env{}, filepath.Join(root, "Photos")); ok {
		t.Error("an unplugged volume has no current location")
	}
}

func TestDisplaySplit(t *testing.T) {
	env := Env{Mounts: map[string]string{"vol-AAAAAAAA": `F:\`}, Phones: map[string]bool{"adb-PIXEL7": true}}
	vol := &Header{SourceKind: KindVolume, SourceID: "vol-AAAAAAAA", Label: "SANDISK 32GB", LastMount: `E:\`}
	off := &Header{SourceKind: KindVolume, SourceID: "vol-BBBBBBBB", Label: "SANDISK 32GB", LastMount: `E:\`}
	phone := &Header{SourceKind: KindPhone, SourceID: "adb-PIXEL7", Label: "Pixel 7"}
	offPhone := &Header{SourceKind: KindPhone, SourceID: "adb-OLD", Label: "Galaxy"}
	folder := &Header{SourceKind: KindFolder, RootPath: filepath.Join(string(filepath.Separator), "data")}
	sep := string(filepath.Separator)

	cases := []struct {
		name    string
		h       *Header
		rel     string
		dir, nm string
	}{
		{"online volume", vol, `Docs\Jobs\Resume.pdf`, filepath.Join(`F:\`, "Docs", "Jobs") + sep, "Resume.pdf"},
		{"file in the volume root", vol, `top.txt`, `F:\`, "top.txt"},
		{"offline volume", off, `Old\resume_old.pdf`, `[SANDISK 32GB · offline · last seen E:] \Old\`, "resume_old.pdf"},
		{"offline volume root", off, `a.txt`, `[SANDISK 32GB · offline · last seen E:] \`, "a.txt"},
		{"online phone", phone, `Download\r.pdf`, "phone:Pixel 7/Download/", "r.pdf"},
		{"offline phone: the row says offline, not the folder", offPhone, `a\b.jpg`, "phone:Galaxy/a/", "b.jpg"},
		{"phone root", phone, `x.jpg`, "phone:Pixel 7/", "x.jpg"},
		{"folder source", folder, `src\main.go`, filepath.Join(folder.RootPath, "src") + sep, "main.go"},
	}
	for _, c := range cases {
		dir, nm := DisplaySplit(c.h, c.rel, env)
		if dir != c.dir || nm != c.nm {
			t.Errorf("%s:\n got %q + %q\nwant %q + %q", c.name, dir, nm, c.dir, c.nm)
		}
	}
}
