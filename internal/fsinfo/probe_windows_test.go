//go:build windows

package fsinfo

import "testing"

// TestRealSystemDrive asks Windows about C:\ (read-only).
func TestRealSystemDrive(t *testing.T) {
	i, err := systemLookup(`C:\`)
	if err != nil {
		t.Fatal(err)
	}
	if i.FileSystem == "" || i.ClusterBytes <= 0 || i.SectorBytes <= 0 {
		t.Errorf("C: = %+v", i)
	}
	t.Logf("C: = %+v", i)
	if r, err := volumeRoot(`C:\Windows\System32`); err != nil || r != `C:\` {
		t.Errorf("volumeRoot = %q, %v", r, err)
	}
}
