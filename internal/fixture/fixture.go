// Package fixture generates deterministic test folders full of awkward file
// names (spaces, Unicode, mixed case, over-long paths, empty folders).
//
// It backs the genfixture command and many unit tests, so scanners, the
// index and the copier are all tested against the same kind of messy data a
// real drive contains.
package fixture

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
)

// Options controls the shape of the generated tree.
type Options struct {
	Files  int   // total number of files (at least the number of special files)
	Depth  int   // how many levels of folders
	Fanout int   // each folder gets between 1 and Fanout sub-folders
	Seed   int64 // same seed = same names and contents
}

// Result is exactly what was created, so tests can compare scanner output
// against it. The root folder itself is not counted.
type Result struct {
	Files, Dirs   int
	LongPathFiles int // files whose path relative to the root is longer than 260 characters
	EmptyDirs     int
}

// specialNames are files that every generated tree contains: Unicode, spaces,
// mixed case. Search and copy tests rely on them.
var specialNames = []string{
	"résumé.pdf",
	"फोटो.jpg",
	"写真.png",
	"Résumé Final.docx",
	"Mixed CASE File.TXT",
	"file with  double  spaces.txt",
	"foto 2023.jpg",
	"Resume.pdf",
	"resume_old.pdf",
	"naïve café.txt",
	"Ünïcödé Ñame.md",
	"日本語のファイル.txt",
}

const (
	longFiles        = 5
	longSegmentLen   = 50
	longSegmentsDeep = 6
	emptyDirCount    = 6
)

var folderWords = []string{
	"Photos", "Documents", "Projects", "Music", "Work Stuff", "Backup 2023",
	"Downloads", "Videos", "School", "Taxes", "WhatsApp Media", "Résumés",
}

var fileFormats = []string{
	"IMG_%05d.JPG", "Report %d.pdf", "notes_%d.txt", "Invoice-%d.PDF",
	"song %d.mp3", "photo %d.jpg", "foto %d.jpg", "Resume %d.pdf",
	"Budget %d.xlsx", "clip_%d.mp4", "README %d.md", "draft (%d).docx",
}

// Generate builds a tree under out (created if missing).
func Generate(out string, o Options) (Result, error) {
	var res Result
	if o.Fanout < 1 {
		o.Fanout = 1
	}
	if o.Depth < 0 {
		o.Depth = 0
	}
	out, err := filepath.Abs(out)
	if err != nil {
		return res, fmt.Errorf("resolving %s: %w", out, err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return res, fmt.Errorf("creating %s: %w", out, err)
	}
	rng := rand.New(rand.NewSource(o.Seed))

	mkdir := func(rel string) error {
		if err := os.MkdirAll(filepath.Join(out, rel), 0o755); err != nil {
			return fmt.Errorf("creating folder %s: %w", rel, err)
		}
		return nil
	}
	// mkdirNew creates a folder that is known not to exist yet and counts it.
	mkdirNew := func(rel string) error {
		if err := mkdir(rel); err != nil {
			return err
		}
		res.Dirs++
		return nil
	}
	writeFile := func(rel string, size int) error {
		buf := make([]byte, size)
		rng.Read(buf)
		if err := os.WriteFile(filepath.Join(out, rel), buf, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", rel, err)
		}
		res.Files++
		return nil
	}

	// 1. The folder tree. dirs holds relative paths ("" is the root).
	dirs := []string{""}
	level := []string{""}
	for d := 0; d < o.Depth; d++ {
		var next []string
		for _, parent := range level {
			n := 1 + rng.Intn(o.Fanout)
			for k := 0; k < n; k++ {
				rel := filepath.Join(parent, fmt.Sprintf("%s %d", folderWords[rng.Intn(len(folderWords))], k))
				if err := mkdirNew(rel); err != nil {
					return res, err
				}
				next = append(next, rel)
			}
		}
		dirs = append(dirs, next...)
		level = next
	}

	// 2. Empty folders: never given any files.
	for i := 0; i < emptyDirCount; i++ {
		parent := dirs[rng.Intn(len(dirs))]
		if err := mkdirNew(filepath.Join(parent, fmt.Sprintf("Empty Folder %d", i))); err != nil {
			return res, err
		}
		res.EmptyDirs++
	}

	// 3. Special files: awkward names, each dropped into a random folder.
	for _, name := range specialNames {
		if err := writeFile(filepath.Join(dirs[rng.Intn(len(dirs))], name), rng.Intn(4097)); err != nil {
			return res, err
		}
	}

	// 4. Files whose full path is far beyond Windows' classic 260-character
	// limit: a chain of long-named folders with a few files at the bottom.
	chain := "Long Paths"
	if err := mkdirNew(chain); err != nil {
		return res, err
	}
	for i := 0; i < longSegmentsDeep; i++ {
		seg := fmt.Sprintf("%02d_%s", i, strings.Repeat("deep_folder_name_", 4))[:longSegmentLen]
		chain = filepath.Join(chain, seg)
		if err := mkdirNew(chain); err != nil {
			return res, err
		}
	}
	for i := 0; i < longFiles; i++ {
		rel := filepath.Join(chain, fmt.Sprintf("long path file %d.txt", i))
		if err := writeFile(rel, rng.Intn(4097)); err != nil {
			return res, err
		}
		if len(rel) > 260 {
			res.LongPathFiles++
		}
	}

	// 5. Ordinary files, spread over the tree. The running number makes every
	// name unique within its folder.
	for i := 0; res.Files < o.Files; i++ {
		name := fmt.Sprintf(fileFormats[rng.Intn(len(fileFormats))], i)
		if err := writeFile(filepath.Join(dirs[rng.Intn(len(dirs))], name), rng.Intn(4097)); err != nil {
			return res, err
		}
	}
	return res, nil
}
