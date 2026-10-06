package query

import (
	"math"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

// Stress / regression tests for edge cases found while stress testing.

// A size bigger than int64 (or exactly MaxInt64) used to wrap around to a
// negative number, so size:>99999999999tb matched EVERY file instead of none.
func TestHugeSizesDoNotWrapAround(t *testing.T) {
	for _, s := range []string{
		"size:>99999999999tb", "size:>9223372036854775807", "size:>=99999999999999999999",
		"size:9999999999999999999999", "size:>8388608tb",
	} {
		q := mustParse(t, s)
		for _, v := range []int64{0, 1, 1 << 30, 1 << 50, math.MaxInt64 - 1} {
			if q.Size.Contains(v) {
				t.Errorf("%s matches a file of %d bytes (%+v)", s, v, *q.Size)
			}
		}
	}
	// ...and the "smaller than" side keeps matching everything real.
	for _, s := range []string{"size:<99999999999tb", "size:<=9223372036854775807", "size:0..99999999999999tb"} {
		q := mustParse(t, s)
		for _, v := range []int64{0, 1 << 40} {
			if !q.Size.Contains(v) {
				t.Errorf("%s doesn't match a file of %d bytes (%+v)", s, v, *q.Size)
			}
		}
	}
}

// A relative date beyond ~292 years overflowed time.Duration and was silently
// capped, so modified:<300000d did not reach back as far as it says.
func TestHugeRelativeDates(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	q, err := Parse("modified:<300000d", now)
	if err != nil {
		t.Fatal(err)
	}
	year1700 := time.Date(1700, 1, 1, 0, 0, 0, 0, time.UTC).Unix() // 326 years ago, < 300000 days
	if !q.Modified.Contains(year1700) {
		t.Errorf("modified:<300000d (about 821 years) should include 1700, range %+v", *q.Modified)
	}
	q, err = Parse("modified:>300000d", now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Modified.Contains(year1700) || q.Modified.Contains(now.Unix()) {
		t.Errorf("modified:>300000d (older than about 821 years) should not include 1700 or now, range %+v", *q.Modified)
	}
	for _, s := range []string{"modified:<99999999999999999999d", "modified:>99999999999999999999w"} {
		q, err := Parse(s, now)
		if err != nil {
			t.Fatal(err)
		}
		if s[9] == '<' && !q.Modified.Contains(1) {
			t.Errorf("%s should include every real date, range %+v", s, *q.Modified)
		}
		if s[9] == '>' && q.Modified.Contains(1) {
			t.Errorf("%s should include no real date, range %+v", s, *q.Modified)
		}
	}
}

// ext:tar.gz never matched anything: only the part after the LAST dot was compared.
func TestMultiPartExtensions(t *testing.T) {
	ix := index.New()
	ix.Header = index.Header{SourceKind: index.KindFolder, SourceID: "dir-1", RootPath: "/x"}
	for i, n := range []string{"backup.tar.gz", "notes.gz", "tar.gz", "photo.JPG", "archive.TAR.GZ", "x.tar.gzip"} {
		ix.Add(index.Entry{ID: uint64(i + 1), Name: n, Size: 1})
	}
	got := func(text string) map[string]bool {
		q := mustParse(t, text)
		res := index.Search([]*index.Index{ix}, q.Matcher(index.Env{}), index.SearchOptions{})
		m := map[string]bool{}
		for _, h := range res.Hits {
			m[ix.Names[h.Slot]] = true
		}
		return m
	}
	m := got("ext:tar.gz")
	if !m["backup.tar.gz"] || !m["archive.TAR.GZ"] || m["notes.gz"] || m["x.tar.gzip"] {
		t.Errorf("ext:tar.gz matched %v", m)
	}
	m = got("ext:gz")
	if !m["backup.tar.gz"] || !m["notes.gz"] || !m["tar.gz"] || m["x.tar.gzip"] {
		t.Errorf("ext:gz matched %v", m)
	}
	m = got("ext:.jpg,tar.gz")
	if len(m) != 3 || !m["photo.JPG"] {
		t.Errorf("ext:.jpg,tar.gz matched %v", m)
	}
}
