package index

import (
	"path/filepath"
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
