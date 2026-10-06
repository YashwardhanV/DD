package query

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A fixed "now" so relative dates are predictable: 10 Oct 2026, 12:00 local time.
var now = time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)

func day(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 0, 0, 0, 0, time.Local).Unix() }

func mustParse(t *testing.T, s string) *Query {
	t.Helper()
	q, err := Parse(s, now)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return q
}

// Every example from BUILD.md's syntax table, with exactly what it must parse to.
func TestEveryExampleFromTheSpecTable(t *testing.T) {
	const KB, MB, GB = int64(1 << 10), int64(1 << 20), int64(1 << 30)
	cases := []struct {
		in   string
		want Query
	}{
		// word: name contains, all terms must match
		{`resume pdf`, Query{Terms: []string{"resume", "pdf"}}},
		// "two words": phrase
		{`"final report"`, Query{Terms: []string{"final report"}}},
		// !word: exclude
		{`invoice !draft`, Query{Terms: []string{"invoice"}, Exclude: []string{"draft"}}},
		// ext:a,b
		{`ext:jpg,png`, Query{Exts: []string{"jpg", "png"}}},
		// type:file / type:dir
		{`type:dir photos`, Query{Type: DirsOnly, Terms: []string{"photos"}}},
		{`type:file`, Query{Type: FilesOnly}},
		// size filters
		{`size:>1gb ext:mp4`, Query{Size: &Range{Min: GB + 1, HasMin: true}, Exts: []string{"mp4"}}},
		{`size:>100mb`, Query{Size: &Range{Min: 100*MB + 1, HasMin: true}}},
		{`size:<1kb`, Query{Size: &Range{Max: KB - 1, HasMax: true}}},
		{`size:10mb..1gb`, Query{Size: &Range{Min: 10 * MB, Max: GB, HasMin: true, HasMax: true}}},
		// modified filters: absolute date, relative days
		{`modified:>2025-01-01`, Query{Modified: &Range{Min: day(2025, 1, 2), HasMin: true}}},
		{`modified:<30d`, Query{Modified: &Range{Min: now.Add(-30 * 24 * time.Hour).Unix(), HasMin: true}}},
		{`modified:<7d`, Query{Modified: &Range{Min: now.Add(-7 * 24 * time.Hour).Unix(), HasMin: true}}},
		// drive filters
		{`drive:E`, Query{Drives: []DriveFilter{{Letter: "E:"}}}},
		{`drive:phone IMG`, Query{Drives: []DriveFilter{{Phone: true}}, Terms: []string{"img"}}},
		{`drive:"SANDISK 32GB"`, Query{Drives: []DriveFilter{{Name: "SANDISK 32GB"}}}},
		// path:
		{`path:whatsapp`, Query{PathTerms: []string{"whatsapp"}}},
		// combinations
		{`ext:jpg size:>2mb modified:<7d vacation !thumb`, Query{
			Exts: []string{"jpg"}, Size: &Range{Min: 2*MB + 1, HasMin: true},
			Modified: &Range{Min: now.Add(-7 * 24 * time.Hour).Unix(), HasMin: true},
			Terms:    []string{"vacation"}, Exclude: []string{"thumb"},
		}},
	}
	for _, c := range cases {
		got := mustParse(t, c.in)
		if got.Regexes != nil {
			t.Errorf("%q: unexpected regexes", c.in)
		}
		if !reflect.DeepEqual(*got, c.want) {
			t.Errorf("Parse(%q)\n got %+v (size %+v modified %+v)\nwant %+v (size %+v modified %+v)", c.in, *got, got.Size, got.Modified, c.want, c.want.Size, c.want.Modified)
		}
	}
}

func TestRegexFilter(t *testing.T) {
	q := mustParse(t, `re:^IMG_\d{8}`)
	if len(q.Regexes) != 1 {
		t.Fatalf("regexes = %v", q.Regexes)
	}
	re := q.Regexes[0]
	for name, want := range map[string]bool{"IMG_20240101.jpg": true, "img_20240101.jpg": true /* not case sensitive */, "IMG_2024.jpg": false, "my IMG_20240101.jpg": false} {
		if re.MatchString(name) != want {
			t.Errorf("%q matches = %v, want %v", name, !want, want)
		}
	}
	if q2 := mustParse(t, `re:"^my file \d+$"`); !q2.Regexes[0].MatchString("My File 12") {
		t.Error("a quoted regex may contain spaces")
	}
	if q3 := mustParse(t, `re:a|b`); !q3.Regexes[0].MatchString("xbx") {
		t.Error("alternation")
	}
}

