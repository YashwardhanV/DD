package copier

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/yashwardhanv/dd/internal/copier/trace"
)

func TestAlignedBuffer(t *testing.T) {
	for _, n := range []int{4096, 1 << 20, unbufferedBufSize} {
		b := alignedBuffer(n)
		if len(b) != n || cap(b) != n || uintptr(unsafe.Pointer(&b[0]))%sectorAlign != 0 {
			t.Errorf("alignedBuffer(%d): len %d cap %d addr %% 4096 = %d", n, len(b), cap(b), uintptr(unsafe.Pointer(&b[0]))%sectorAlign)
		}
	}
	for in, want := range map[int]int{0: 0, 1: 4096, 4095: 4096, 4096: 4096, 4097: 8192} {
		if roundUp(in) != want {
			t.Errorf("roundUp(%d) = %d", in, roundUp(in))
		}
	}
}

// fileSum hashes a file.
func fileSum(t *testing.T, p string) [32]byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	var s [32]byte
	copy(s[:], h.Sum(nil))
	return s
}

func TestCopyAlignedSizes(t *testing.T) {
	dir := t.TempDir()
	sizes := []int64{0, 1, 4095, 4096, 4097, 3*sectorAlign + 17, 256<<20 + 1}
	for _, size := range sizes {
		for _, srcUnbuf := range []bool{false, true} {
			src := filepath.Join(dir, "src")
			f, _ := os.Create(src)
			if size > 1<<20 {
				f.Truncate(size) // sparse: 256 MB of zeros without writing them
				f.WriteAt([]byte("end!"), size-4)
				f.WriteAt([]byte("start"), 0)
			} else {
				data := bytes.Repeat([]byte("0123456789abcdef"), int(size)/16+1)[:size]
				f.Write(data)
			}
			f.Close()

			in, _ := os.Open(src)
			out, _ := os.Create(filepath.Join(dir, "dst"))
			var r io.Reader = in
			if srcUnbuf {
				r = &unbufferedSource{f: in}
			}
			n, err := copyAligned(context.Background(), out, r, alignedBuffer(1<<20), srcUnbuf)
			in.Close()
			out.Close()
			if err != nil || n != size {
				t.Fatalf("size %d (unbuffered source %v): n=%d err=%v", size, srcUnbuf, n, err)
			}
			st, _ := os.Stat(filepath.Join(dir, "dst"))
			if st.Size() != size {
				t.Errorf("size %d: the copy is %d bytes (the padding was not cut off)", size, st.Size())
			}
			if fileSum(t, src) != fileSum(t, filepath.Join(dir, "dst")) {
				t.Errorf("size %d (unbuffered source %v): contents differ", size, srcUnbuf)
			}
		}
	}
	if _, err := copyAligned(context.Background(), nil, nil, make([]byte, 100), false); err == nil {
		t.Error("an unaligned buffer size must be refused")
	}
}

func fastAll() Options {
	o := fastOpts(4)
	o.Fast = FastPaths{SingleOpen: true, Unbuffered: true, MediaVerify: true}
	return o
}

func TestSingleOpenUsesTheFilesRealTime(t *testing.T) {
	src, _ := source(t, 40)
	dst := filepath.Join(t.TempDir(), "out")
	p := plan(t, src, dst, OnConflictNewer)
	// The source changes after planning: the plan's times are now stale.
	changed := filepath.Join(src, p.Items[0].Rel[len(filepath.Base(src))+1:])
	newTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(changed, newTime, newTime)

	r := mustRun(t, p, fastAll())
	if r.Failed != 0 {
		t.Fatalf("%+v", r.Failures)
	}
	st, _ := os.Stat(filepath.Join(dst, p.Items[0].Rel))
	if !st.ModTime().Equal(newTime) {
		t.Errorf("single-open copy has time %v, want the file's real time %v", st.ModTime(), newTime)
	}
	// v1 uses the plan's time.
	dst2 := filepath.Join(t.TempDir(), "out")
	p2 := plan(t, src, dst2, OnConflictNewer)
	os.Chtimes(changed, newTime.Add(time.Minute), newTime.Add(time.Minute))
	mustRun(t, p2, fastOpts(4))
	st, _ = os.Stat(filepath.Join(dst2, p.Items[0].Rel))
	if st.ModTime().Equal(newTime.Add(time.Minute)) {
		t.Error("v1 should keep using the plan's time")
	}
}

func TestSingleOpenSkipsLinksAndReportsVanished(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "real.txt"), []byte("real"), 0o644)
	os.WriteFile(filepath.Join(src, "gone.txt"), []byte("gone"), 0o644)
	if err := os.Symlink(filepath.Join(src, "real.txt"), filepath.Join(src, "link.txt")); err != nil {
		t.Skip("symlinks not available:", err)
	}
	dst := t.TempDir()
	p := plan(t, src, dst, OnConflictNewer)
	p.MayBeStale = true
	os.Remove(filepath.Join(src, "gone.txt"))
	r := mustRun(t, p, fastAll())
	if r.Written != 1 || r.NotRegular != 1 || r.Vanished != 1 || r.Failed != 0 {
		t.Errorf("%+v", r)
	}
}

