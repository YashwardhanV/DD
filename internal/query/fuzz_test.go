package query

import (
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

// FuzzParse: any search text either parses or gives a *SyntaxError, and a
// parsed query can be run against an index without panicking.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		`resume pdf`, `"final report" !draft`, `ext:jpg,png size:>2mb`, `size:10mb..1gb`,
		`modified:>2025-01-01`, `modified:<30d`, `drive:E`, `drive:"SANDISK 32GB"`,
		`path:whatsapp`, `re:^IMG_\d{8}`, `!ext:jpg`, `size:..`, `""`, `!`, `type:dir`,
	} {
		f.Add(s)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	ix := index.New()
	ix.Header = index.Header{SourceKind: index.KindVolume, SourceID: "vol-1", Label: "USB", LastMount: `E:\`}
	ix.Add(index.Entry{ID: 1, Name: "Photos", Flags: index.FlagDir})
	ix.Add(index.Entry{ID: 2, ParentID: 1, Name: "IMG_20240101.jpg", Size: 3 << 20, ModTime: now.Unix() - 86400})
	ix.Add(index.Entry{ID: 3, ParentID: 1, Name: "notes", Size: -1, Flags: index.FlagNoMeta})
	f.Fuzz(func(t *testing.T, s string) {
		q, err := Parse(s, now)
		if err != nil {
			if _, ok := err.(*SyntaxError); !ok {
				t.Fatalf("Parse(%q) returned a %T, want *SyntaxError", s, err)
			}
			return
		}
		m := q.Matcher(index.Env{})
		index.Search([]*index.Index{ix}, m, index.SearchOptions{Limit: 10})
	})
}
