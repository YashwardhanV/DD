package portable

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/index"
)

const samsung = `\\?\usb#vid_04e8&pid_6860&ms_comp_mtp&samsung_android#6&2b5c4a3c&0&0000#{6ac27878-a6fa-4155-ba85-f98f491d4f33}`

func TestInstanceIDFromInterface(t *testing.T) {
	cases := map[string]string{
		samsung: `USB\VID_04E8&PID_6860&MS_COMP_MTP&SAMSUNG_ANDROID\6&2B5C4A3C&0&0000`,
		`\\?\usb#vid_18d1&pid_4ee1#R58M12345#{6ac27878-a6fa-4155-ba85-f98f491d4f33}`: `USB\VID_18D1&PID_4EE1\R58M12345`,
	}
	for in, want := range cases {
		if got, ok := InstanceIDFromInterface(in); !ok || got != want {
			t.Errorf("InstanceIDFromInterface(%s) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", `usb#a#b#{x}`, `\\?\usb#vid#{guid}`, `\\?\usb#a#b#nobrace`, `\\?\#a#b#{g}`, `\\?\usb#a##{g}`} {
		if got, ok := InstanceIDFromInterface(bad); ok {
			t.Errorf("InstanceIDFromInterface(%q) = %q, should fail", bad, got)
		}
	}
}

func TestSerialFromInstanceID(t *testing.T) {
	for in, want := range map[string]string{
		`USB\VID_18D1&PID_4EE1\R58M12345`:                                     "R58M12345",
		`USB\VID_04E8&PID_6860&MS_COMP_MTP&SAMSUNG_ANDROID\6&2B5C4A3C&0&0000`: "",
		`USB\VID_05AC&PID_12A8\`:                                              "",
		`nobackslash`:                                                         "",
	} {
		if got := SerialFromInstanceID(in); got != want {
			t.Errorf("SerialFromInstanceID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOnlyRealUSBDevicesCount(t *testing.T) {
	if !IsUSBInterface(samsung) || !IsUSBInterface(`\\?\USB#VID_05AC&PID_12A8&MI_00#7&1#{6ac27878-a6fa-4155-ba85-f98f491d4f33}`) {
		t.Error("USB phones and iPhones are portable devices")
	}
	// A pen drive's WPD twin (it already has a drive letter) and Bluetooth devices are not.
	for _, p := range []string{
		`\\?\swd#wpdbusenum#_??_usbstor#disk&ven_sandisk&prod_ultra&rev_1.00#4c530001#{53f56307-b6bf-11d0-94f2-00a0c91efb8b}#{6ac27878-a6fa-4155-ba85-f98f491d4f33}`,
		`\\?\bthenum#{0000110a-0000-1000-8000-00805f9b34fb}#1&2#{6ac27878-a6fa-4155-ba85-f98f491d4f33}`,
	} {
		if IsUSBInterface(p) {
			t.Errorf("%s must be skipped", p)
		}
	}
}

func TestSourceIDIsStableAndFollowsTheSerial(t *testing.T) {
	a := Device{InstanceID: `USB\VID_04E8&PID_6860&MS_COMP_MTP\6&AAA&0&0000`, Serial: "R58M12345"}
	b := Device{InstanceID: `USB\VID_04E8&PID_6860&MS_COMP_MTP\6&BBB&0&0000`, Serial: "r58m12345"} // other port
	if a.SourceID() != b.SourceID() {
		t.Error("the same phone on another port must keep its index")
	}
	if !strings.HasPrefix(a.SourceID(), "dev-") || len(a.SourceID()) != 16 {
		t.Errorf("SourceID = %q", a.SourceID())
	}
	noSerial1 := Device{InstanceID: `USB\VID_1\6&AAA`}
	noSerial2 := Device{InstanceID: `USB\VID_1\6&BBB`}
	if noSerial1.SourceID() == noSerial2.SourceID() || noSerial1.SourceID() == a.SourceID() {
		t.Error("devices without a serial are told apart by instance ID")
	}
	if a.Arg() != "device:"+a.SourceID() {
		t.Errorf("Arg = %q", a.Arg())
	}
	if (Device{}).Label() != "Portable device" || (Device{Name: " Pixel 8 "}).Label() != "Pixel 8" {
		t.Error("labels")
	}
}

func TestParseTarget(t *testing.T) {
	for in, want := range map[string]string{"device": "", "DEVICE": "", " device:Pixel 8 ": "Pixel 8", "device:dev-0123": "dev-0123"} {
		if got, ok := ParseTarget(in); !ok || got != want {
			t.Errorf("ParseTarget(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"phone", "device:", "devices", `E:\device`, "dev"} {
		if _, ok := ParseTarget(in); ok {
			t.Errorf("ParseTarget(%q) should not match", in)
		}
	}
}

func TestPickDevice(t *testing.T) {
	pixel := Device{InstanceID: `USB\A\1`, Serial: "1A2B", Name: "Pixel 8"}
	cam := Device{InstanceID: `USB\B\6&X`, Name: "Canon EOS"}
	if _, err := PickDevice(nil, ""); !errors.Is(err, ErrNoDevice) {
		t.Errorf("none: %v", err)
	}
	if d, err := PickDevice([]Device{pixel}, ""); err != nil || d != pixel {
		t.Errorf("only one: %v %v", d, err)
	}
	var multi *MultipleError
	if _, err := PickDevice([]Device{pixel, cam}, ""); !errors.As(err, &multi) || len(multi.Devices) != 2 {
		t.Errorf("two: %v", err)
	}
	for _, which := range []string{pixel.SourceID(), "1a2b", "pixel 8"} {
		if d, err := PickDevice([]Device{cam, pixel}, which); err != nil || d != pixel {
			t.Errorf("PickDevice(%q) = %v, %v", which, d, err)
		}
	}
	var nf *NotFoundError
	if _, err := PickDevice([]Device{cam, pixel}, "iPhone"); !errors.As(err, &nf) {
		t.Errorf("unknown name: %v", err)
	}
	twin := Device{InstanceID: `USB\C\9`, Serial: "9", Name: "Pixel 8"}
	if _, err := PickDevice([]Device{pixel, twin}, "Pixel 8"); !errors.As(err, &multi) {
		t.Errorf("two with the same name must ask: %v", err)
	}
}

// ---- scanner, with a fake device ----------------------------------------------------

type fakeFolder struct {
	name    string
	entries []fakeEntry
	err     error
	opened  *int // open folders right now, shared by the whole tree
}

type fakeEntry struct {
	Item
	sub *fakeFolder
}

func (f *fakeFolder) List(ctx context.Context) ([]Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []Entry
	for _, e := range f.entries {
		en := Entry{Item: e.Item}
		if e.sub != nil {
			*f.opened++
			e.sub.opened = f.opened
			en.Sub = e.sub
		}
		out = append(out, en)
	}
	return out, nil
}

func (f *fakeFolder) Close() { *f.opened-- }

func file(name string, size, mtime int64) fakeEntry {
	return fakeEntry{Item: Item{Name: name, Size: size, ModTime: mtime}}
}

func dir(name string, entries ...fakeEntry) fakeEntry {
	return fakeEntry{Item: Item{Name: name, IsDir: true}, sub: &fakeFolder{name: name, entries: entries}}
}

func phoneTree() *fakeFolder {
	return &fakeFolder{entries: []fakeEntry{
		dir("Internal storage",
			dir("DCIM", dir("Camera", file("IMG_0001.jpg", 2_400_000, 1700000000), file("VID_0002.mp4", 3_000_000_000, 1700000100))),
			dir("Download", file("invoice.pdf", 81_000, 1690000000), file("mystery.bin", -1, 0)),
			dir("Empty"),
		),
		dir("SD card", dir("Music", file("song.mp3", 5_000_000, 1600000000))),
	}}
}

func scanFake(t *testing.T, root *fakeFolder) (*index.Index, *Scanner, error) {
	t.Helper()
	opened := 1
	root.opened = &opened
	s := &Scanner{Open: func(context.Context) (Folder, error) { return root, nil }}
	b := index.NewBuilder(index.Header{SourceKind: index.KindDevice, SourceID: "dev-test"})
	err := s.Scan(context.Background(), b, nil)
	if opened != 0 {
		t.Errorf("%d folders were left open", opened)
	}
	return b.Index(), s, err
}

func paths(ix *index.Index) []string {
	var out []string
	cache := map[uint64]string{}
	for slot := 0; slot < ix.Len(); slot++ {
		p := ix.RelPathCached(slot, cache)
		if ix.Flags[slot]&index.FlagDir != 0 {
			p += `\`
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func TestScannerIndexesEveryStorageAndFolder(t *testing.T) {
	ix, s, err := scanFake(t, phoneTree())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`Internal storage\`, `Internal storage\DCIM\`, `Internal storage\DCIM\Camera\`,
		`Internal storage\DCIM\Camera\IMG_0001.jpg`, `Internal storage\DCIM\Camera\VID_0002.mp4`,
		`Internal storage\Download\`, `Internal storage\Download\invoice.pdf`, `Internal storage\Download\mystery.bin`,
		`Internal storage\Empty\`, `SD card\`, `SD card\Music\`, `SD card\Music\song.mp3`,
	}
	if got := paths(ix); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("paths =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	st := s.Stats()
	if st.Files != 5 || st.Dirs != 7 || st.Errors != 0 {
		t.Errorf("stats = %+v", st)
	}
	for slot := 0; slot < ix.Len(); slot++ {
		switch ix.Names[slot] {
		case "VID_0002.mp4":
			if ix.Sizes[slot] != 3_000_000_000 || ix.MTimes[slot] != 1700000100 || ix.Flags[slot] != 0 {
				t.Errorf("a 3 GB video must keep its size and date: %d %d %d", ix.Sizes[slot], ix.MTimes[slot], ix.Flags[slot])
			}
		case "mystery.bin":
			if ix.Flags[slot]&index.FlagNoMeta == 0 || ix.Sizes[slot] != -1 {
				t.Error("an unknown size is marked as such")
			}
		}
	}
}

func TestALockedPhoneShowsNoStorageAndIsAnError(t *testing.T) {
	_, _, err := scanFake(t, &fakeFolder{})
	if !errors.Is(err, ErrNoStorage) {
		t.Errorf("err = %v, want ErrNoStorage", err)
	}
	_, _, err = scanFake(t, &fakeFolder{entries: []fakeEntry{file("stray.txt", 1, 0)}})
	if !errors.Is(err, ErrNoStorage) {
		t.Errorf("files but no storage folder: %v", err)
	}
}

func TestUnreadableFoldersAreCountedNotFatal(t *testing.T) {
	root := phoneTree()
	root.entries[0].sub.entries[1].sub.err = errors.New("the device stopped responding") // Download
	ix, s, err := scanFake(t, root)
	if err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Errors != 1 || len(st.FirstErrors) != 1 || !strings.Contains(st.FirstErrors[0], `Internal storage\Download: the device stopped responding`) {
		t.Errorf("stats = %+v", st)
	}
	if ix.Len() != 10 {
		t.Errorf("everything else must still be indexed: %d entries", ix.Len())
	}
}

func TestCancellingReleasesEveryFolder(t *testing.T) {
	root := phoneTree()
	opened := 1
	root.opened = &opened
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled after the storages are listed: nothing else is read
	s := &Scanner{Open: func(context.Context) (Folder, error) { return root, nil }}
	if err := s.Scan(ctx, index.NewBuilder(index.Header{}), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if opened != 0 {
		t.Errorf("%d folders left open after cancelling", opened)
	}
}

func TestOpenFailureIsReturned(t *testing.T) {
	boom := errors.New("not visible")
	s := &Scanner{Open: func(context.Context) (Folder, error) { return nil, boom }}
	if err := s.Scan(context.Background(), index.NewBuilder(index.Header{}), nil); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if s.Name() != "shell" {
		t.Errorf("Name = %q", s.Name())
	}
}
