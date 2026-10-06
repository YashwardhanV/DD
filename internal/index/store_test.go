package index

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func saved(t *testing.T, s *Store, id, name string) {
	t.Helper()
	ix := New()
	ix.Header = Header{SourceKind: KindFolder, SourceID: id, Label: name, RootPath: `D:\` + name}
	ix.Add(Entry{ID: 1, Name: name + ".txt", Size: 1})
	if _, err := s.Save(ix); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
}

func TestStoreSaveLoadDelete(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "nested", "indexes")) // folder doesn't exist yet
	saved(t, s, "dir-aaaaaaaaaaaa", "alpha")

	if !s.Exists("dir-aaaaaaaaaaaa") {
		t.Fatal("index should exist after Save")
	}
	ix, err := s.Load("dir-aaaaaaaaaaaa")
	if err != nil || ix.Len() != 1 || ix.Names[0] != "alpha.txt" {
		t.Fatalf("Load = %v, %v", ix, err)
	}
	if err := s.Delete("dir-aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if s.Exists("dir-aaaaaaaaaaaa") {
		t.Error("index still exists after Delete")
	}
}

func TestSaveIsAtomicAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	saved(t, s, "dir-aaaaaaaaaaaa", "one")
	saved(t, s, "dir-aaaaaaaaaaaa", "two") // overwrite

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	ix, err := s.Load("dir-aaaaaaaaaaaa")
	if err != nil || ix.Names[0] != "two.txt" {
		t.Fatalf("overwrite not visible: %v %v", ix, err)
	}
}

func TestLoadAllSkipsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	saved(t, s, "dir-aaaaaaaaaaaa", "alpha")
	saved(t, s, "dir-bbbbbbbbbbbb", "beta")
	if err := os.WriteFile(filepath.Join(dir, "broken.ddx"), []byte("not an index"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("x"), 0o644)

	loaded, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Indexes) != 2 {
		t.Errorf("loaded %d indexes, want 2", len(loaded.Indexes))
	}
	if len(loaded.Warnings) != 1 || !errors.Is(loaded.Warnings[0], ErrCorruptIndex) {
		t.Errorf("warnings = %v, want one ErrCorruptIndex", loaded.Warnings)
	}
}

func TestLoadAllWhenNothingIndexed(t *testing.T) {
	loaded, err := NewStore(filepath.Join(t.TempDir(), "missing")).LoadAll()
	if err != nil || len(loaded.Indexes) != 0 {
		t.Errorf("LoadAll on missing folder = %+v, %v; want empty, nil", loaded, err)
	}
}

func TestListReadsHeadersOnly(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	saved(t, s, "dir-aaaaaaaaaaaa", "alpha")
	os.WriteFile(filepath.Join(dir, "broken.ddx"), []byte("garbage garbage"), 0o644)

	infos, err := s.List()
	if err != nil || len(infos) != 2 {
		t.Fatalf("List = %v, %v", infos, err)
	}
	var good, bad int
	for _, in := range infos {
		if in.Err != nil {
			bad++
		} else {
			good++
			if in.Header.Label != "alpha" || in.FileSize == 0 {
				t.Errorf("bad info: %+v", in)
			}
		}
	}
	if good != 1 || bad != 1 {
		t.Errorf("good=%d bad=%d, want 1/1", good, bad)
	}
}

func TestFileNameIsSafe(t *testing.T) {
	cases := map[string]string{
		"vol-1A2B3C4D":         "vol-1A2B3C4D.ddx",
		"adb-192.168.1.5:5555": "adb-192.168.1.5_5555.ddx",
		`adb-weird/\name*?`:    "adb-weird__name__.ddx",
		"adb-ABC123":           "adb-ABC123.ddx",
	}
	for in, want := range cases {
		if got := fileName(in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSaveRequiresSourceID(t *testing.T) {
	if _, err := NewStore(t.TempDir()).Save(New()); err == nil {
		t.Error("Save without SourceID should fail")
	}
}

func TestDefaultDirHonoursEnv(t *testing.T) {
	t.Setenv(EnvIndexDir, "/somewhere/else")
	if d, _ := DefaultDir(); d != "/somewhere/else" {
		t.Errorf("DefaultDir = %q", d)
	}
}

func TestFolderSourceID(t *testing.T) {
	a := FolderSourceID(`D:\Projects`)
	if a != FolderSourceID(`d:\projects`) {
		t.Error("source ID should ignore case")
	}
	if a == FolderSourceID(`D:\Other`) {
		t.Error("different folders must get different IDs")
	}
	if len(a) != len("dir-")+12 || !strings.HasPrefix(a, "dir-") {
		t.Errorf("unexpected ID format %q", a)
	}
}
