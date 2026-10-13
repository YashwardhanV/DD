package index

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestDisplayPath(t *testing.T) {
	env := Env{
		Mounts: map[string]string{"vol-AAAAAAAA": `F:\`},
		Phones: map[string]bool{"adb-PIXEL7": true},
	}
	vol := &Header{SourceKind: KindVolume, SourceID: "vol-AAAAAAAA", Label: "SANDISK 32GB", LastMount: `E:\`}
	offVol := &Header{SourceKind: KindVolume, SourceID: "vol-BBBBBBBB", Label: "SANDISK 32GB", LastMount: `E:\`}
	phone := &Header{SourceKind: KindPhone, SourceID: "adb-PIXEL7", Label: "Pixel 7"}
	offPhone := &Header{SourceKind: KindPhone, SourceID: "adb-OLD", Label: "Galaxy S9"}
	folder := &Header{SourceKind: KindFolder, SourceID: "dir-1", RootPath: filepath.Join(string(filepath.Separator), "data", "Projects")}

	cases := []struct {
		name string
		h    *Header
		rel  string
		want string
	}{
		{"volume online uses the CURRENT letter", vol, `Photos\2024\img.jpg`, filepath.Join(`F:\`, "Photos", "2024", "img.jpg")},
		{"volume offline", offVol, `Photos\2024\img.jpg`, `[SANDISK 32GB · offline · last seen E:] \Photos\2024\img.jpg`},
		{"phone online", phone, `DCIM\Camera\IMG_1.jpg`, "phone:Pixel 7/DCIM/Camera/IMG_1.jpg"},
		{"phone offline", offPhone, `DCIM\a.jpg`, "phone:Galaxy S9/DCIM/a.jpg · offline"},
		{"folder", folder, `src\main.go`, filepath.Join(folder.RootPath, "src", "main.go")},
	}
	for _, c := range cases {
		if got := DisplayPath(c.h, c.rel, env); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestDisplayPathOfflineVolumeWithoutLabelOrMount(t *testing.T) {
	h := &Header{SourceKind: KindVolume, SourceID: "vol-CCCCCCCC"}
	got := DisplayPath(h, `a.txt`, Env{})
	want := `[vol-CCCCCCCC · offline · last seen ?] \a.txt`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEnvOnline(t *testing.T) {
	env := Env{Mounts: map[string]string{"vol-1": `E:\`}, Phones: map[string]bool{"adb-1": true}}
	if !env.Online(&Header{SourceKind: KindVolume, SourceID: "vol-1"}) || env.Online(&Header{SourceKind: KindVolume, SourceID: "vol-2"}) {
		t.Error("volume online detection wrong")
	}
	if !env.Online(&Header{SourceKind: KindPhone, SourceID: "adb-1"}) || env.Online(&Header{SourceKind: KindPhone, SourceID: "adb-2"}) {
		t.Error("phone online detection wrong")
	}
	if !env.Online(&Header{SourceKind: KindFolder}) {
		t.Error("folders are always online")
	}
}

func TestLocalPathAndCopyPath(t *testing.T) {
	env := Env{Mounts: map[string]string{"vol-AAAAAAAA": `F:\`}, Phones: map[string]bool{"adb-PIXEL7": true}}
	vol := &Header{SourceKind: KindVolume, SourceID: "vol-AAAAAAAA", Label: "SANDISK", LastMount: `E:\`}
	off := &Header{SourceKind: KindVolume, SourceID: "vol-BBBBBBBB", Label: "OLD", LastMount: `G:\`}
	phone := &Header{SourceKind: KindPhone, SourceID: "adb-PIXEL7", Label: "Pixel 7", RootPath: "/sdcard"}
	folder := &Header{SourceKind: KindFolder, SourceID: "dir-1", RootPath: filepath.Join(string(filepath.Separator), "data")}

	if p, ok := LocalPath(vol, `a\b.txt`, env); !ok || p != filepath.Join(`F:\`, "a", "b.txt") {
		t.Errorf("online volume: %q %v", p, ok)
	}
	if _, ok := LocalPath(off, `a.txt`, env); ok {
		t.Error("an unplugged drive has no local path")
	}
	if _, ok := LocalPath(phone, `a.txt`, env); ok {
		t.Error("a phone file is not a file on this PC")
	}
	if p, ok := LocalPath(folder, `x\y.txt`, env); !ok || p != filepath.Join(folder.RootPath, "x", "y.txt") {
		t.Errorf("folder source: %q %v", p, ok)
	}

	if got := CopyPath(vol, `a\b.txt`, env); got != filepath.Join(`F:\`, "a", "b.txt") {
		t.Errorf("online: %q", got)
	}
	if got := CopyPath(off, `Old\a.txt`, env); got != JoinRoot(`G:\`, `Old\a.txt`) {
		t.Errorf("offline drive: where it was last seen, got %q", got)
	}
	if got := CopyPath(phone, `DCIM\Camera\a.jpg`, env); got != "phone:/DCIM/Camera/a.jpg" {
		t.Errorf("phone: %q", got)
	}
}

func TestSourceFilter(t *testing.T) {
	a := names("report.pdf")
	a.Header.SourceID = "vol-AAAAAAAA"
	b := names("report.docx")
	b.Header.SourceID = "vol-BBBBBBBB"
	ixs := []*Index{a, b}
	m := &SourceFilter{Inner: NewTermMatcher("report"), SourceID: "vol-BBBBBBBB"}
	res := Search(ixs, m, SearchOptions{Limit: 10})
	if got := hitNames(ixs, res); !reflect.DeepEqual(got, []string{"report.docx"}) || res.Indexes != 1 {
		t.Errorf("got %q %+v", got, res)
	}
}

func TestPortableDevicePaths(t *testing.T) {
	h := &Header{SourceKind: KindDevice, SourceID: "dev-0123456789ab", Label: "Galaxy S21"}
	rel := `Internal storage\DCIM\Camera\IMG_1.jpg`
	on := Env{Devices: map[string]bool{"dev-0123456789ab": true}}
	off := Env{}

	if got := DisplayPath(h, rel, on); got != `device:Galaxy S21\Internal storage\DCIM\Camera\IMG_1.jpg` {
		t.Errorf("online = %q", got)
	}
	if got := DisplayPath(h, rel, off); got != `device:Galaxy S21\Internal storage\DCIM\Camera\IMG_1.jpg · offline` {
		t.Errorf("offline = %q", got)
	}
	if !on.Online(h) || off.Online(h) {
		t.Error("online-ness comes from Env.Devices")
	}
	dir, name := DisplaySplit(h, rel, off)
	if dir != `device:Galaxy S21\Internal storage\DCIM\Camera\` || name != "IMG_1.jpg" {
		t.Errorf("split = %q %q", dir, name)
	}
	if _, ok := LocalPath(h, rel, on); ok {
		t.Error("a file on a device is not a path on this PC")
	}
	if got := CopyPath(h, rel, on); got != `device:Galaxy S21\Internal storage\DCIM\Camera\IMG_1.jpg` {
		t.Errorf("copy path = %q", got)
	}
	if got := DevicePath(&Header{SourceID: "dev-x"}, ""); got != "device:dev-x" {
		t.Errorf("no label, no rel = %q", got)
	}
}