func TestQuotingAndOddInput(t *testing.T) {
	cases := map[string]Query{
		``:                             {},
		`   `:                          {},
		`""`:                           {},
		`!`:                            {Terms: []string{"!"}}, // a lone ! is just text
		`!"final draft" report`:        {Exclude: []string{"final draft"}, Terms: []string{"report"}},
		`"unterminated phrase`:         {Terms: []string{"unterminated phrase"}},
		`RÉSUMÉ`:                       {Terms: []string{"résumé"}},
		`foo:bar`:                      {Terms: []string{"foo:bar"}}, // unknown key = plain text
		`C:\Users`:                     {Terms: []string{`c:\users`}},
		`"ext:jpg"`:                    {Terms: []string{"ext:jpg"}}, // quoting turns a filter into plain text
		`EXT:JPG`:                      {Exts: []string{"jpg"}},      // keys are not case sensitive
		`ext:.JPG,,png `:               {Exts: []string{"jpg", "png"}},
		`type:folders`:                 {Type: DirsOnly},
		`path:Photos/2024`:             {PathTerms: []string{`photos\2024`}},
		`path:"my photos"`:             {PathTerms: []string{"my photos"}},
		`drive:phone drive:E: drive:g`: {Drives: []DriveFilter{{Phone: true}, {Letter: "E:"}, {Letter: "G:"}}},
		`drive:vol-1A2B3C4D`:           {Drives: []DriveFilter{{Name: "vol-1A2B3C4D"}}},
	}
	for in, want := range cases {
		got := mustParse(t, in)
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("Parse(%q) = %+v, want %+v", in, *got, want)
		}
	}
}

func TestIsEmptyAndUsesMetadata(t *testing.T) {
	if !mustParse(t, "").IsEmpty() || !mustParse(t, `""`).IsEmpty() {
		t.Error("an empty query has no criteria")
	}
	for _, s := range []string{"a", "!a", "ext:x", "type:dir", "size:>1", "modified:<1d", "drive:E", "path:x", "re:x"} {
		if mustParse(t, s).IsEmpty() {
			t.Errorf("%q is not empty", s)
		}
	}
	if !mustParse(t, "size:>1").UsesMetadata() || !mustParse(t, "modified:<1d").UsesMetadata() || mustParse(t, "ext:jpg").UsesMetadata() {
		t.Error("UsesMetadata wrong")
	}
}

// ---- size semantics (boundaries matter) ---------------------------------------------------

func TestSizeRanges(t *testing.T) {
	const KB, MB = int64(1 << 10), int64(1 << 20)
	cases := []struct {
		in      string
		inside  []int64
		outside []int64
	}{
		{"size:>100mb", []int64{100*MB + 1, 5 << 30}, []int64{100 * MB, 0, 100*MB - 1}},
		{"size:>=100mb", []int64{100 * MB, 100*MB + 1}, []int64{100*MB - 1}},
		{"size:<1kb", []int64{0, 1023}, []int64{KB, KB + 1}},
		{"size:<=1kb", []int64{0, KB}, []int64{KB + 1}},
		{"size:10mb..1gb", []int64{10 * MB, 1 << 30, 500 * MB}, []int64{10*MB - 1, 1<<30 + 1}},
		{"size:..1mb", []int64{0, MB}, []int64{MB + 1}},
		{"size:5mb..", []int64{5 * MB, 1 << 40}, []int64{5*MB - 1}},
		{"size:0", []int64{0}, []int64{1}},
		{"size:=2kb", []int64{2 * KB}, []int64{2*KB - 1, 2*KB + 1}},
		{"size:1.5mb", []int64{MB + MB/2}, []int64{MB}},
		{"size:>1.5kb", []int64{1537}, []int64{1536}},
		{"size:>10", []int64{11}, []int64{10}}, // no unit = bytes
		{"SIZE:>1GB", []int64{1<<30 + 1}, []int64{1 << 30}},
		{"size:>2k", []int64{2*KB + 1}, []int64{2 * KB}},
		{"size:>1 mb", nil, nil}, // a space splits the token; checked in the error tests
	}
	for _, c := range cases[:len(cases)-1] {
		q := mustParse(t, c.in)
		for _, v := range c.inside {
			if !q.Size.Contains(v) {
				t.Errorf("%s should include %d", c.in, v)
			}
		}
		for _, v := range c.outside {
			if q.Size.Contains(v) {
				t.Errorf("%s should exclude %d", c.in, v)
			}
		}
	}
}

