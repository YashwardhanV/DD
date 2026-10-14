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

// SeekPenalty must never crash, whatever the answer. (It may be "unknown" on
// some machines; that is allowed.)
func TestSeekPenaltyDoesNotCrash(t *testing.T) {
	penalty, known := SeekPenalty(`C:\`)
	t.Logf("C:\\ seek penalty = %v (known = %v)", penalty, known)
	if _, known := SeekPenalty(`\\server\share`); known {
		t.Error("network paths should be unknown")
	}
}

// Some storage drivers (Intel RST's iaStorAC) refuse the question on the volume
// with "Incorrect function"; BusType then asks the physical disk, so the system
// drive must always give an answer.
func TestBusTypeAnswersForSystemDrive(t *testing.T) {
	info, err := BusType(`C:\`)
	if err != nil || info.Bus == 0 {
		t.Errorf(`BusType(C:\) = %+v, %v`, info, err)
	}
	if _, err := volumeDisk("C:"); err != nil {
		t.Errorf("volumeDisk(C:): %v", err)
	}
}

// The disk Windows runs from is never hot-pluggable, and the question must be
// answered (through the physical disk if the volume refuses it) without error.
func TestSystemDriveIsInternal(t *testing.T) {
	info, err := BusType(`C:\`)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf(`C:\ bus %s, hotplug %v`, BusName(info.Bus), info.Hotplug)
	v, err := ForPath(`C:\`)
	if err != nil {
		t.Fatal(err)
	}
	if IsExternal(v, info, "C:") {
		t.Error("the system drive must never count as external")
	}
}

// The USB questions must never crash, whatever this machine has (a CI
// machine usually has no USB at all). C: is normally not USB.
func TestUSBQuestionsDoNotCrash(t *testing.T) {
	ports, err := USBPorts()
	t.Logf("USB ports: %d (err %v), summary %+v", len(ports), err, Summarize(ports))
	l, err := USBLinkFor(`C:\`)
	t.Logf("C:\\ USB link: %+v, err %v", l, err)
	if err == nil && l.Speed == SpeedUnknown {
		t.Error("a link without an error should have a speed")
	}
	if _, err := USBLinkFor(`\\server\share`); err == nil {
		t.Error("network paths have no USB link")
	}
}