func TestMediaVerifyUsesTheTeedChecksum(t *testing.T) {
	src, res := source(t, 30)
	opt := fastAll()
	opt.Verify = true
	r := mustRun(t, plan(t, src, filepath.Join(t.TempDir(), "o"), OnConflictNewer), opt)
	if r.Failed != 0 || r.VerifyMismatches != 0 || r.Written != int64(res.Files) {
		t.Fatalf("%+v", r)
	}
	// Corrupt each copy right after it lands: the tee'd source checksum must
	// disagree with the re-read copy.
	afterCopy = func(dst string) {
		if st, _ := os.Stat(dst); st != nil && st.Size() > 0 {
			f, _ := os.OpenFile(dst, os.O_WRONLY, 0)
			f.WriteAt([]byte{0xFF}, 0)
			f.Close()
		}
	}
	defer func() { afterCopy = nil }()
	opt.Retries = 1
	dst := filepath.Join(t.TempDir(), "o")
	r, _ = Run(context.Background(), plan(t, src, dst, OnConflictNewer), opt)
	if r.VerifyMismatches == 0 || r.Failed == 0 {
		t.Errorf("corruption not detected: %+v", r)
	}
}

func TestDirectWritesWithResumeJournal(t *testing.T) {
	src, res := source(t, 120)
	dst := filepath.Join(t.TempDir(), "new-folder") // doesn't exist yet
	opt := fastOpts(4)
	opt.Fast.Direct = true
	tr := trace.New()
	opt.Trace = tr
	r := mustRun(t, plan(t, src, dst, OnConflictNewer), opt)
	if r.Written != int64(res.Files) || r.Failed != 0 {
		t.Fatalf("%+v", r)
	}
	if n := tr.Report(r.Elapsed, trace.Context{}).Time(trace.Rename); n != 0 {
		t.Errorf("direct writes renamed files (%v)", n)
	}
	if _, err := os.Stat(filepath.Join(dst, ResumeJournalName)); !os.IsNotExist(err) {
		t.Error("the journal must be deleted after a complete copy")
	}
	sameTree(t, src, filepath.Join(dst, filepath.Base(src)))

	// An existing destination: never direct.
	opt.Trace = trace.New()
	r = mustRun(t, plan(t, src, t.TempDir(), OnConflictNewer), opt)
	if opt.Trace.Report(r.Elapsed, trace.Context{}).Time(trace.Rename) == 0 {
		t.Error("an existing destination must use .ddxpart + rename")
	}
}

func TestResumeJournalAfterACrash(t *testing.T) {
	src, res := source(t, 50)
	dst := filepath.Join(t.TempDir(), "d")
	opt := fastOpts(2)
	opt.Fast.Direct = true
	p := plan(t, src, dst, OnConflictNewer)
	mustRun(t, p, opt)

	// Simulate a crash during a direct copy: a journal that lists only some
	// files, and one file cut off under its real name (newer than the source!).
	var listed []string
	for i, it := range p.Items {
		if i%2 == 0 {
			listed = append(listed, filepath.ToSlash(it.Rel))
		}
	}
	os.WriteFile(filepath.Join(dst, ResumeJournalName), []byte(strings.Join(listed, "\n")+"\nhalf-a-li"), 0o644)
	victim := filepath.Join(dst, p.Items[1].Rel)
	os.WriteFile(victim, []byte("cut"), 0o644) // mtime = now: "newer" than the source

	// The re-run doesn't even ask for direct writes: the journal is honoured anyway.
	r := mustRun(t, plan(t, src, dst, OnConflictNewer), fastOpts(2))
	if r.Failed != 0 || r.Written != int64(res.Files-len(listed)) {
		t.Errorf("re-run: %+v (want %d re-copied)", r, res.Files-len(listed))
	}
	sameTree(t, src, filepath.Join(dst, filepath.Base(src)))
	if _, err := os.Stat(filepath.Join(dst, ResumeJournalName)); !os.IsNotExist(err) {
		t.Error("journal left after a complete re-run")
	}

	// Cancelled direct copy: the journal stays; the next run completes.
	dst2 := filepath.Join(t.TempDir(), "d2")
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int64
	afterCopy = func(string) {
		if n.Add(1) == 10 {
			cancel()
		}
	}
	Run(ctx, plan(t, src, dst2, OnConflictNewer), opt)
	afterCopy = nil
	if _, err := os.Stat(filepath.Join(dst2, ResumeJournalName)); err != nil {
		t.Errorf("journal missing after a cancelled direct copy: %v", err)
	}
	mustRun(t, plan(t, src, dst2, OnConflictNewer), opt)
	sameTree(t, src, filepath.Join(dst2, filepath.Base(src)))
}
