package copier

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/ntfs/ntfstest"
	"github.com/yashwardhanv/dd/internal/scan"
)

// randomTree writes a random folder tree with awkward names, sizes and
// sub-second times under root.
func randomTree(t testing.TB, root string, seed int64, files int) {
	t.Helper()
	r := rand.New(rand.NewSource(seed))
	words := []string{"Photos", "résumé", "data", "फोटो", "a b", "x.y.z", "UPPER", "lower", "写真", "deep"}
	dirs := []string{root}
	for i := 0; i < files; i++ {
		if r.Intn(4) == 0 {
			parent := dirs[r.Intn(len(dirs))]
			d := filepath.Join(parent, fmt.Sprintf("%s %d", words[r.Intn(len(words))], i))
			os.MkdirAll(d, 0o755)
			dirs = append(dirs, d)
			if r.Intn(5) == 0 {
				continue // an empty folder now and then
			}
		}
		parent := dirs[r.Intn(len(dirs))]
		p := filepath.Join(parent, fmt.Sprintf("%s_%d.%s", words[r.Intn(len(words))], i, []string{"jpg", "txt", "bin", ""}[r.Intn(4)]))
		os.WriteFile(p, make([]byte, r.Intn(5000)), 0o644)
		when := time.Unix(1_600_000_000+r.Int63n(100_000_000), r.Int63n(1e7)*100)
		os.Chtimes(p, when, when)
	}
}

// planShape is what must be equal between the two planners.
type planShape struct {
	Dirs  []string
	Files []string // "rel|size|mtimeNs"
	Links int64
}

func shapeOf(p *Plan) planShape {
	s := planShape{Dirs: append([]string(nil), p.Dirs...), Links: p.LinksSkipped}
	for _, it := range p.Items {
		s.Files = append(s.Files, fmt.Sprintf("%s|%d|%d|%s", it.Rel, it.Size, it.MTimeNs(), it.Src))
	}
	sort.Strings(s.Dirs)
	sort.Strings(s.Files)
	return s
}

// indexOf indexes dir with the parallel scanner, as `drivedex index` does.
func indexOf(t testing.TB, dir string) *index.Index {
	t.Helper()
	b := index.NewBuilder(index.Header{SourceID: "dir-test", SourceKind: index.KindFolder})
	if err := scan.NewParallel(dir, 4).Scan(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	return b.Index()
}

// TestPlanFromIndexEqualsPlanLocal is the required property test: for random
// trees, planning from an index gives exactly the plan a fresh listing gives.
func TestPlanFromIndexEqualsPlanLocal(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "Source Folder")
			os.MkdirAll(src, 0o755)
			randomTree(t, src, seed, 300)
			dst := t.TempDir()
			// Something already at the destination, so decisions are compared too.
			os.MkdirAll(filepath.Join(dst, "Source Folder"), 0o755)

			want, err := PlanLocal(context.Background(), src, dst, "", 4)
			if err != nil {
				t.Fatal(err)
			}
			ix := indexOf(t, src)
			got, err := PlanFromIndex(context.Background(), ix, 0, src, dst, "", 4)
			if err != nil {
				t.Fatal(err)
			}
			if a, b := shapeOf(got), shapeOf(want); !reflect.DeepEqual(a, b) {
				t.Fatalf("plans differ:\nindex: %d dirs %d files %d links\nlocal: %d dirs %d files %d links\nfirst index file %v\nfirst local file %v",
					len(a.Dirs), len(a.Files), a.Links, len(b.Dirs), len(b.Files), b.Links, first(a.Files), first(b.Files))
			}

			// A sub-folder of the index, too.
			var sub string
			filepath.WalkDir(src, func(p string, d fs.DirEntry, _ error) error {
				if sub == "" && d.IsDir() && p != src {
					sub = p
				}
				return nil
			})
			if sub == "" {
				return
			}
			rel, _ := filepath.Rel(src, sub)
			id, ok := ix.FindFolder(rel)
			if !ok {
				t.Fatalf("FindFolder(%q)", rel)
			}
			want, _ = PlanLocal(context.Background(), sub, dst, "", 4)
			got, err = PlanFromIndex(context.Background(), ix, id, sub, dst, "", 4)
			if err != nil {
				t.Fatal(err)
			}
			if a, b := shapeOf(got), shapeOf(want); !reflect.DeepEqual(a, b) {
				t.Fatalf("sub-folder plans differ: %d/%d files", len(a.Files), len(b.Files))
			}
		})
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// mirror builds a fake NTFS volume with exactly the tree on disk at root
// (names, sizes, exact times), as the MFT of a real drive holding it would.
func mirror(t *testing.T, root string) *ntfstest.Volume {
	t.Helper()
	v := ntfstest.New()
	frns := map[string]uint64{root: 0}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		parent := frns[filepath.Dir(p)]
		info, _ := d.Info()
		if d.IsDir() {
			frns[p] = v.Mkdir(parent, d.Name())
			v.SetMeta(frns[p], 0, info.ModTime().UnixNano())
			return nil
		}
		frn := v.Create(parent, d.Name())
		v.SetMeta(frn, info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return v
}

// mftIndex indexes a fake volume in turbo mode, then fills sizes and dates
// folder by folder, as `drivedex index E:` now does.
func mftIndex(t *testing.T, v *ntfstest.Volume, root string) (*index.Index, ntfs.MetaStats) {
	t.Helper()
	b := index.NewBuilder(index.Header{SourceKind: index.KindVolume, SourceID: "vol-TEST", FileSystem: "NTFS", Scanner: "mft"})
	sc := &ntfs.MFTScanner{Session: v.Session()}
	if err := sc.Scan(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	ix := b.Index()
	st, err := ntfs.FillMetadataByDir(context.Background(), ix, root, v.Lister(root), 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ix, st
}

// TestPlanFromATurboIndexEqualsPlanLocal: the same property for an index made
// from the master file table (through the fake NTFS) and filled by folder.
func TestPlanFromATurboIndexEqualsPlanLocal(t *testing.T) {
	for seed := int64(11); seed <= 14; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "Drive")
			os.MkdirAll(src, 0o755)
			randomTree(t, src, seed, 200)
			v := mirror(t, src)
			ix, st := mftIndex(t, v, src)
			if st.Missing != 0 || !ix.Header.MetaComplete || !ix.Header.ExactTimes() {
				t.Fatalf("metadata %+v complete=%v exact=%v", st, ix.Header.MetaComplete, ix.Header.ExactTimes())
			}
			dst := t.TempDir()
			want, _ := PlanLocal(context.Background(), src, dst, "", 4)
			got, err := PlanFromIndex(context.Background(), ix, 0, src, dst, "", 4)
			if err != nil {
				t.Fatal(err)
			}
			if a, b := shapeOf(got), shapeOf(want); !reflect.DeepEqual(a, b) {
				t.Fatalf("plans differ: index %d files / %d dirs, local %d / %d\n%v\n%v", len(a.Files), len(a.Dirs), len(b.Files), len(b.Dirs), first(a.Files), first(b.Files))
			}
			for _, it := range got.Items {
				if it.SrcFileID == 0 {
					t.Fatalf("%s has no file ID", it.Rel)
				}
			}
		})
	}
}

