package phone

import (
	"bufio"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
)

// parseFixture feeds the sample file to a parser.
func parseFixture(t *testing.T) (*Parser, *index.Index) {
	t.Helper()
	f, err := os.Open("testdata/adb_find_sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := index.NewBuilder(index.Header{})
	p := NewParser(b, "/sdcard/")
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p.ParseStatLine(sc.Text())
	}
	return p, b.Index()
}

func entryByPath(ix *index.Index) map[string]index.Entry {
	m := map[string]index.Entry{}
	for slot := 0; slot < ix.Len(); slot++ {
		m[ix.RelPath(slot)] = ix.Entry(slot)
	}
	return m
}

func TestParseStatLinesFromFixture(t *testing.T) {
	p, ix := parseFixture(t)
	byPath := entryByPath(ix)

	// Files, including awkward names. Paths use `\` internally.
	files := map[string]struct{ size, mtime int64 }{
		`DCIM\Camera\IMG_20240101_120000.jpg`: {1048576, 1700000000},
		`Download\My Report (final).pdf`:      {2048, 1700000100},
		`Download\weird|name|with|pipes.txt`:  {12, 1700000200}, // "|" in the name survives
		`Documents\写真\फोटो.jpg`:               {345, 1700000300},
		`a\b\c\d\e\f\g\deep.txt`:              {0, 1690000000},
		`top.txt`:                             {7, 1700000400},
		`DCIM\Camera\IMG_20240102_120000.jpg`: {99, 1700000500},
		`DCIM\crlf.jpg`:                       {55, 1700000600}, // trailing \r removed
	}
	for path, want := range files {
		e, ok := byPath[path]
		if !ok {
			t.Errorf("missing file %q", path)
			continue
		}
		if e.IsDir() || e.Size != want.size || e.ModTime != want.mtime || e.Flags&index.FlagNoMeta != 0 {
			t.Errorf("%q = %+v, want size %d mtime %d", path, e, want.size, want.mtime)
		}
	}
	if p.Files != int64(len(files)) {
		t.Errorf("Files = %d, want %d", p.Files, len(files))
	}

	// Folders were synthesized for every ancestor, exactly once.
	dirs := []string{"DCIM", `DCIM\Camera`, "Download", "Documents", `Documents\写真`,
		"a", `a\b`, `a\b\c`, `a\b\c\d`, `a\b\c\d\e`, `a\b\c\d\e\f`, `a\b\c\d\e\f\g`}
	for _, d := range dirs {
		e, ok := byPath[d]
		if !ok || !e.IsDir() || e.Size != 0 || e.Flags&index.FlagNoMeta != 0 {
			t.Errorf("folder %q = %+v (ok=%v)", d, e, ok)
		}
	}
	if p.Dirs != int64(len(dirs)) {
		t.Errorf("Dirs = %d, want %d", p.Dirs, len(dirs))
	}
	if ix.Len() != len(files)+len(dirs) {
		t.Errorf("index has %d entries, want %d", ix.Len(), len(files)+len(dirs))
	}
}

func TestParentLinksAreConsistent(t *testing.T) {
	_, ix := parseFixture(t)
	byPath := entryByPath(ix)
	camera := byPath[`DCIM\Camera`]
	if byPath[`DCIM\Camera\IMG_20240101_120000.jpg`].ParentID != camera.ID ||
		byPath[`DCIM\Camera\IMG_20240102_120000.jpg`].ParentID != camera.ID {
		t.Error("both camera files must share one Camera folder entry")
	}
	if camera.ParentID != byPath["DCIM"].ID || byPath["DCIM"].ParentID != 0 {
		t.Error("folder chain broken")
	}
	if byPath["top.txt"].ParentID != 0 {
		t.Error("files in the root have parent 0")
	}
	// IDs are unique.
	seen := map[uint64]bool{}
	for slot := 0; slot < ix.Len(); slot++ {
		if seen[ix.IDs[slot]] {
			t.Fatalf("duplicate ID %d", ix.IDs[slot])
		}
		seen[ix.IDs[slot]] = true
	}
}

func TestMalformedLinesAreCountedAndSkipped(t *testing.T) {
	p, ix := parseFixture(t)
	// malformed: text line, bad size, bad time, negative size. The blank line is not an error.
	if p.Malformed != 4 {
		t.Errorf("Malformed = %d, want 4 (%q)", p.Malformed, p.FirstMalformed)
	}
	for _, bad := range []string{"bad-size.txt", "bad-time.txt", "negative.txt"} {
		for slot := 0; slot < ix.Len(); slot++ {
			if ix.Names[slot] == bad {
				t.Errorf("malformed line for %s must not create an entry", bad)
			}
		}
	}
	if len(p.FirstMalformed) != 4 || !strings.Contains(p.FirstMalformed[0], "this line is malformed") {
		t.Errorf("FirstMalformed = %q", p.FirstMalformed)
	}
}

func TestOnlyFirst20MalformedLinesAreKept(t *testing.T) {
	p := NewParser(index.NewDiscardBuilder(), "/sdcard/")
	for i := 0; i < 50; i++ {
		p.ParseStatLine("junk")
	}
	if p.Malformed != 50 || len(p.FirstMalformed) != 20 {
		t.Errorf("Malformed=%d kept=%d", p.Malformed, len(p.FirstMalformed))
	}
}

func TestParseNameLinesGiveNoMetadata(t *testing.T) {
	b := index.NewBuilder(index.Header{})
	p := NewParser(b, "/sdcard")
	for _, l := range []string{"/sdcard/DCIM/a.jpg", "/sdcard/b.txt\r", "", "/sdcard/"} {
		p.ParseNameLine(l)
	}
	by := entryByPath(b.Index())
	for _, f := range []string{`DCIM\a.jpg`, "b.txt"} {
		e := by[f]
		if e.Size != -1 || e.ModTime != 0 || e.Flags&index.FlagNoMeta == 0 {
			t.Errorf("%s = %+v, want unknown size/time with FlagNoMeta", f, e)
		}
	}
	if !by["DCIM"].IsDir() || p.Files != 2 || p.Malformed != 1 {
		t.Errorf("Files=%d Malformed=%d dir=%+v", p.Files, p.Malformed, by["DCIM"])
	}
}

func TestParserHandlesRootWithoutTrailingSlash(t *testing.T) {
	b := index.NewBuilder(index.Header{})
	p := NewParser(b, "/storage/emulated/0")
	p.ParseStatLine("5|10|/storage/emulated/0/Music/song.mp3")
	if got := entryByPath(b.Index()); got[`Music\song.mp3`].Size != 5 {
		t.Errorf("paths = %v", keys(got))
	}
}

func keys(m map[string]index.Entry) []string {
	var k []string
	for p := range m {
		k = append(k, p)
	}
	sort.Strings(k)
	return k
}
