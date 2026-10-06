package phone

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const sampleDevices = `* daemon not running; starting now at tcp:5037
* daemon started successfully
List of devices attached
R58M1234ABC            device usb:1-1 product:panther model:Pixel_7 device:panther transport_id:1
emulator-5554          offline transport_id:3
ABCDEF0123             unauthorized usb:1-2 transport_id:2
0123456789ABCDEF       no permissions; see [http://developer.android.com/tools/device.html]
192.168.1.5:5555       device product:x model:Galaxy_S9_Plus device:x transport_id:5

`

func TestParseDevices(t *testing.T) {
	got := ParseDevices(sampleDevices)
	want := []Device{
		{Serial: "R58M1234ABC", State: "device", Model: "Pixel 7"},
		{Serial: "emulator-5554", State: "offline"},
		{Serial: "ABCDEF0123", State: "unauthorized"},
		{Serial: "0123456789ABCDEF", State: "no permissions"},
		{Serial: "192.168.1.5:5555", State: "device", Model: "Galaxy S9 Plus"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseDevicesEmptyAndNoise(t *testing.T) {
	for _, in := range []string{"", "List of devices attached\n\n", "* daemon started successfully\nList of devices attached\n", "garbage with no header\nabc device\n"} {
		if got := ParseDevices(in); len(got) != 0 {
			t.Errorf("ParseDevices(%q) = %+v, want none", in, got)
		}
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in     string
		serial string
		ok     bool
	}{
		{"phone", "", true}, {"PHONE", "", true}, {" phone ", "", true},
		{"phone:R58M1234ABC", "R58M1234ABC", true}, {"Phone:abc", "abc", true}, {"phone:192.168.1.5:5555", "192.168.1.5:5555", true},
		{"phones", "", false}, {`E:\`, "", false}, {"mobile", "", false}, {"phone:", "", false},
	}
	for _, c := range cases {
		serial, ok := ParseTarget(c.in)
		if serial != c.serial || ok != c.ok {
			t.Errorf("ParseTarget(%q) = %q,%v want %q,%v", c.in, serial, ok, c.serial, c.ok)
		}
	}
}

func TestPickDevice(t *testing.T) {
	pixel := Device{Serial: "AAA", State: "device", Model: "Pixel 7"}
	galaxy := Device{Serial: "BBB", State: "device", Model: "Galaxy S9"}
	locked := Device{Serial: "CCC", State: "unauthorized"}

	if d, err := PickDevice([]Device{pixel}, ""); err != nil || d != pixel {
		t.Errorf("single device: %v %v", d, err)
	}
	if d, err := PickDevice([]Device{locked, pixel}, ""); err != nil || d != pixel {
		t.Errorf("an unauthorized phone must not block the usable one: %v %v", d, err)
	}
	if d, err := PickDevice([]Device{pixel, galaxy}, "bbb"); err != nil || d != galaxy {
		t.Errorf("by serial (case-insensitive): %v %v", d, err)
	}

	var multi *MultipleDevicesError
	if _, err := PickDevice([]Device{pixel, galaxy}, ""); !errors.As(err, &multi) || len(multi.Devices) != 2 ||
		!strings.Contains(err.Error(), "Pixel 7 (AAA)") || !strings.Contains(err.Error(), "Galaxy S9 (BBB)") {
		t.Errorf("two phones: %v", err)
	}
	var none *NoDeviceError
	if _, err := PickDevice(nil, ""); !errors.As(err, &none) {
		t.Errorf("no phones: %v", err)
	}
	if _, err := PickDevice([]Device{pixel}, "ZZZ"); !errors.As(err, &none) || none.Wanted != "ZZZ" {
		t.Errorf("unknown serial: %v", err)
	}
	var notUsable *NotUsableError
	if _, err := PickDevice([]Device{locked}, ""); !errors.As(err, &notUsable) || notUsable.Device.Hint() == "" {
		t.Errorf("only an unauthorized phone: %v", err)
	}
	if _, err := PickDevice([]Device{locked}, "CCC"); !errors.As(err, &notUsable) {
		t.Errorf("unauthorized by serial: %v", err)
	}
}

func TestDeviceHints(t *testing.T) {
	if h := (Device{State: "unauthorized"}).Hint(); !strings.Contains(h, "Allow") {
		t.Errorf("unauthorized hint = %q", h)
	}
	for _, st := range []string{"offline", "no permissions"} {
		if (Device{State: st}).Hint() == "" {
			t.Errorf("%s needs a hint", st)
		}
	}
	if (Device{State: "device"}).Hint() != "" {
		t.Error("a usable phone needs no hint")
	}
	if (Device{Serial: "S", Model: "M"}).Name() != "M" || (Device{Serial: "S"}).Name() != "S" {
		t.Error("Name falls back to the serial")
	}
	if SourceID("R58") != "adb-R58" {
		t.Error("SourceID")
	}
}

func TestVersionReturnsFirstLine(t *testing.T) {
	var got []string
	adb := NewADBWithRunner(runnerFunc(func(ctx context.Context, args ...string) ([]byte, error) {
		got = args
		return []byte("Android Debug Bridge version 1.0.41\r\nVersion 35.0.2-12147458\r\nInstalled as C:\\adb.exe\r\n"), nil
	}))
	v, err := adb.Version(context.Background())
	if err != nil || v != "Android Debug Bridge version 1.0.41" {
		t.Errorf("v=%q err=%v", v, err)
	}
	if len(got) != 1 || got[0] != "version" {
		t.Errorf("args = %v", got)
	}
}

func TestVersionError(t *testing.T) {
	adb := NewADBWithRunner(runnerFunc(func(context.Context, ...string) ([]byte, error) { return nil, errors.New("not running") }))
	if _, err := adb.Version(context.Background()); err == nil {
		t.Error("an adb failure must be reported")
	}
}
