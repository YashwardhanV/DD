//go:build windows

package ntfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// fileID is the NTFS file reference number of path, asked the slow way.
func fileID(t *testing.T, path string) uint64 {
	t.Helper()
	p, _ := windows.UTF16PtrFromString(path)
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		t.Fatal(err)
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
}

// TestWindowsListerMatchesPerFileAnswers lists a real NTFS folder in one go
// and checks every record against what Windows says about each file.
func TestWindowsListerMatchesPerFileAnswers(t *testing.T) {
	dir := t.TempDir()
	when := time.Unix(1_700_000_123, 456_789_100)
	for i, name := range []string{"a.txt", "résumé.pdf", "big.bin"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, make([]byte, 1000*(i+1)), 0o644)
		os.Chtimes(p, when, when)
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)

	recs, err := WindowsLister{}.ListDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("%d records, want 4: %+v", len(recs), recs)
	}
	for _, r := range recs {
		full := filepath.Join(dir, r.Name)
		st, err := os.Lstat(full)
		if err != nil {
			t.Fatalf("%s: %v", r.Name, err)
		}
		if r.FileID != fileID(t, full) {
			t.Errorf("%s: file ID %x, want %x", r.Name, r.FileID, fileID(t, full))
		}
		if !st.IsDir() && (r.Size != st.Size() || r.MTimeNs != st.ModTime().UnixNano()) {
			t.Errorf("%s: size %d time %d, want %d %d", r.Name, r.Size, r.MTimeNs, st.Size(), st.ModTime().UnixNano())
		}
		if (r.Attrs&AttrDirectory != 0) != st.IsDir() {
			t.Errorf("%s: directory attribute wrong", r.Name)
		}
	}
	if _, err := (WindowsLister{}).ListDir(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing folder must fail")
	}
}
