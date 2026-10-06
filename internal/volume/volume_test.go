package volume

import "testing"

func TestSourceID(t *testing.T) {
	if got := SourceID(Volume{Serial: 0x1A2B3C4D}); got != "vol-1A2B3C4D" {
		t.Errorf("got %q", got)
	}
	if got := SourceID(Volume{Serial: 0xAB}); got != "vol-000000AB" {
		t.Errorf("serials are zero-padded to 8 digits, got %q", got)
	}
}

func TestIsDriveRoot(t *testing.T) {
	yes := []string{"E:", "e:", `E:\`, "E:/", "  d:  "}
	no := []string{`E:\Photos`, "E", `\\server\share`, "", ".", "EE:", `C:\Users\`}
	for _, s := range yes {
		if !IsDriveRoot(s) {
			t.Errorf("IsDriveRoot(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if IsDriveRoot(s) {
			t.Errorf("IsDriveRoot(%q) = true, want false", s)
		}
	}
}

func TestNormalizeLetter(t *testing.T) {
	for in, want := range map[string]string{"e": "E:", "E:": "E:", `e:\`: "E:", " f ": "F:"} {
		if got, ok := NormalizeLetter(in); !ok || got != want {
			t.Errorf("NormalizeLetter(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "ab", `E:\x`, "1"} {
		if _, ok := NormalizeLetter(in); ok {
			t.Errorf("NormalizeLetter(%q) should fail", in)
		}
	}
}

func TestVolumeHelpers(t *testing.T) {
	v := Volume{Root: `E:\`, DriveType: DriveRemovable}
	if v.Letter() != "E:" || v.TypeName() != "Removable" {
		t.Errorf("Letter=%q TypeName=%q", v.Letter(), v.TypeName())
	}
	if (Volume{DriveType: DriveFixed}).TypeName() != "Fixed" || (Volume{DriveType: DriveRemote}).TypeName() != "Network" {
		t.Error("type names wrong")
	}
}
