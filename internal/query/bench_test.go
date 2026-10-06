package query

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

func synthetic(n int) *index.Index {
	rng := rand.New(rand.NewSource(1))
	words := []string{"report", "photo", "resume", "invoice", "budget", "holiday", "IMG", "notes", "backup", "project"}
	exts := []string{".jpg", ".png", ".pdf", ".txt", ".docx", ".mp4", ".zip"}
	ix := index.NewWithCapacity(n)
	ix.Header = index.Header{SourceKind: index.KindFolder, SourceID: "dir-bench", RootPath: "/bench"}
	folders := n / 50
	for i := 0; i < folders; i++ {
		ix.Add(index.Entry{ID: uint64(i + 1), ParentID: uint64(rng.Intn(i + 1)), Name: fmt.Sprintf("Folder %d", i), Flags: index.FlagDir})
	}
	for i := folders; i < n; i++ {
		name := fmt.Sprintf("%s_%s_%d%s", words[rng.Intn(len(words))], words[rng.Intn(len(words))], rng.Intn(1000000), exts[rng.Intn(len(exts))])
		ix.Add(index.Entry{ID: uint64(i + 1), ParentID: uint64(1 + rng.Intn(folders)), Name: name, Size: int64(rng.Intn(1 << 22)), ModTime: 1700000000 + int64(i)})
	}
	return ix
}

// BenchmarkQuery: the power-search syntax on 1,000,000 entries. Cheap filters
// (type, extension, name words) run first, so adding the expensive ones
// (full path, regex, size, date) must not make a search noticeably slower.
func BenchmarkQuery(b *testing.B) {
	ixs := []*index.Index{synthetic(1_000_000)}
	now := time.Now()
	for _, q := range []string{
		"resume",
		"resume ext:pdf",
		"photo ext:jpg,png size:>2mb",
		"photo ext:jpg modified:<3650d size:>1mb",
		"invoice !draft ext:pdf path:folder",
		`re:^IMG_\w+_\d{6}\.jpg$`,
		"ext:jpg size:>3mb",
	} {
		b.Run(q, func(b *testing.B) {
			parsed, err := Parse(q, now)
			if err != nil {
				b.Fatal(err)
			}
			m := parsed.Matcher(index.Env{})
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				index.Search(ixs, m, index.SearchOptions{Limit: 50})
			}
		})
	}
}