// ---- date semantics ------------------------------------------------------------------------

func TestModifiedRanges(t *testing.T) {
	jan1end := time.Date(2025, 1, 1, 23, 59, 59, 0, time.Local).Unix()
	jan2 := day(2025, 1, 2)
	cases := []struct {
		in      string
		inside  []int64
		outside []int64
	}{
		{"modified:>2025-01-01", []int64{jan2, jan2 + 5, day(2026, 1, 1)}, []int64{jan1end, day(2025, 1, 1), day(2024, 12, 31)}},
		{"modified:>=2025-01-01", []int64{day(2025, 1, 1), jan1end}, []int64{day(2025, 1, 1) - 1}},
		{"modified:<2025-01-01", []int64{day(2025, 1, 1) - 1, day(2020, 5, 5)}, []int64{day(2025, 1, 1), jan1end}},
		{"modified:<=2025-01-01", []int64{day(2025, 1, 1), jan1end}, []int64{jan2}},
		{"modified:=2025-01-01", []int64{day(2025, 1, 1), jan1end}, []int64{day(2025, 1, 1) - 1, jan2}},
		{"modified:2025-01-01..2025-01-31", []int64{day(2025, 1, 1), day(2025, 1, 31), time.Date(2025, 1, 31, 23, 59, 59, 0, time.Local).Unix()}, []int64{day(2025, 1, 1) - 1, day(2025, 2, 1)}},
		// relative: <Nd = within the last N days, >Nd = older than N days
		{"modified:<7d", []int64{now.Unix(), now.Add(-6 * 24 * time.Hour).Unix(), now.Add(-7 * 24 * time.Hour).Unix()}, []int64{now.Add(-7*24*time.Hour).Unix() - 1, now.Add(-30 * 24 * time.Hour).Unix()}},
		{"modified:>30d", []int64{now.Add(-31 * 24 * time.Hour).Unix(), now.Add(-400 * 24 * time.Hour).Unix()}, []int64{now.Unix(), now.Add(-29 * 24 * time.Hour).Unix(), now.Add(-30 * 24 * time.Hour).Unix()}},
		{"modified:<12h", []int64{now.Add(-11 * time.Hour).Unix()}, []int64{now.Add(-13 * time.Hour).Unix()}},
		{"modified:<2w", []int64{now.Add(-13 * 24 * time.Hour).Unix()}, []int64{now.Add(-15 * 24 * time.Hour).Unix()}},
	}
	for _, c := range cases {
		q := mustParse(t, c.in)
		for _, v := range c.inside {
			if !q.Modified.Contains(v) {
				t.Errorf("%s should include %s", c.in, time.Unix(v, 0))
			}
		}
		for _, v := range c.outside {
			if q.Modified.Contains(v) {
				t.Errorf("%s should exclude %s", c.in, time.Unix(v, 0))
			}
		}
	}
}

// ---- friendly errors -------------------------------------------------------------------------

func TestSyntaxErrorsSayHowToFixThem(t *testing.T) {
	cases := map[string]string{
		"ext:":                            "ext: needs a value, for example ext:jpg,png",
		"ext:,":                           "ext: needs a value",
		"size:":                           "size: needs a value",
		"size:>":                          "can't understand the size",
		"size:>abc":                       `can't understand the size "abc"`,
		"size:>10pb":                      "can't understand the size",
		"size:..":                         "needs at least one end",
		"size:1gb..1mb":                   "backwards",
		"modified:":                       "modified: needs a value",
		"modified:soon":                   "can't understand modified:soon",
		"modified:>2025-13-45":            "isn't a date",
		"modified:30d":                    "needs < or > in front",
		"modified:2025-06-30..2025-01-01": "backwards",
		"type:banana":                     `type: must be file or dir, not "banana"`,
		"drive:":                          "drive: needs a value",
		"path:":                           "path: needs a value",
		"re:":                             "re: needs a value",
		"re:[unclosed":                    "isn't a valid regular expression",
		"!ext:jpg":                        "'!' can only be put in front of plain words",
		"!size:>1mb":                      "'!' can only be put in front of plain words",
	}
	for in, want := range cases {
		q, err := Parse(in, now)
		var se *SyntaxError
		if q != nil || !errors.As(err, &se) || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, %v; want an error mentioning %q", in, q, err, want)
		}
	}
	// A space after the operator splits the token, which is reported clearly too.
	if _, err := Parse("size:> 1mb", now); err == nil {
		t.Error("`size:> 1mb` (with a space) should be reported, not silently misread")
	}
}
