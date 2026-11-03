//go:build windows

package copier

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// These run on the Windows CI: real reparse points and real unbuffered I/O.

func TestSingleOpenRefusesSymlinksAndJunctions(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "f.txt"), []byte("x"), 0o644)
	if f, _, _, err := openSourceSingle(filepath.Join(target, "f.txt")); err != nil {
		t.Fatalf("a regular file: %v", err)
	} else {
		f.Close()
	}
	if err := os.Symlink(filepath.Join(target, "f.txt"), filepath.Join(dir, "link.txt")); err == nil {
		if _, _, _, err := openSourceSingle(filepath.Join(dir, "link.txt")); err != errNotRegular {
			t.Errorf("a file symlink: %v, want errNotRegular", err)
		}
	} else {
		t.Logf("no symlink privilege (%v) - skipping that part", err)
	}
	junction := filepath.Join(dir, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v %s", err, out)
	}
	if _, _, _, err := openSourceSingle(junction); err != errNotRegular {
		t.Errorf("a junction: %v, want errNotRegular", err)
	}
	if _, _, _, err := openSourceSingle(filepath.Join(dir, "missing")); err != errSourceGone {
		t.Errorf("a missing file: %v, want errSourceGone", err)
	}
}

func TestUnbufferedCopyOnWindows(t *testing.T) {
	old := unbufferedMin
	unbufferedMin = 1 << 20
	defer func() { unbufferedMin = old }()
	src := t.TempDir()
	for name, size := range map[string]int{"exact.bin": 4 << 20, "odd.bin": 5<<20 + 4097, "small.bin": 100} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i * 7)
		}
		os.WriteFile(filepath.Join(src, name), data, 0o644)
	}
	dst := filepath.Join(t.TempDir(), "out")
	opt := fastAll()
	opt.Verify = true
	r := mustRun(t, plan(t, src, dst, OnConflictNewer), opt)
	if r.Failed != 0 || r.VerifyMismatches != 0 {
		t.Fatalf("%+v", r)
	}
	sameTree(t, src, filepath.Join(dst, filepath.Base(src)))
}
