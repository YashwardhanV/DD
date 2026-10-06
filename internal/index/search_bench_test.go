package index

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// synthetic builds an index of n entries that looks like a real drive:
// ~1,000 folders, file names made from common words plus numbers.
func synthetic(n int) *Index {
	rng := rand.New(rand.NewSource(1))
	words := []string{"IMG", "photo", "report", "invoice", "resume", "backup", "song", "movie", "notes", "draft", "Budget", "WhatsApp", "scan", "project"}
	exts := []string{".jpg", ".png", ".pdf", ".docx", ".txt", ".mp3", ".mp4", ".zip", ".xlsx"}
	ix := NewWithCapacity(n)
	ix.Header = Header{SourceID: "dir-bench", SourceKind: KindFolder, RootPath: `E:\`}
	const folders = 1000
	for i := 0; i < folders; i++ {
		ix.Add(Entry{ID: uint64(i + 1), ParentID: uint64(rng.Intn(i + 1)), Name: fmt.Sprintf("Folder %d", i), Flags: FlagDir})
	}
	for i := folders; i < n; i++ {
		name := fmt.Sprintf("%s_%s_%d%s", words[rng.Intn(len(words))], words[rng.Intn(len(words))], rng.Intn(1000000), exts[rng.Intn(len(exts))])
		ix.Add(Entry{ID: uint64(i + 1), ParentID: uint64(1 + rng.Intn(folders)), Name: name, Size: int64(rng.Intn(1 << 20)), ModTime: 1700000000 + int64(i)})
	}
	return ix
}

// BenchmarkSearch: target < 100 ms/op on a typical laptop, for 1,000,000 entries.
func BenchmarkSearch(b *testing.B) {
	ix := synthetic(1_000_000)
	ixs := []*Index{ix}
	for _, q := range []string{"resume", "photo jpg", "a", "IMG_2024"} {
		b.Run(q, func(b *testing.B) {
			m := NewTermMatcher(q)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				Search(ixs, m, SearchOptions{Limit: 50})
			}
		})
	}
}

// BenchmarkLoad: target < 1.5 s to read from disk and prepare a 1,000,000-entry
// file (read, checksum, parse, lowercase names, build the ID map).
func BenchmarkLoad(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.ddx")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := Write(f, synthetic(1_000_000)); err != nil {
		b.Fatal(err)
	}
	f.Close()
	st, _ := os.Stat(path)
	b.SetBytes(st.Size())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ReadFile(path); err != nil {
			b.Fatal(err)
		}
	}
}
