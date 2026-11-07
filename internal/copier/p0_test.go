package copier

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/phone/phonetest"
)

// writeAt creates path with content and the given modification time.
func writeAt(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// TestModTimePreservedTo100ns: a copy keeps the source's modification time at
// full NTFS precision (100 ns), not rounded to whole seconds.
func TestModTimePreservedTo100ns(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	when := time.Unix(1_700_000_123, 456_789_100) // a multiple of 100 ns
	writeAt(t, filepath.Join(src, "a.txt"), "hello", when)
	res := mustRun(t, plan(t, src, dst, ""), fastOpts(2))
	if res.Written != 1 {
		t.Fatalf("result %+v", res)
	}
	st, err := os.Stat(filepath.Join(dst, filepath.Base(src), "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(when) {
		t.Errorf("copy's time = %v, want %v (to the 100 ns)", st.ModTime().UnixNano(), when.UnixNano())
	}
	// And a second run recognises it as already there.
	if res := mustRun(t, plan(t, src, dst, ""), fastOpts(2)); res.Written != 0 || res.Skipped != 1 {
		t.Errorf("second run %+v, want everything skipped", res)
	}
}

// TestINV4_SourceThatChangesDuringCopyIsRejected: a file that another program
// appends to while it is copied is not accepted (it would be a mix of old and
// new bytes under the old time), and nothing appears under its real name.
func TestINV4_SourceThatChangesDuringCopyIsRejected(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(src, "log.txt"), "first line\n", time.Unix(1_700_000_000, 0))
	p := plan(t, src, dst, "")

	var opens atomic.Int64
	openSource = func(name string, locked bool) (*os.File, error) {
		opens.Add(1)
		// Another program appends just before DriveDex reads (before the
		// open, so the locked open isn't what stops it).
		f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			f.WriteString("another line\n")
			f.Close()
		}
		return defaultOpenSource(name, locked)
	}
	defer func() { openSource = defaultOpenSource }()

	res := mustRun(t, p, fastOpts(1))
	if res.Failed != 1 || res.Written != 0 {
		t.Fatalf("result %+v, want one failure", res)
	}
	if !strings.Contains(res.Failures[0].Err, "kept changing") {
		t.Errorf("failure text %q should say the file kept changing", res.Failures[0].Err)
	}
	if opens.Load() != 3 {
		t.Errorf("attempts = %d, want 3 (first try + 2 retries)", opens.Load())
	}
	if _, err := os.Stat(filepath.Join(dst, filepath.Base(src), "log.txt")); err == nil {
		t.Error("the file must not appear under its real name")
	}
	leftovers(t, dst)
}

// TestVerifyMediaReadsSourceOnceAndDestinationUnbuffered: --verify (media)
// hashes the source while copying (one read) and reads the copy back through
// the unbuffered opener.
func TestVerifyMediaReadsSourceOnceAndDestinationUnbuffered(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for i, size := range []int{0, 1, bufferSize - 1, bufferSize, bufferSize + 4097} {
		writeAt(t, filepath.Join(src, "f"+string(rune('a'+i))), strings.Repeat("z", size), time.Unix(1_700_000_000, 0))
	}
	var opens, unbuffered atomic.Int64
	openSource = func(name string, locked bool) (*os.File, error) {
		opens.Add(1)
		return defaultOpenSource(name, locked)
	}
	openUnbuffered = func(name string) (*os.File, error) {
		unbuffered.Add(1)
		return unbufferedOpen(name)
	}
	defer func() { openSource, openUnbuffered = defaultOpenSource, unbufferedOpen }()

	opt := fastOpts(2)
	opt.Verify = VerifyMedia
	p := plan(t, src, dst, "")
	res := mustRun(t, p, opt)
	if res.Written != 5 || res.Failed != 0 || res.VerifyMismatches != 0 {
		t.Fatalf("result %+v", res)
	}
	if opens.Load() != 5 {
		t.Errorf("source opened %d times for 5 files, want 5 (read once)", opens.Load())
	}
	if unbuffered.Load() != 5 {
		t.Errorf("unbuffered read-backs = %d, want 5", unbuffered.Load())
	}
	for _, it := range p.Items {
		if !it.Hashed || !it.MediaChecked {
			t.Errorf("%s: hashed=%v mediaChecked=%v", it.Rel, it.Hashed, it.MediaChecked)
		}
	}
}

