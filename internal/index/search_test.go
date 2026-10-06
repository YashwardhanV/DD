package index

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// names builds a flat index (all in the root) from a list of names; a name
// ending in "/" is a folder.
func names(list ...string) *Index {
	ix := New()
	ix.Header = Header{SourceID: "dir-test", SourceKind: KindFolder}
	for i, n := range list {
		e := Entry{ID: uint64(i + 1), Name: strings.TrimSuffix(n, "/"), Size: 1}
		if strings.HasSuffix(n, "/") {
			e.Flags = FlagDir
		}
		ix.Add(e)
	}
	return ix
}

func hitNames(ixs []*Index, res SearchResult) []string {
	var out []string
	for _, h := range res.Hits {
		out = append(out, ixs[h.Index].Names[h.Slot])
	}
	return out
}

func search(ixs []*Index, query string, limit int) []string {
	return hitNames(ixs, Search(ixs, NewTermMatcher(query), SearchOptions{Limit: limit}))
}

func TestParseTerms(t *testing.T) {
	cases := map[string][]string{
		"resume pdf":           {"resume", "pdf"},
		"  Resume   PDF  ":     {"resume", "pdf"},
		`"final report" draft`: {"final report", "draft"},
		`foo "bar baz"`:        {"foo", "bar baz"},
		`"unterminated phrase`: {"unterminated phrase"},
		"RÉSUMÉ":               {"résumé"},
		"":                     nil,
	}
	for in, want := range cases {
		if got := ParseTerms(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseTerms(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchAllTermsMustMatch(t *testing.T) {
	ixs := []*Index{names("resume.pdf", "resume.docx", "invoice.pdf", "notes.txt")}
	if got := search(ixs, "resume pdf", 10); !reflect.DeepEqual(got, []string{"resume.pdf"}) {
		t.Errorf("got %q", got)
	}
	if got := search(ixs, "pdf", 10); len(got) != 2 {
		t.Errorf("got %q, want 2 pdfs", got)
	}
	if got := search(ixs, "nonexistent", 10); len(got) != 0 {
		t.Errorf("got %q, want none", got)
	}
}

func TestSearchIsCaseInsensitiveIncludingUnicode(t *testing.T) {
	ixs := []*Index{names("résumé.pdf", "Straße.txt", "Notes.TXT")}
	for _, q := range []string{"RÉSUMÉ", "résumé", "Résumé"} {
		if got := search(ixs, q, 10); !reflect.DeepEqual(got, []string{"résumé.pdf"}) {
			t.Errorf("query %q: got %q", q, got)
		}
	}
	if got := search(ixs, "notes.txt", 10); !reflect.DeepEqual(got, []string{"Notes.TXT"}) {
		t.Errorf("got %q", got)
	}
}

func TestSearchPhraseStaysTogether(t *testing.T) {
	ixs := []*Index{names("final report.docx", "report final.docx")}
	if got := search(ixs, `"final report"`, 10); !reflect.DeepEqual(got, []string{"final report.docx"}) {
		t.Errorf("got %q", got)
	}
}

func TestSearchRanking(t *testing.T) {
	ixs := []*Index{names(
		"my resume backup.zip", // score 2: word boundary
		"xresumex.txt",         // score 3: buried inside
		"resume.pdf",           // score 1: starts with
		"resume",               // score 0: exact
		"resume_old.pdf",       // score 1, longer name
		"old-resume.doc",       // score 2
	)}
	got := search(ixs, "resume", 10)
	want := []string{"resume", "resume.pdf", "resume_old.pdf", "old-resume.doc", "my resume backup.zip", "xresumex.txt"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ranking:\n got %q\nwant %q", got, want)
	}
}

func TestSearchTieBreaks(t *testing.T) {
	// Same score and length: files before folders, then path order.
	ix := New()
	ix.Header = Header{SourceID: "dir-test", SourceKind: KindFolder}
	ix.Add(Entry{ID: 1, Name: "b", Flags: FlagDir})
	ix.Add(Entry{ID: 2, Name: "a", Flags: FlagDir})
	ix.Add(Entry{ID: 3, ParentID: 1, Name: "cat1"}) // path b\cat1
	ix.Add(Entry{ID: 4, ParentID: 2, Name: "cat2"}) // path a\cat2
	ix.Add(Entry{ID: 5, ParentID: 2, Name: "cat3", Flags: FlagDir})
	ixs := []*Index{ix}
	got := search(ixs, "cat", 10)
	want := []string{"cat2", "cat1", "cat3"} // files by path (a\ before b\), folder last
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tie-break:\n got %q\nwant %q", got, want)
	}
}

func TestSearchSkipsTombstones(t *testing.T) {
	ix := names("alpha.txt", "alpha2.txt", "beta.txt")
	ix.Delete(2)
	res := Search([]*Index{ix}, NewTermMatcher("alpha"), SearchOptions{Limit: 10})
	if res.Total != 1 || len(res.Hits) != 1 {
		t.Errorf("Total=%d hits=%d, want 1/1", res.Total, len(res.Hits))
	}
	if res.Searched != 2 {
		t.Errorf("Searched = %d, want 2 (live entries only)", res.Searched)
	}
}

func TestSearchLimitAndTotal(t *testing.T) {
	var list []string
	for i := 0; i < 100; i++ {
		list = append(list, fmt.Sprintf("file%03d.txt", i))
	}
	ixs := []*Index{names(list...)}
	res := Search(ixs, NewTermMatcher("file"), SearchOptions{Limit: 10})
	if res.Total != 100 || len(res.Hits) != 10 {
		t.Errorf("Total=%d hits=%d, want 100/10", res.Total, len(res.Hits))
	}
	all := Search(ixs, NewTermMatcher("file"), SearchOptions{Limit: 0})
	if len(all.Hits) != 100 {
		t.Errorf("limit 0 returned %d hits, want all 100", len(all.Hits))
	}
	// The limited result is exactly the head of the full ranking.
	for i, h := range res.Hits {
		if h != all.Hits[i] {
			t.Fatalf("limited hit %d = %+v, full ranking has %+v", i, h, all.Hits[i])
		}
	}
}

func TestSearchAcrossIndexesAndSourceFilter(t *testing.T) {
	a := names("report.pdf")
	a.Header.SourceID = "vol-AAAAAAAA"
	b := names("report.docx")
	b.Header.SourceID = "vol-BBBBBBBB"
	ixs := []*Index{a, b}

	res := Search(ixs, NewTermMatcher("report"), SearchOptions{Limit: 10})
	if res.Total != 2 || res.Indexes != 2 || res.Searched != 2 {
		t.Errorf("both: %+v", res)
	}
	m := NewTermMatcher("report")
	m.SourceID = "vol-BBBBBBBB"
	res = Search(ixs, m, SearchOptions{Limit: 10})
	if got := hitNames(ixs, res); !reflect.DeepEqual(got, []string{"report.docx"}) || res.Indexes != 1 {
		t.Errorf("filtered: %q %+v", got, res)
	}
}

func TestSearchFilesOnlyDirsOnly(t *testing.T) {
	ixs := []*Index{names("photos/", "photos.txt", "photo album/")}
	m := NewTermMatcher("photo")
	m.OnlyDirs = true
	if got := hitNames(ixs, Search(ixs, m, SearchOptions{Limit: 10})); len(got) != 2 {
		t.Errorf("dirs: %q", got)
	}
	m = NewTermMatcher("photo")
	m.OnlyFiles = true
	if got := hitNames(ixs, Search(ixs, m, SearchOptions{Limit: 10})); !reflect.DeepEqual(got, []string{"photos.txt"}) {
		t.Errorf("files: %q", got)
	}
}

// A big index is split into several chunks searched in parallel; the answer
// must not depend on how it was split.
func TestSearchParallelChunksGiveSameAnswer(t *testing.T) {
	ix := New()
	ix.Header = Header{SourceID: "dir-big", SourceKind: KindFolder}
	for i := 0; i < 3*minChunk+7; i++ {
		ix.Add(Entry{ID: uint64(i + 1), Name: fmt.Sprintf("item%d.txt", i)})
	}
	res := Search([]*Index{ix}, NewTermMatcher("item1"), SearchOptions{Limit: 5})
	want := 0
	for i := 0; i < ix.Len(); i++ {
		if strings.Contains(ix.Lower[i], "item1") {
			want++
		}
	}
	if res.Total != want {
		t.Errorf("Total = %d, want %d", res.Total, want)
	}
	// Best five: shortest names starting with "item1": item1, item10..item19 - length order.
	got := hitNames([]*Index{ix}, res)
	if got[0] != "item1.txt" || len(got) != 5 {
		t.Errorf("top hits = %q", got)
	}
}
