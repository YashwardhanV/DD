//go:build windows

package copier

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFirstLCNOnRealNTFS: a file with real clusters has a position; a tiny
// file stored inside its MFT record has none.
func TestFirstLCNOnRealNTFS(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	os.WriteFile(big, make([]byte, 256<<10), 0o644)
	lcn, ok := platformFirstLCN(big)
	if !ok {
		// Seen on the owner's PC: C: answers FSCTL_GET_RETRIEVAL_POINTERS
		// with "not supported" (so does Windows' own fsutil file
		// queryextents). Disk order then falls back to reference numbers.
		t.Skip("this volume doesn't report cluster positions")
	}
	if lcn <= 0 {
		t.Errorf("256 KB file: lcn %d", lcn)
	}
	tiny := filepath.Join(dir, "tiny.txt")
	os.WriteFile(tiny, []byte("hi"), 0o644)
	if _, ok := platformFirstLCN(tiny); ok {
		t.Log("a 2-byte file reported a cluster (not resident on this volume) - allowed, just unusual")
	}
	if _, ok := platformFirstLCN(filepath.Join(dir, "missing")); ok {
		t.Error("a missing file has no position")
	}
}
