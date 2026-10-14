package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yashwardhanv/dd/internal/volume"
)

// A USB 3 pen drive stuck in a USB 2.0 port, on a PC with two USB 3 ports.
var (
	slowLink = volume.USBLink{Speed: volume.SpeedHigh, Capable: volume.SpeedSuper, PortKnown: true, Port: 3}
	fastLink = volume.USBLink{Speed: volume.SpeedSuper, Capable: volume.SpeedSuper, PortKnown: true, PortUSB3: true, Port: 5}
	pcPorts  = []volume.PortInfo{
		{Hub: "h", Number: 1, Root: true, UserConnectable: true, USB2: true, CompanionPort: 5},
		{Hub: "h", Number: 5, Root: true, UserConnectable: true, USB3: true, CompanionPort: 1},
		{Hub: "h", Number: 2, Root: true, UserConnectable: true, TypeC: true, USB2: true, CompanionPort: 6},
		{Hub: "h", Number: 6, Root: true, UserConnectable: true, TypeC: true, USB3: true, CompanionPort: 2, Connected: true, Speed: volume.SpeedSuper},
		{Hub: "h", Number: 3, Root: true, UserConnectable: true, USB2: true},
	}
)

// fakeUSB makes every drive in links a USB drive with that link.
func fakeUSB(t *testing.T, links map[string]volume.USBLink, ports []volume.PortInfo) {
	t.Helper()
	oldBus, oldLink, oldPorts := busTypeFor, usbLinkFor, usbPortsFor
	var mu sync.Mutex
	busTypeFor = func(root string) (volume.BusInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := links[driveOf(root)]; ok {
			return volume.BusInfo{Bus: volume.BusTypeUsb}, nil
		}
		return volume.BusInfo{Bus: volume.BusTypeSata}, nil
	}
	usbLinkFor = func(root string) (volume.USBLink, error) {
		mu.Lock()
		defer mu.Unlock()
		if l, ok := links[driveOf(root)]; ok {
			return l, nil
		}
		return volume.USBLink{}, volume.ErrNotUSB
	}
	usbPortsFor = func() ([]volume.PortInfo, error) { return ports, nil }
	t.Cleanup(func() { busTypeFor, usbLinkFor, usbPortsFor = oldBus, oldLink, oldPorts })
}

