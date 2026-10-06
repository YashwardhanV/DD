// Command genfixture is a developer tool that creates a deterministic folder
// tree for testing DriveDex: thousands of files with spaces, Unicode, mixed
// case, over-long paths and empty folders.
//
//	go run ./cmd/genfixture -out testdata/generated/fx20k -files 20000
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/yashwardhanv/dd/internal/fixture"
)

func main() {
	out := flag.String("out", "", "folder to create the test tree in (required)")
	files := flag.Int("files", 20000, "total number of files to create")
	depth := flag.Int("depth", 4, "levels of sub-folders")
	fanout := flag.Int("fanout", 8, "maximum sub-folders per folder")
	seed := flag.Int64("seed", 1, "random seed (same seed = same tree)")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "genfixture: -out <dir> is required")
		flag.Usage()
		os.Exit(2)
	}

	res, err := fixture.Generate(*out, fixture.Options{Files: *files, Depth: *depth, Fanout: *fanout, Seed: *seed})
	if err != nil {
		fmt.Fprintf(os.Stderr, "genfixture: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Created %s\n  files:   %d\n  folders: %d (%d of them empty)\n  files with paths over 260 characters: %d\n",
		*out, res.Files, res.Dirs, res.EmptyDirs, res.LongPathFiles)
}