func TestPlanFromIndexSkipsLinksAndDedupFilesAreCopied(t *testing.T) {
	root := t.TempDir()
	v := ntfstest.New()
	docs := v.Mkdir(0, "Docs")
	v.SetMeta(v.Create(docs, "a.txt"), 3, 1_700_000_000e9)
	v.CreateLink(docs, "shortcut", false, 0xA000000C) // symbolic link
	v.CreateLink(0, "Junction", true, 0xA0000003)     // junction (a folder)
	d := v.CreateLink(docs, "deduped.iso", false, ntfs.TagDedup)
	v.SetMeta(d, 10, 1_700_000_000e9)
	ix, _ := mftIndex(t, v, root)
	p, err := PlanFromIndex(context.Background(), ix, 0, root, t.TempDir(), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, it := range p.Items {
		rels = append(rels, filepath.Base(it.Rel))
	}
	sort.Strings(rels)
	if strings.Join(rels, ",") != "a.txt,deduped.iso" || p.LinksSkipped != 2 {
		t.Errorf("files %v, links %d (want a.txt,deduped.iso and 2 links)", rels, p.LinksSkipped)
	}
}

func TestPlanFromIndexRejectsAFileOrUnknownFolder(t *testing.T) {
	ix := index.New()
	ix.Add(index.Entry{ID: 1, Name: "f.txt", Size: 1})
	if _, err := PlanFromIndex(context.Background(), ix, 1, t.TempDir(), t.TempDir(), "", 1); err == nil {
		t.Error("a file is not a folder")
	}
	if _, err := PlanFromIndex(context.Background(), ix, 99, t.TempDir(), t.TempDir(), "", 1); err == nil {
		t.Error("an unknown folder")
	}
}

// syntheticIndex builds an index of n entries: a top folder "Photos" with
// subFiles files (in folders of 50, 40 folders per year folder), then more
// top folders of the same shape until n entries. It returns Photos' ID.
func syntheticIndex(n, subFiles int) (*index.Index, uint64) {
	ix := index.NewWithCapacity(n)
	ix.Header.MTimeUnit = "ns"
	id := uint64(1)
	newID := func() uint64 { id++; return id - 1 }
	var photos uint64
	for top := 0; ix.Len() < n; top++ {
		tid := newID()
		name := fmt.Sprintf("Archive %d", top)
		if top == 0 {
			name, photos = "Photos", tid
		}
		ix.Add(index.Entry{ID: tid, Name: name, Flags: index.FlagDir})
		files := 0
		for y := 0; (top == 0 && files < subFiles) || (top > 0 && files < subFiles && ix.Len() < n); y++ {
			yid := newID()
			ix.Add(index.Entry{ID: yid, ParentID: tid, Name: fmt.Sprintf("%d", 2000+y), Flags: index.FlagDir})
			for f := 0; f < 40 && files < subFiles; f++ {
				fid := newID()
				ix.Add(index.Entry{ID: fid, ParentID: yid, Name: fmt.Sprintf("Event %02d", f), Flags: index.FlagDir})
				for i := 0; i < 50 && files < subFiles; i++ {
					fileID := newID()
					ix.Add(index.Entry{ID: fileID, ParentID: fid, Name: fmt.Sprintf("IMG_%07d.jpg", fileID), Size: int64(fileID % 5_000_000), ModTime: 1_700_000_000, Nanos: uint32(fileID % 1e9)})
					files++
				}
			}
		}
	}
	return ix, photos
}

// BenchmarkPlanFromIndex: 1,000,000 indexed entries, planning one top folder
// (about 200,000 files) to a destination that doesn't exist yet (so the
// destination check is excluded, as the design asks). Target: < 500 ms.
func BenchmarkPlanFromIndex(b *testing.B) {
	ix, folder := syntheticIndex(1_000_000, 200_000)
	dst := filepath.Join(b.TempDir(), "not-yet")
	b.ResetTimer()
	var files int
	for i := 0; i < b.N; i++ {
		p, err := PlanFromIndex(context.Background(), ix, folder, `X:\Src`, dst, "", 8)
		if err != nil {
			b.Fatal(err)
		}
		files = len(p.Items)
	}
	b.ReportMetric(float64(files), "files")
}
