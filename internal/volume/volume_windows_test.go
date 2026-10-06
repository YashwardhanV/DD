//go:build windows

package volume

import "testing"

// These tests talk to the real Windows APIs. They only read: C:\ is always there.
func TestListIncludesSystemDrive(t *testing.T) {
	vols, err := List()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols {
		if v.Root == `C:\` {
			if !v.Ready || v.FileSystem != "NTFS" || v.Serial == 0 || v.TotalBytes == 0 {
				t.Errorf("C:\\ looks wrong: %+v", v)
			}
			return
		}
	}
	t.Fatalf("C:\\ not found in %+v", vols)
}

func TestForPathFindsContainingDrive(t *testing.T) {
	v, err := ForPath(`C:\Windows\System32`)
	if err != nil || v.Root != `C:\` {
		t.Errorf("ForPath = %+v, %v", v, err)
	}
	if _, err := ForPath(`\\server\share\x`); err == nil {
		t.Error("UNC paths should be rejected")
	}
}
