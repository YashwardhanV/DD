package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/phone/phonetest"
	"github.com/yashwardhanv/dd/internal/volume"
)

// fakePhoneLinks gives phones (by serial) a USB link; others "can't tell".
func fakePhoneLinks(t *testing.T, links map[string]volume.USBLink) {
	t.Helper()
	old := phoneUSBLink
	phoneUSBLink = func(serial string) (volume.USBLink, error) {
		if l, ok := links[serial]; ok {
			return l, nil
		}
		return volume.USBLink{}, volume.ErrPhoneNotMatched
	}
	t.Cleanup(func() { phoneUSBLink = old })
}

func TestCopyFromPhonePrintsTheUSBSpeedFirst(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fs := phonetest.NewFS()
	fs.Add("/sdcard/DCIM/Camera/a.jpg", "aaaa", 1_700_000_000)
	useFS(t, fs)
	fakeDriveType(t, false, true)

	fakePhoneLinks(t, map[string]volume.USBLink{"R58M1234ABC": {Speed: volume.SpeedHigh, Capable: volume.SpeedHigh}})
	out, err := run(t, "copy", "phone:/DCIM/Camera", filepath.Join(t.TempDir(), "d"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Pixel 7 connected at USB 2.0 · 480 Mbps - max ≈ 40 MB/s") || !strings.Contains(out, "this phone's port is USB 2.0") {
		t.Errorf("USB 2.0 phone line missing:\n%s", out)
	}

	fakePhoneLinks(t, nil)
	out, _ = run(t, "copy", "phone:/DCIM/Camera", filepath.Join(t.TempDir(), "d"))
	if !strings.Contains(out, "Pixel 7: USB speed: can't tell") {
		t.Errorf("unknown speed must say so, never guess:\n%s", out)
	}
}

func TestDrivesListsPhonesWithTheirUSBSpeed(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	oldList, oldPhones := listVolumes, connectedPhones
	listVolumes = func() ([]volume.Volume, error) { return nil, nil }
	connectedPhones = func() []phone.Device {
		return []phone.Device{
			{Serial: "R58M1234ABC", State: "device", Model: "Pixel 7"},
			{Serial: "XYZ", State: "unauthorized"},
		}
	}
	t.Cleanup(func() { listVolumes, connectedPhones = oldList, oldPhones })
	fakePhoneLinks(t, map[string]volume.USBLink{"R58M1234ABC": {Speed: volume.SpeedHigh, Capable: volume.SpeedSuper, PortKnown: true}})

	out, err := run(t, "drives")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PHONE", "CONNECTION", "phone:R58M1234ABC", "USB 2.0 · 480 Mbps ⚠", "USB 2.0 only", "phone:XYZ", "unauthorized"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}
