//go:build windows

package copier

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func addr(b []byte) uintptr { return uintptr(unsafe.Pointer(&b[0])) }

// TestVerifyMediaOpensWithoutTheCache: the read-back opener asks Windows for
// FILE_FLAG_NO_BUFFERING, and really reads a file with it.
func TestVerifyMediaOpensWithoutTheCache(t *testing.T) {
	if unbufferedFlags&windows.FILE_FLAG_NO_BUFFERING == 0 {
		t.Fatal("the read-back must bypass the Windows cache")
	}
	path := filepath.Join(t.TempDir(), "x")
	os.WriteFile(path, make([]byte, 12345), 0o644)
	sum, err := hashUnbuffered(t.Context(), path, alignedBuffer(bufferSize, maxSectorAlign))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hashCached(t.Context(), path, make([]byte, bufferSize))
	if sum != want {
		t.Error("unbuffered and cached reads disagree")
	}
}

// TestLockedOpenReportsAFileInUse: a file another program has open for
// writing can't be opened "share-read only"; the copy retries and finally
// copies it with a normal open (guarded by the before/after check).
func TestLockedOpenReportsAFileInUse(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	path := filepath.Join(src, "open.db")
	writeAt(t, path, "data", time.Unix(1_700_000_000, 0))
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := lockedOpen(path); !errors.Is(err, errInUse) {
		t.Fatalf("lockedOpen = %v, want errInUse", err)
	}
	res := mustRun(t, plan(t, src, dst, ""), fastOpts(1))
	if res.Written != 1 || res.Failed != 0 {
		t.Errorf("result %+v: the last attempt opens normally and succeeds", res)
	}
}
