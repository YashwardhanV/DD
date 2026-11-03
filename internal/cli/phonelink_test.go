package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestCopyFromPhonePlansFromTheIndex(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fs := phonetest.NewFS()
	old := time.Now().Add(-48 * time.Hour).Unix()
	for _, n := range []string{"a", "b", "c"} {
		fs.Add("/sdcard/DCIM/Camera/"+n+".jpg", "photo "+n, old)
	}
	useFS(t, fs)
	fakeDriveType(t, false, true)
	if out, err := run(t, "index", "phone"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	fs.Add("/sdcard/DCIM/Camera/new.jpg", "taken after indexing", time.Now().Unix())
	fs.Remove("/sdcard/DCIM/Camera/c.jpg")

	dst := filepath.Join(t.TempDir(), "d")
	out, err := run(t, "copy", "phone:/DCIM/Camera", dst)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Planned from the phone index (made just now) + 1 changed file.", "Copied 3 files", "1 file had been deleted since the index was made (vanished)", "Phone index updated with 1 changed file."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, c := range fs.ShellCommands() {
		if strings.HasPrefix(c, "find /sdcard/DCIM/Camera") && !strings.Contains(c, "-cmin") {
			t.Errorf("a full listing ran: %s", c)
		}
	}
	// The refreshed index now finds the new photo.
	out, _ = run(t, "search", "new.jpg")
	if !strings.Contains(out, "new.jpg") {
		t.Errorf("search after the refresh:\n%s", out)
	}

	// --fresh lists the phone like v1.
	out, err = run(t, "copy", "phone:/DCIM/Camera", filepath.Join(t.TempDir(), "e"), "--fresh")
	if err != nil || !strings.Contains(out, "Listed the phone folder (a fresh listing was asked for).") {
		t.Errorf("--fresh: %v\n%s", err, out)
	}
	if _, err := run(t, "copy", "phone:/DCIM/Camera", t.TempDir(), "--plan", "sideways"); err == nil {
		t.Error("--plan sideways must be refused")
	}
}

func TestCopyFromPhoneUsesTheTarStream(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", t.TempDir())
	fs := phonetest.NewFS()
	for i := 0; i < 150; i++ {
		fs.Add("/sdcard/WhatsApp/Media/IMG-"+strings.Repeat("0", 3)+string(rune('A'+i%26))+string(rune('a'+i/26))+".jpg", "media", 1_700_000_000)
	}
	useFS(t, fs)
	fakeDriveType(t, false, true)
	out, err := run(t, "copy", "phone:/WhatsApp/Media", filepath.Join(t.TempDir(), "d"), "--trace")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Copied 150 files") || !strings.Contains(out, "Phone files came by tar stream") || len(fs.TarCalls()) != 1 || fs.PullCount() != 0 {
		t.Errorf("tar streams %d, pulls %d:\n%s", len(fs.TarCalls()), fs.PullCount(), out)
	}
	if _, err := run(t, "copy", "phone:/WhatsApp/Media", t.TempDir(), "--phone-mode", "fax"); err == nil {
		t.Error("--phone-mode fax must be refused")
	}
	out, _ = run(t, "copy", "phone:/WhatsApp/Media", filepath.Join(t.TempDir(), "e"), "--v1")
	if !strings.Contains(out, "Phone files came by adb pull.") {
		t.Errorf("--v1 must use adb pull:\n%s", out)
	}
}
