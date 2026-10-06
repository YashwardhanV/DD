package phone

import (
	"sort"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
)

// Paths with "//", "./" or "../" in them used to create folders with empty
// (or "." / "..") names, and two spellings of one folder became two folders.
func TestOddPathSpellingsShareOneFolder(t *testing.T) {
	b := index.NewBuilder(index.Header{})
	p := NewParser(b, "/sdcard/")
	for _, line := range []string{
		"1|1|/sdcard/DCIM//Camera/a.jpg",
		"2|1|/sdcard/DCIM/./Camera/b.jpg",
		"3|1|/sdcard/DCIM/Other/../Camera/c.jpg",
		"4|1|/sdcard//top.txt",
		"5|1|/sdcard/../escape.txt",
	} {
		if !p.ParseStatLine(line) {
			t.Errorf("line %q was rejected", line)
		}
	}
	ix := b.Index()
	var got []string
	for slot := 0; slot < ix.Len(); slot++ {
		if ix.Names[slot] == "" || ix.Names[slot] == "." || ix.Names[slot] == ".." {
			t.Errorf("entry with name %q", ix.Names[slot])
		}
		got = append(got, ix.RelPath(slot))
	}
	sort.Strings(got)
	want := []string{`DCIM`, `DCIM\Camera`, `DCIM\Camera\a.jpg`, `DCIM\Camera\b.jpg`, `DCIM\Camera\c.jpg`, `escape.txt`, `top.txt`}
	if len(got) != len(want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q\nwant %q", got, want)
		}
	}
	if p.Dirs != 2 || p.Files != 5 {
		t.Errorf("Dirs=%d Files=%d, want 2 and 5", p.Dirs, p.Files)
	}
}
