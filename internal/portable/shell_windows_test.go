//go:build windows

package portable

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
)

// The Shell code can't be tested with a phone in CI, but the Shell treats a
// local folder exactly like a device folder (same Folder/FolderItem objects),
// so walking a temp folder checks names, sizes, dates and recursion for real.
func TestShellWalksARealFolder(t *testing.T) {
	root := t.TempDir()
	dcim := filepath.Join(root, "Internal storage", "DCIM")
	if err := os.MkdirAll(filepath.Join(dcim, "Camera"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "Internal storage", "Empty"), 0o755)
	photo := filepath.Join(dcim, "Camera", "IMG_0001.jpg")
	os.WriteFile(photo, make([]byte, 12345), 0o644)
	when := time.Date(2025, 3, 14, 15, 9, 26, 0, time.Local)
	os.Chtimes(photo, when, when)

	// A zip is a file, even though the Shell can open it like a folder.
	zf, _ := os.Create(filepath.Join(root, "Internal storage", "backup.zip"))
	zw := zip.NewWriter(zf)
	w, _ := zw.Create("inside.txt")
	w.Write([]byte("hello"))
	zw.Close()
	zf.Close()

	// A file over 4 GB must keep its real size (sparse: costs no disk space).
	big := filepath.Join(root, "Internal storage", "movie.mkv")
	bf, _ := os.Create(big)
	bf.Truncate(5 << 30)
	bf.Close()

	s := &Scanner{Open: func(context.Context) (Folder, error) { return openShellPath(root) }}
	b := index.NewBuilder(index.Header{SourceKind: index.KindDevice})
	if err := s.Scan(context.Background(), b, nil); err != nil {
		t.Fatal(err)
	}
	ix := b.Index()
	got := map[string]int{}
	cache := map[uint64]string{}
	for slot := 0; slot < ix.Len(); slot++ {
		got[ix.RelPathCached(slot, cache)] = slot
	}
	for _, want := range []string{`Internal storage`, `Internal storage\DCIM\Camera\IMG_0001.jpg`, `Internal storage\Empty`, `Internal storage\backup.zip`, `Internal storage\movie.mkv`} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s; indexed %v", want, keys(got))
		}
	}
	if _, ok := got[`Internal storage\backup.zip\inside.txt`]; ok {
		t.Error("the inside of a zip must not be indexed as files")
	}
	if slot, ok := got[`Internal storage\DCIM\Camera\IMG_0001.jpg`]; ok {
		if ix.Sizes[slot] != 12345 {
			t.Errorf("size = %d", ix.Sizes[slot])
		}
		if ix.MTimes[slot] != when.Unix() {
			t.Errorf("mtime = %v, want %v", time.Unix(ix.MTimes[slot], 0), when)
		}
	}
	if slot, ok := got[`Internal storage\movie.mkv`]; ok && ix.Sizes[slot] != 5<<30 {
		t.Errorf("5 GB file size = %d", ix.Sizes[slot])
	}
	if slot, ok := got[`Internal storage\backup.zip`]; ok && ix.Flags[slot]&index.FlagDir != 0 {
		t.Error("the zip must be a file")
	}
}

func TestListDoesNotFail(t *testing.T) {
	devs, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		t.Logf("connected: %s serial=%q id=%s (%s)", d.Label(), d.Serial, d.SourceID(), d.InterfacePath)
		if !IsUSBInterface(d.InterfacePath) || d.InstanceID == "" {
			t.Errorf("bad device %+v", d)
		}
	}
}

func TestOpeningAnUnpluggedDeviceExplains(t *testing.T) {
	_, err := OpenDevice(Device{InterfacePath: `\\?\usb#vid_0000&pid_0000#nothere#{6ac27878-a6fa-4155-ba85-f98f491d4f33}`, Name: "Ghost Phone 9"})
	if err == nil || !strings.Contains(err.Error(), "Ghost Phone 9") {
		t.Errorf("err = %v", err)
	}
}

func keys(m map[string]int) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
