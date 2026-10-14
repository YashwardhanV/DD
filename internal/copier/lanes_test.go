package copier

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// With BigLanes = 1, at most one big file is in flight at any moment, small
// files keep flowing meanwhile, and everything arrives with the right size and
// time.
func TestBigLanesLimitBigFilesWithoutStarvingSmallOnes(t *testing.T) {
	oldBig := bigFileSize
	bigFileSize = 4 << 10
	defer func() { bigFileSize = oldBig; openSource = os.Open; afterCopy = nil }()

	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_600_000_000, 0)
	write := func(name string, size int) {
		p := filepath.Join(src, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	for i := 0; i < 5; i++ {
		write(fmt.Sprintf("big%02d.bin", i), 8<<10)
	}
	for i := 0; i < 30; i++ {
		write(fmt.Sprintf("small%02d.txt", i), 100) // (plain letters ran into '|', which Windows forbids)
	}

	var bigActive, maxBig, smallDuringBig atomic.Int32
	var mu sync.Mutex
	openSource = func(name string) (*os.File, error) {
		if strings.HasPrefix(filepath.Base(name), "big") {
			n := bigActive.Add(1)
			mu.Lock()
			if n > maxBig.Load() {
				maxBig.Store(n)
			}
			mu.Unlock()
			time.Sleep(15 * time.Millisecond) // hold the lane so overlap is observable
		}
		return os.Open(name)
	}
	afterCopy = func(dst string) {
		if strings.HasPrefix(filepath.Base(dst), "big") {
			bigActive.Add(-1)
		} else if bigActive.Load() > 0 {
			smallDuringBig.Add(1)
		}
	}

	dst := filepath.Join(t.TempDir(), "dst")
	p := plan(t, src, dst, "")
	opt := fastOpts(4)
	opt.BigLanes = 1
	r := mustRun(t, p, opt)
	if r.Written != 35 || r.Failed != 0 {
		t.Fatalf("result %+v", r)
	}
	if maxBig.Load() != 1 {
		t.Errorf("up to %d big files at once, want 1", maxBig.Load())
	}
	if smallDuringBig.Load() == 0 {
		t.Error("no small file was copied while a big one was in flight: the small files waited")
	}
	files, _ := listTree(t, filepath.Join(dst, "src"))
	if len(files) != 35 {
		t.Fatalf("copied %d files", len(files))
	}
	for rel, size := range files {
		want := int64(100)
		if strings.HasPrefix(rel, "big") {
			want = 8 << 10
		}
		if size != want {
			t.Errorf("%s: size %d, want %d", rel, size, want)
		}
		fi, _ := os.Stat(filepath.Join(dst, "src", rel))
		if fi.ModTime().Unix() != mtime.Unix() {
			t.Errorf("%s: mtime %v, want %v", rel, fi.ModTime(), mtime)
		}
	}
	leftovers(t, dst)
}

// BigLanes that is >= Workers (or negative) is ignored rather than leaving no
// worker for small files.
func TestBigLanesOutOfRangeIsIgnored(t *testing.T) {
	for _, lanes := range []int{-1, 4, 9} {
		src, res := source(t, 60)
		dst := filepath.Join(t.TempDir(), "dst")
		p := plan(t, src, dst, "")
		opt := fastOpts(4)
		opt.BigLanes = lanes
		if r := mustRun(t, p, opt); int(r.Written) != res.Files || r.Failed != 0 {
			t.Errorf("BigLanes %d: result %+v, want %d written", lanes, r, res.Files)
		}
	}
}

// Cancelling while both dispatchers are blocked must not hang.
func TestBigLanesCancel(t *testing.T) {
	oldBig := bigFileSize
	bigFileSize = 1
	defer func() { bigFileSize = oldBig }()
	src, _ := source(t, 200)
	dst := filepath.Join(t.TempDir(), "dst")
	p := plan(t, src, dst, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opt := fastOpts(3)
	opt.BigLanes = 1
	done := make(chan struct{})
	go func() { Run(ctx, p, opt); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run hung after cancel")
	}
}

// A file big enough to have its space reserved first arrives byte for byte,
// with the exact size (reserving space must never pad the file) and time.
func TestReservedSpaceFileArrivesExactly(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	os.MkdirAll(src, 0o755)
	data := make([]byte, 3<<20+12345)
	for i := range data {
		data[i] = byte(i*7 + i/4096)
	}
	p := filepath.Join(src, "film.mp4")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_500_000_000, 0)
	os.Chtimes(p, mtime, mtime)

	dst := filepath.Join(t.TempDir(), "dst")
	opt := fastOpts(2)
	opt.Verify = true
	if r := mustRun(t, plan(t, p, dst, ""), opt); r.Written != 1 || r.Failed != 0 || r.VerifyMismatches != 0 {
		t.Fatalf("result %+v", r)
	}
	got, err := os.ReadFile(filepath.Join(dst, "film.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("copy differs: %d bytes, want %d", len(got), len(data))
	}
	fi, _ := os.Stat(filepath.Join(dst, "film.mp4"))
	if fi.ModTime().Unix() != mtime.Unix() {
		t.Errorf("mtime %v, want %v", fi.ModTime(), mtime)
	}
	leftovers(t, dst)
}
