package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/portable"
)

// fakeFolder is a folder on a pretend phone.
type fakeFolder struct {
	items []fakeItem
}

type fakeItem struct {
	portable.Item
	sub *fakeFolder
}

func (f *fakeFolder) List(context.Context) ([]portable.Entry, error) {
	var out []portable.Entry
	for _, it := range f.items {
		e := portable.Entry{Item: it.Item}
		if it.sub != nil {
			e.Sub = it.sub
		}
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeFolder) Close() {}

var testGalaxy = portable.Device{
	InterfacePath: `\\?\usb#vid_04e8&pid_6860#R58M12345#{6ac27878-a6fa-4155-ba85-f98f491d4f33}`,
	InstanceID:    `USB\VID_04E8&PID_6860\R58M12345`, Serial: "R58M12345", Name: "Galaxy S21",
}

// connectDevice pretends a phone in "File transfer" mode is plugged in.
func connectDevice(t *testing.T, root *fakeFolder, devs ...portable.Device) {
	t.Helper()
	oldList, oldOpen := listDevices, openDevice
	listDevices = func() ([]portable.Device, error) { return devs, nil }
	openDevice = func(portable.Device) func(context.Context) (portable.Folder, error) {
		return func(context.Context) (portable.Folder, error) {
			if root == nil {
				return nil, errors.New("not visible")
			}
			return root, nil
		}
	}
	t.Cleanup(func() { listDevices, openDevice = oldList, oldOpen })
}

func galaxyFiles() *fakeFolder {
	camera := &fakeFolder{items: []fakeItem{
		{Item: portable.Item{Name: "IMG_20260101_120000.jpg", Size: 2_400_000, ModTime: 1767268800}},
		{Item: portable.Item{Name: "VID_20260102_090000.mp4", Size: 50_000_000, ModTime: 1767344400}},
	}}
	dcim := &fakeFolder{items: []fakeItem{{Item: portable.Item{Name: "Camera", IsDir: true}, sub: camera}}}
	download := &fakeFolder{items: []fakeItem{{Item: portable.Item{Name: "boarding_pass.pdf", Size: 90_000, ModTime: 1767000000}}}}
	storage := &fakeFolder{items: []fakeItem{
		{Item: portable.Item{Name: "DCIM", IsDir: true}, sub: dcim},
		{Item: portable.Item{Name: "Download", IsDir: true}, sub: download},
	}}
	return &fakeFolder{items: []fakeItem{{Item: portable.Item{Name: "Internal storage", IsDir: true}, sub: storage}}}
}

func TestIndexAndSearchAPortableDevice(t *testing.T) {
	withFixture(t)
	plug(t) // no drives: only the device (and the same on every OS, for CI)
	connectDevice(t, galaxyFiles(), testGalaxy)

	out, err := run(t, "--no-color", "index", "device")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Indexed Galaxy S21 — 7 entries") {
		t.Errorf("index output:\n%s", out)
	}

	out, err = run(t, "--no-color", "search", "boarding")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `device:Galaxy S21\Internal storage\Download\boarding_pass.pdf`) || strings.Contains(out, "offline") {
		t.Errorf("search output:\n%s", out)
	}
	out, _ = run(t, "--no-color", "search", "drive:device", "ext:jpg,mp4", "--paths-only")
	if strings.Count(out, "device:Galaxy S21") != 2 {
		t.Errorf("drive:device should find the two camera files:\n%s", out)
	}
	out, _ = run(t, "--no-color", "search", "drive:phone", "IMG")
	if !strings.Contains(out, "IMG_20260101_120000.jpg") {
		t.Errorf("drive:phone should include phones read as portable devices:\n%s", out)
	}

	out, _ = run(t, "--no-color", "drives")
	if !strings.Contains(out, "device:"+testGalaxy.SourceID()) || !strings.Contains(out, "Galaxy S21") || !strings.Contains(out, "yes (just now") {
		t.Errorf("drives should list the connected device as indexed:\n%s", out)
	}
	out, _ = run(t, "--no-color", "list")
	if !strings.Contains(out, "device") || !strings.Contains(out, "online") || !strings.Contains(out, "MTP") {
		t.Errorf("list:\n%s", out)
	}

	// Copying search results: the device's files are pointed at Explorer, not silently dropped.
	dst := filepath.Join(t.TempDir(), "out")
	out, _ = run(t, "--no-color", "search", "boarding", "--copy-to", dst)
	if !strings.Contains(out, "1 matching file is on Galaxy S21 and was skipped: copy it in Explorer (This PC → Galaxy S21)") {
		t.Errorf("copy-to output:\n%s", out)
	}

	// Unplugged: still searchable, marked offline.
	connectDevice(t, nil)
	out, _ = run(t, "--no-color", "search", "boarding")
	if !strings.Contains(out, "boarding_pass.pdf") || !strings.Contains(out, "offline") {
		t.Errorf("an unplugged device's files must still be found, marked offline:\n%s", out)
	}
}

// fullText is a user error's message and hint together.
func fullText(err error) string {
	var ue *UserError
	if errors.As(err, &ue) {
		return ue.Msg + "\n" + ue.Hint
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestIndexDeviceExplainsProblems(t *testing.T) {
	withFixture(t)
	connectDevice(t, galaxyFiles())
	if _, err := run(t, "index", "device"); err == nil || !strings.Contains(fullText(err), "File transfer") {
		t.Errorf("nothing connected: %v", err)
	}

	camera := portable.Device{InstanceID: `USB\VID_04A9&PID_32D9\6&1`, Name: "Canon EOS R50"}
	connectDevice(t, galaxyFiles(), testGalaxy, camera)
	_, err := run(t, "index", "device")
	if err == nil || !strings.Contains(fullText(err), "device:"+camera.SourceID()) || !strings.Contains(fullText(err), "Canon EOS R50") {
		t.Errorf("two connected: the choices must be listed: %v", err)
	}
	if out, err := run(t, "--no-color", "index", "device:canon eos r50"); err != nil || !strings.Contains(out, "Indexed Canon EOS R50") {
		t.Errorf("choosing by name: %v\n%s", err, out)
	}

	// A locked phone shows no storage.
	connectDevice(t, &fakeFolder{}, testGalaxy)
	if _, err := run(t, "index", "device"); err == nil || !strings.Contains(fullText(err), "unlock it") {
		t.Errorf("locked phone: %v", err)
	}
}