// TestVerifyMediaCatchesACorruptCopyAndRetries: a byte flipped on the way
// (simulated after the rename) is caught, the bad copy deleted and the file
// copied again.
func TestVerifyMediaCatchesACorruptCopyAndRetries(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(src, "photo.jpg"), strings.Repeat("p", 5000), time.Unix(1_700_000_000, 0))
	var corrupted atomic.Bool
	afterCopy = func(path string) {
		if corrupted.CompareAndSwap(false, true) {
			b, _ := os.ReadFile(path)
			b[100] ^= 0xFF
			os.WriteFile(path, b, 0o644)
		}
	}
	defer func() { afterCopy = nil }()
	opt := fastOpts(1)
	opt.Verify = VerifyMedia
	res := mustRun(t, plan(t, src, dst, ""), opt)
	if res.Written != 1 || res.VerifyMismatches != 1 || res.Failed != 0 {
		t.Fatalf("result %+v, want one mismatch then a good copy", res)
	}
	got, _ := os.ReadFile(filepath.Join(dst, filepath.Base(src), "photo.jpg"))
	if !bytes.Equal(got, []byte(strings.Repeat("p", 5000))) {
		t.Error("the final copy is not identical")
	}
}

func TestVerifyCacheModeStillChecks(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeAt(t, filepath.Join(src, "a"), "abc", time.Unix(1_700_000_000, 0))
	opt := fastOpts(1)
	opt.Verify = VerifyCache
	p := plan(t, src, dst, "")
	if res := mustRun(t, p, opt); res.Written != 1 || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
	if !p.Items[0].Hashed || p.Items[0].MediaChecked {
		t.Errorf("cache mode: hashed=%v mediaChecked=%v (want true, false)", p.Items[0].Hashed, p.Items[0].MediaChecked)
	}
}

func TestParseVerifyMode(t *testing.T) {
	for in, want := range map[string]VerifyMode{"": VerifyOff, "off": VerifyOff, "media": VerifyMedia, "true": VerifyMedia, "CACHE": VerifyCache} {
		if got, ok := ParseVerifyMode(in); !ok || got != want {
			t.Errorf("ParseVerifyMode(%q) = %v, %v", in, got, ok)
		}
	}
	if _, ok := ParseVerifyMode("sometimes"); ok {
		t.Error("nonsense accepted")
	}
}

func TestAlignedBufferIsAligned(t *testing.T) {
	for i := 0; i < 20; i++ {
		b := alignedBuffer(8192, 4096)
		if len(b) != 8192 || addr(b)%4096 != 0 {
			t.Fatalf("len %d, address %% 4096 = %d", len(b), addr(b)%4096)
		}
	}
}

// ---- phones ---------------------------------------------------------------------

func TestPhoneVerifyUsesThePhonesHashToolAndCatchesCorruption(t *testing.T) {
	fp := phonetest.NewFS()
	sampleCamera(fp, 3)
	fp.CorruptPull["/sdcard/DCIM/Camera/IMG_0001.jpg"] = true
	fp.CorruptTimes["/sdcard/DCIM/Camera/IMG_0001.jpg"] = 1 // a bad cable, once

	tool, err := fp.ADB().HashTool(context.Background(), serial)
	if err != nil || tool != "sha256sum" {
		t.Fatalf("HashTool = %q, %v", tool, err)
	}
	dst := t.TempDir()
	p := phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, "")
	p.Phone.HashTool = tool
	opt := fastOpts(2)
	opt.Verify = VerifyMedia
	res := mustRun(t, p, opt)
	if res.Written != 3 || res.Failed != 0 || res.VerifyMismatches != 1 {
		t.Fatalf("result %+v, want 3 written after one mismatch", res)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "Camera", "IMG_0001.jpg"))
	if string(got) != strings.Repeat("x", 11) {
		t.Errorf("IMG_0001 = %q", got)
	}
}

func TestPhoneVerifyFailsAFileThatIsAlwaysCorrupt(t *testing.T) {
	fp := phonetest.NewFS()
	sampleCamera(fp, 2)
	fp.CorruptPull["/sdcard/DCIM/Camera/IMG_0000.jpg"] = true
	dst := t.TempDir()
	p := phonePlan(t, fp, "/sdcard/DCIM/Camera", dst, "")
	p.Phone.HashTool = "md5sum"
	opt := fastOpts(2)
	opt.Verify = VerifyCache
	res := mustRun(t, p, opt)
	if res.Written != 1 || res.Failed != 1 {
		t.Fatalf("result %+v, want 1 written and 1 failed", res)
	}
	if _, err := os.Stat(filepath.Join(dst, "Camera", "IMG_0000.jpg")); err == nil {
		t.Error("a copy that never matched must not be left behind")
	}
}

func TestPhoneWithoutHashToolsReportsNone(t *testing.T) {
	fp := phonetest.NewFS()
	fp.HashTools = nil
	if tool, err := fp.ADB().HashTool(context.Background(), serial); err != nil || tool != "" {
		t.Errorf("HashTool = %q, %v; want none", tool, err)
	}
}