func TestDriveOf(t *testing.T) {
	for in, want := range map[string]string{`E:\Photos`: "E:", `e:`: "E:", `C:\`: "C:", `/tmp/x`: "", `\\server\share`: "", `1:\x`: "", ``: ""} {
		if got := driveOf(in); got != want {
			t.Errorf("driveOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPortsCommandSummarizesThePCAndEachDrive(t *testing.T) {
	plug(t, fakeDrive("E:"), fakeDrive("F:"))
	fakeUSB(t, map[string]volume.USBLink{"E:": slowLink, "F:": fastLink}, pcPorts)

	out, err := run(t, "--no-color", "ports")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"This PC has 2 USB 3 ports (1 USB-C).",
		"1 USB 2.0-only port",
		"⚠ E: SANDISK 32GB: can run at USB 3 · 5 Gbps but is running at USB 2.0 · 480 Mbps",
		"This port is USB 2.0 only.",
		"ports --watch",
		"✓ F: SANDISK 32GB: running at USB 3 · 5 Gbps - as fast as this drive can",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}

	out, _ = run(t, "--no-color", "ports", "--all")
	for _, want := range []string{"HUB", "USB-C (with port 2)", "USB 3 · 5 Gbps", "USB 2.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all missing %q:\n%s", want, out)
		}
	}
}

func TestPortsCommandWithoutUSBDrivesOrUSB3(t *testing.T) {
	plug(t)
	fakeUSB(t, nil, []volume.PortInfo{{Hub: "h", Number: 1, Root: true, UserConnectable: true, USB2: true}})
	out, err := run(t, "--no-color", "ports")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no USB 3 ports") || !strings.Contains(out, "No USB drive is plugged in") {
		t.Errorf("got:\n%s", out)
	}

	old := usbPortsFor
	usbPortsFor = func() ([]volume.PortInfo, error) { return nil, errors.ErrUnsupported }
	defer func() { usbPortsFor = old }()
	if _, err := run(t, "ports"); err == nil || !strings.Contains(err.Error(), "only works on Windows") {
		t.Errorf("off Windows: %v", err)
	}
}

func TestPortsWatchReportsEachPlugIn(t *testing.T) {
	old := watchInterval
	watchInterval = 5 * time.Millisecond
	defer func() { watchInterval = old }()

	var mu sync.Mutex
	vols := []volume.Volume{}
	oldList := listVolumes
	listVolumes = func() ([]volume.Volume, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]volume.Volume(nil), vols...), nil
	}
	defer func() { listVolumes = oldList }()
	links := map[string]volume.USBLink{"E:": slowLink}
	fakeUSB(t, links, pcPorts)

	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- runPorts(ctx, &out, true, false) }()

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(out.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("never saw %q:\n%s", want, out.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("Watching.")
	mu.Lock()
	vols = []volume.Volume{fakeDrive("E:")}
	mu.Unlock()
	waitFor("480 Mbps")
	mu.Lock()
	vols = nil
	mu.Unlock()
	waitFor("E: unplugged")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestDrivesFlagsASlowUSBLink(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	plug(t, fakeDrive("E:"))
	fakeUSB(t, map[string]volume.USBLink{"E:": slowLink}, pcPorts)
	out, err := run(t, "--no-color", "drives")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"USB 2.0 · 480 Mbps · external ⚠", "can run at USB 3 · 5 Gbps", "this PC has 2 USB 3 ports"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}

	fakeUSB(t, map[string]volume.USBLink{"E:": fastLink}, pcPorts)
	out, _ = run(t, "--no-color", "drives")
	if !strings.Contains(out, "USB 3 · 5 Gbps · external") || strings.Contains(out, "⚠") {
		t.Errorf("a fast link is shown without a warning:\n%s", out)
	}
}

func TestDoctorReportsUSBLinkAndPorts(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	plug(t, fakeDrive("E:"))
	fakeUSB(t, map[string]volume.USBLink{"E:": slowLink}, pcPorts)
	out, _ := run(t, "--no-color", "doctor")
	for _, want := range []string{"USB ports", "this PC has 2 USB 3 ports (1 USB-C) and 1 USB 2.0-only", "This port is USB 2.0 only."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestCopyWarnsAboutASlowUSBLinkWithATimeFloor(t *testing.T) {
	fakeUSB(t, map[string]volume.USBLink{"E:": slowLink, "F:": fastLink}, pcPorts)

	var out bytes.Buffer
	anyUSB := warnSlowUSB(&out, []string{`E:\Photos`, `E:\Backup`, `D:\x`}, 12<<30)
	text := out.String()
	if !anyUSB {
		t.Error("E: is USB")
	}
	if strings.Count(text, "⚠") != 1 {
		t.Errorf("one warning per drive, got:\n%s", text)
	}
	// 12 GiB at ~40 MB/s is a little over 5 minutes.
	if !strings.Contains(text, "at least 5m") || !strings.Contains(text, "USB 2.0 only") {
		t.Errorf("got:\n%s", text)
	}

	out.Reset()
	if anyUSB := warnSlowUSB(&out, []string{`F:\`, ""}, 12<<30); !anyUSB || out.Len() != 0 {
		t.Errorf("a fast USB drive: anyUSB=%v, output %q", anyUSB, out.String())
	}
	out.Reset()
	if anyUSB := warnSlowUSB(&out, []string{`D:\`, "/tmp/x"}, 1<<30); anyUSB || out.Len() != 0 {
		t.Errorf("no USB drive: anyUSB=%v, output %q", anyUSB, out.String())
	}
	// Small copies don't get a time estimate.
	out.Reset()
	warnSlowUSB(&out, []string{`E:\`}, 1<<20)
	if strings.Contains(out.String(), "at least") {
		t.Errorf("tiny copy got a time floor:\n%s", out.String())
	}
}
