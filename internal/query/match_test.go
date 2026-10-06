package query

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

const (
	KB = int64(1 << 10)
	MB = int64(1 << 20)
	GB = int64(1 << 30)
)

func ts(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 12, 0, 0, 0, time.Local).Unix() }

// world builds three sources: an online drive, an offline drive and a phone.
func world() ([]*index.Index, index.Env) {
	drive := index.New()
	drive.Header = index.Header{SourceKind: index.KindVolume, SourceID: "vol-AAAAAAAA", Label: "SANDISK 32GB", LastMount: `E:\`}
	add := func(ix *index.Index, id, parent uint64, name string, size, mtime int64, flags uint8) {
		ix.Add(index.Entry{ID: id, ParentID: parent, Name: name, Size: size, ModTime: mtime, Flags: flags})
	}
	add(drive, 1, 0, "Photos", 0, ts(2025, 1, 1), index.FlagDir)
	add(drive, 2, 1, "vacation 1.JPG", 3*MB, ts(2026, 10, 8), 0) // 2 days before "now"
	add(drive, 3, 1, "vacation 2.png", 500*KB, ts(2025, 1, 1), 0)
	add(drive, 4, 1, "thumb_vacation.jpg", 10*KB, ts(2026, 10, 9), 0)
	add(drive, 5, 0, "WhatsApp Media", 0, ts(2025, 1, 1), index.FlagDir)
	add(drive, 6, 5, "IMG_20240101.jpg", 2*MB, ts(2024, 1, 1), 0)
	add(drive, 7, 5, "IMG_2024.jpg", 1*MB, ts(2024, 6, 1), 0)
	add(drive, 8, 0, "movie.mp4", 2*GB, ts(2023, 3, 3), 0)
	add(drive, 9, 0, "draft invoice.pdf", 80*KB, ts(2025, 6, 6), 0)
	add(drive, 10, 0, "invoice.pdf", 90*KB, ts(2025, 6, 7), 0)
	add(drive, 11, 0, ".hidden.jpg", 5*KB, ts(2025, 1, 5), index.FlagHidden)
	add(drive, 12, 0, "pagefile.sys", 4*GB, ts(2025, 1, 5), index.FlagHidden|index.FlagSystem)
	add(drive, 13, 0, "noext", 100, ts(2025, 1, 5), 0)
	add(drive, 14, 0, "NTFS only.txt", -1, 0, index.FlagNoMeta) // turbo-mode entry: no size or date
	add(drive, 15, 0, "photos.2024", 0, ts(2025, 1, 5), index.FlagDir)

	off := index.New()
	off.Header = index.Header{SourceKind: index.KindVolume, SourceID: "vol-BBBBBBBB", Label: "OLD DRIVE", LastMount: `G:\`}
	add(off, 1, 0, "Old", 0, 0, index.FlagDir)
	add(off, 2, 1, "invoice_old.pdf", 70*KB, ts(2020, 1, 1), 0)

	ph := index.New()
	ph.Header = index.Header{SourceKind: index.KindPhone, SourceID: "adb-R58", Label: "Pixel 7", RootPath: "/sdcard"}
	add(ph, 1, 0, "WhatsApp", 0, 0, index.FlagDir)
	add(ph, 2, 1, "IMG-20260101-WA0001.jpg", 200*KB, ts(2026, 1, 1), 0)
	add(ph, 3, 0, "IMG_0001.jpg", 3*MB, ts(2026, 10, 1), 0)

	env := index.Env{Mounts: map[string]string{"vol-AAAAAAAA": `F:\`}, Phones: map[string]bool{"adb-R58": true}} // the SanDisk is F: right now
	return []*index.Index{drive, off, ph}, env
}

// find runs a query over the world and returns "SourceLabel:relative path" strings, sorted.
func find(t *testing.T, text string, mutate ...func(*Matcher)) ([]string, *Matcher) {
	t.Helper()
	ixs, env := world()
	q, err := Parse(text, now)
	if err != nil {
		t.Fatalf("Parse(%q): %v", text, err)
	}
	m := q.Matcher(env)
	for _, f := range mutate {
		f(m)
	}
	res := index.Search(ixs, m, index.SearchOptions{Limit: 0})
	var out []string
	cache := map[int]map[uint64]string{}
	for _, h := range res.Hits {
		if cache[h.Index] == nil {
			cache[h.Index] = map[uint64]string{}
		}
		ix := ixs[h.Index]
		out = append(out, ix.Header.Label+":"+ix.RelPathCached(h.Slot, cache[h.Index]))
	}
	sort.Strings(out)
	return out, m
}

func names(results []string) []string {
	var out []string
	for _, r := range results {
		out = append(out, r[strings.Index(r, ":")+1:])
	}
	sort.Strings(out)
	return out
}

func eq(t *testing.T, query string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if !reflect.DeepEqual(names(got), want) && !(len(got) == 0 && len(want) == 0) {
		t.Errorf("%q\n got %q\nwant %q", query, names(got), want)
	}
}

func TestPlainWordsAndPhrasesAndExclusion(t *testing.T) {
	r, _ := find(t, "invoice")
	eq(t, "invoice", r, `draft invoice.pdf`, `invoice.pdf`, `Old\invoice_old.pdf`)
	r, _ = find(t, "invoice !draft")
	eq(t, "invoice !draft", r, `invoice.pdf`, `Old\invoice_old.pdf`)
	r, _ = find(t, `"draft invoice"`)
	eq(t, `"draft invoice"`, r, `draft invoice.pdf`)
	r, _ = find(t, `invoice draft`) // AND, any order
	eq(t, "invoice draft", r, `draft invoice.pdf`)
	r, _ = find(t, `!pdf !jpg !png !mp4 !sys !jpg photos`)
	eq(t, "photos with exclusions", r, `Photos`, `photos.2024`)
}

func TestExtFilter(t *testing.T) {
	r, _ := find(t, "ext:jpg")
	eq(t, "ext:jpg", r, `.hidden.jpg`, `IMG_0001.jpg`, `Photos\vacation 1.JPG`, `Photos\thumb_vacation.jpg`, `WhatsApp Media\IMG_2024.jpg`, `WhatsApp Media\IMG_20240101.jpg`, `WhatsApp\IMG-20260101-WA0001.jpg`)
	r, _ = find(t, "ext:png,mp4")
	eq(t, "ext:png,mp4", r, `Photos\vacation 2.png`, `movie.mp4`)
	r, _ = find(t, "ext:.PDF")
	if len(r) != 3 {
		t.Errorf("ext is not case sensitive and tolerates a dot: %q", r)
	}
	// Folders never match an extension filter (even "photos.2024"), and a name with no dot has none.
	r, _ = find(t, "ext:2024")
	eq(t, "ext:2024", r)
	r, _ = find(t, "ext:noext")
	eq(t, "ext:noext", r)
}

func TestTypeFilter(t *testing.T) {
	r, _ := find(t, "type:dir photos")
	eq(t, "type:dir photos", r, `Photos`, `photos.2024`)
	r, _ = find(t, "type:file photos")
	eq(t, "type:file photos", r)
	r, _ = find(t, "type:dir whatsapp")
	eq(t, "type:dir whatsapp", r, `WhatsApp Media`, `WhatsApp`)
}

func TestSizeFilters(t *testing.T) {
	r, _ := find(t, "size:>1gb ext:mp4")
	eq(t, "size:>1gb ext:mp4", r, `movie.mp4`)
	r, _ = find(t, "size:>1gb")
	eq(t, "size:>1gb", r, `movie.mp4`, `pagefile.sys`)
	r, _ = find(t, "size:<1kb")
	eq(t, "size:<1kb", r, `noext`, `Photos`, `WhatsApp Media`, `Old`, `WhatsApp`, `photos.2024`) // folders count as size 0
	r, _ = find(t, "size:1mb..2mb ext:jpg")
	eq(t, "size:1mb..2mb", r, `WhatsApp Media\IMG_2024.jpg`, `WhatsApp Media\IMG_20240101.jpg`)
	r, _ = find(t, "size:>100mb type:file ext:mp4,sys")
	eq(t, "big files", r, `movie.mp4`, `pagefile.sys`)
}

func TestModifiedFilters(t *testing.T) {
	r, _ := find(t, "modified:>2026-01-01 type:file")
	// IMG_0001.jpg (phone, Oct 2026), vacation 1 and thumb_vacation (Oct 2026); the phone's WA0001 file is dated 1 Jan 2026 noon - after the 1st.
	eq(t, "modified:>2026-01-01", r, `IMG_0001.jpg`, `Photos\vacation 1.JPG`, `Photos\thumb_vacation.jpg`)
	r, _ = find(t, "modified:<7d type:file")
	eq(t, "modified:<7d", r, `Photos\vacation 1.JPG`, `Photos\thumb_vacation.jpg`)
	r, _ = find(t, "modified:>1000d ext:mp4")
	eq(t, "modified:>1000d", r, `movie.mp4`) // 2023-03-03 is more than 1000 days before 2026-10-10
	r, _ = find(t, "modified:<2024-02-01 type:file")
	eq(t, "modified:<2024-02-01", r, `WhatsApp Media\IMG_20240101.jpg`, `movie.mp4`, `Old\invoice_old.pdf`)
}

func TestMetadataFiltersSkipEntriesWithoutMetadataAndCountThem(t *testing.T) {
	r, m := find(t, "ntfs size:>0")
	eq(t, "ntfs size:>0", r)
	if m.SkippedNoMeta() != 1 {
		t.Errorf("SkippedNoMeta = %d, want 1 (the CLI prints one tip about it)", m.SkippedNoMeta())
	}
	r, m = find(t, "ntfs modified:>2000-01-01")
	eq(t, "ntfs modified:", r)
	if m.SkippedNoMeta() != 1 {
		t.Errorf("SkippedNoMeta = %d", m.SkippedNoMeta())
	}
	// Without a size/date filter the same entry is found normally, and nothing is counted.
	r, m = find(t, "ntfs")
	eq(t, "ntfs", r, `NTFS only.txt`)
	if m.SkippedNoMeta() != 0 {
		t.Errorf("SkippedNoMeta = %d, want 0", m.SkippedNoMeta())
	}
}

func TestDriveFilters(t *testing.T) {
	// The SanDisk is F: right now (it used to be E:).
	for _, q := range []string{"drive:F invoice", "drive:f: invoice", `drive:"sandisk 32gb" invoice`, "drive:vol-AAAAAAAA invoice"} {
		r, _ := find(t, q)
		eq(t, q, r, `draft invoice.pdf`, `invoice.pdf`)
	}
	// The unplugged drive is found by its LAST letter, and by label.
	for _, q := range []string{"drive:G invoice", `drive:"OLD DRIVE" invoice`} {
		r, _ := find(t, q)
		eq(t, q, r, `Old\invoice_old.pdf`)
	}
	// E: is not any drive's current letter and nobody's last letter: nothing.
	r, _ := find(t, "drive:E invoice")
	eq(t, "drive:E", r)
	r, _ = find(t, "drive:phone IMG")
	eq(t, "drive:phone IMG", r, `IMG_0001.jpg`, `WhatsApp\IMG-20260101-WA0001.jpg`)
	// Several drive: filters mean "any of them".
	r, _ = find(t, "drive:phone drive:G ext:jpg,pdf type:file")
	eq(t, "drive:phone drive:G", r, `Old\invoice_old.pdf`, `IMG_0001.jpg`, `WhatsApp\IMG-20260101-WA0001.jpg`)
}

func TestPathFilter(t *testing.T) {
	r, _ := find(t, "path:whatsapp img")
	eq(t, "path:whatsapp img", r, `WhatsApp Media\IMG_20240101.jpg`, `WhatsApp Media\IMG_2024.jpg`, `WhatsApp\IMG-20260101-WA0001.jpg`)
	r, _ = find(t, `path:"media\img_2024"`)
	eq(t, "path with backslash", r, `WhatsApp Media\IMG_2024.jpg`, `WhatsApp Media\IMG_20240101.jpg`)
	r, _ = find(t, `path:media/img_20240101`) // forward slashes are accepted too
	eq(t, "path with slash", r, `WhatsApp Media\IMG_20240101.jpg`)
	// The name filter alone doesn't see the folder name, which is exactly why path: exists.
	r, _ = find(t, "whatsapp type:file")
	eq(t, "whatsapp type:file", r)
}

func TestRegexFilterMatching(t *testing.T) {
	r, _ := find(t, `re:^IMG_\d{8}`)
	eq(t, `re:^IMG_\d{8}`, r, `WhatsApp Media\IMG_20240101.jpg`) // IMG_2024.jpg has only 4 digits; IMG-2026... uses a dash
	r, _ = find(t, `re:^img[-_]\d+ ext:jpg drive:phone`)
	eq(t, "regex + filters", r, `IMG_0001.jpg`, `WhatsApp\IMG-20260101-WA0001.jpg`) // both phone files start with IMG then - or _ then digits
	r, _ = find(t, `re:^img_\d+ ext:jpg drive:phone`)
	eq(t, "regex narrowed to underscores", r, `IMG_0001.jpg`)
	r, _ = find(t, `re:\.JPG$ vacation`)
	eq(t, "case-insensitive regex", r, `Photos\vacation 1.JPG`, `Photos\thumb_vacation.jpg`)
}

func TestHiddenFiles(t *testing.T) {
	r, _ := find(t, "hidden")
	eq(t, "hidden shown by default", r, `.hidden.jpg`)
	r, _ = find(t, "hidden", func(m *Matcher) { m.HideHidden = true })
	eq(t, "hidden hidden", r)
	r, _ = find(t, "pagefile", func(m *Matcher) { m.HideHidden = true })
	eq(t, "system file hidden", r)
}

func TestCombinedExample(t *testing.T) {
	r, _ := find(t, "vacation ext:jpg,png size:>2mb modified:<7d !thumb")
	eq(t, "combined", r, `Photos\vacation 1.JPG`)
}

func TestOnlyFiltersRanksEverythingEqually(t *testing.T) {
	ixs, env := world()
	q := mustParse(t, "ext:pdf")
	res := index.Search(ixs, q.Matcher(env), index.SearchOptions{Limit: 10})
	for _, h := range res.Hits {
		if h.Score != 3 {
			t.Errorf("a filter-only query has no word to rank by: %+v", h)
		}
	}
	// Ranking by words still works.
	res = index.Search(ixs, mustParse(t, "invoice ext:pdf").Matcher(env), index.SearchOptions{Limit: 10})
	var order []string
	for _, h := range res.Hits {
		order = append(order, ixs[h.Index].Names[h.Slot])
	}
	if want := []string{"invoice.pdf", "invoice_old.pdf", "draft invoice.pdf"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %q, want %q", order, want)
	}
}

func TestMatcherIsSafeForConcurrentUse(t *testing.T) {
	// Search runs Match on many goroutines at once (use -race to check).
	ix := index.New()
	ix.Header = index.Header{SourceID: "vol-X", SourceKind: index.KindVolume}
	for i := 0; i < 200000; i++ {
		flags := uint8(0)
		size := int64(i)
		if i%7 == 0 {
			flags, size = index.FlagNoMeta, -1
		}
		ix.Add(index.Entry{ID: uint64(i + 1), Name: fmt.Sprintf("file%d.txt", i), Size: size, ModTime: int64(1_700_000_000 + i), Flags: flags})
	}
	m := mustParse(t, `file ext:txt size:>100 re:\d+ path:file modified:>2000-01-01`).Matcher(index.Env{})
	res := index.Search([]*index.Index{ix}, m, index.SearchOptions{Limit: 5})
	if res.Total == 0 || m.SkippedNoMeta() == 0 {
		t.Errorf("total=%d skipped=%d", res.Total, m.SkippedNoMeta())
	}
}
