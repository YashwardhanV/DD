package volume

import (
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

// exBuf builds a USB_NODE_CONNECTION_INFORMATION_EX reply (packed, as in usbioctl.h).
func exBuf(speed byte, connected bool, bcdUSB uint16) []byte {
	b := make([]byte, 35)
	le := binary.LittleEndian
	le.PutUint32(b[0:], 3)       // ConnectionIndex
	b[4], b[5] = 18, 1           // bLength, bDescriptorType
	le.PutUint16(b[6:], bcdUSB)  // bcdUSB
	le.PutUint16(b[12:], 0x0781) // idVendor (SanDisk)
	le.PutUint16(b[14:], 0x5581) // idProduct
	b[23] = speed                // Speed
	le.PutUint16(b[25:], 7)      // DeviceAddress
	le.PutUint32(b[27:], 2)      // NumberOfOpenPipes
	if connected {
		le.PutUint32(b[31:], 1) // DeviceConnected
	}
	return b
}

func v2Buf(protocols, flags uint32) []byte {
	b := ConnectionInfoV2Request(3)
	binary.LittleEndian.PutUint32(b[8:], protocols)
	binary.LittleEndian.PutUint32(b[12:], flags)
	return b
}

func TestParseConnectionInfoEx(t *testing.T) {
	c, err := ParseConnectionInfoEx(exBuf(2, true, 0x0210))
	if err != nil {
		t.Fatal(err)
	}
	if c.Speed != SpeedHigh || !c.Connected || c.IsHub || c.VendorID != 0x0781 || c.ProductID != 0x5581 || c.BcdUSB != 0x0210 {
		t.Errorf("got %+v", c)
	}
	if c, _ := ParseConnectionInfoEx(exBuf(3, false, 0x0320)); c.Speed != SpeedSuper || c.Connected {
		t.Errorf("superspeed, not connected: got %+v", c)
	}
	if _, err := ParseConnectionInfoEx(make([]byte, 34)); err == nil {
		t.Error("a short buffer must be an error")
	}
}

func TestParseConnectionInfoV2(t *testing.T) {
	v, err := ParseConnectionInfoV2(v2Buf(0b011, 0b0010)) // USB 1.1+2.0 port, SuperSpeed-capable device not running at it
	if err != nil {
		t.Fatal(err)
	}
	want := ConnectionInfoV2{PortUSB1: true, PortUSB2: true, CapableSuper: true}
	if v != want {
		t.Errorf("got %+v, want %+v", v, want)
	}
	if _, err := ParseConnectionInfoV2(make([]byte, 15)); err == nil {
		t.Error("a short buffer must be an error")
	}
	req := ConnectionInfoV2Request(5)
	if binary.LittleEndian.Uint32(req[0:]) != 5 || binary.LittleEndian.Uint32(req[4:]) != 16 || binary.LittleEndian.Uint32(req[8:]) != 7 {
		t.Errorf("bad request %v", req)
	}
}

func TestSpeeds(t *testing.T) {
	cases := []struct {
		name         string
		ex           ConnectionInfoEx
		v2           *ConnectionInfoV2
		now, capable USBSpeed
	}{
		{"USB 3 drive in a USB 2.0 port", ConnectionInfoEx{Speed: SpeedHigh}, &ConnectionInfoV2{PortUSB2: true, CapableSuper: true}, SpeedHigh, SpeedSuper},
		{"USB 3 drive in a USB 3 port", ConnectionInfoEx{Speed: SpeedSuper}, &ConnectionInfoV2{PortUSB3: true, OperatingSuper: true, CapableSuper: true}, SpeedSuper, SpeedSuper},
		{"old answer says high speed, flags say SuperSpeed", ConnectionInfoEx{Speed: SpeedHigh}, &ConnectionInfoV2{OperatingSuper: true, CapableSuper: true}, SpeedSuper, SpeedSuper},
		{"10 Gbps drive running at 5", ConnectionInfoEx{Speed: SpeedSuper}, &ConnectionInfoV2{OperatingSuper: true, CapableSuper: true, CapableSuperPlus: true}, SpeedSuper, SpeedSuperPlus},
		{"10 Gbps drive at full speed", ConnectionInfoEx{Speed: SpeedSuper}, &ConnectionInfoV2{OperatingSuper: true, OperatingSuperPlus: true, CapableSuper: true, CapableSuperPlus: true}, SpeedSuperPlus, SpeedSuperPlus},
		{"plain USB 2.0 drive", ConnectionInfoEx{Speed: SpeedHigh, BcdUSB: 0x0200}, &ConnectionInfoV2{PortUSB2: true, PortUSB3: true}, SpeedHigh, SpeedHigh},
		{"USB 2.0 drive stuck at 12 Mbps", ConnectionInfoEx{Speed: SpeedFull, BcdUSB: 0x0200}, &ConnectionInfoV2{PortUSB2: true}, SpeedFull, SpeedHigh},
		{"no V2 answer", ConnectionInfoEx{Speed: SpeedHigh}, nil, SpeedHigh, SpeedUnknown},
	}
	for _, c := range cases {
		now, capable := Speeds(c.ex, c.v2)
		if now != c.now || capable != c.capable {
			t.Errorf("%s: got %v / %v, want %v / %v", c.name, now, capable, c.now, c.capable)
		}
	}
}

func TestJudge(t *testing.T) {
	cases := []struct {
		name string
		l    USBLink
		want Verdict
	}{
		{"nothing known", USBLink{}, VerdictUnknown},
		{"full speed", USBLink{Speed: SpeedSuper, Capable: SpeedSuper, PortKnown: true, PortUSB3: true}, VerdictFull},
		{"USB 2.0 drive in USB 2.0 port", USBLink{Speed: SpeedHigh, Capable: SpeedHigh, PortKnown: true}, VerdictFull},
		{"USB 3 drive, USB 2.0-only port", USBLink{Speed: SpeedHigh, Capable: SpeedSuper, PortKnown: true}, VerdictSlowPort},
		{"USB 3 drive on the USB 2.0 half of a USB 3 socket", USBLink{Speed: SpeedHigh, Capable: SpeedSuper, PortKnown: true, PortTwinUSB3: true}, VerdictSlowCable},
		{"USB 3 drive through a USB 2.0 hub", USBLink{Speed: SpeedHigh, Capable: SpeedSuper, PortKnown: true, ViaHub: true}, VerdictSlowHub},
		{"10 Gbps drive on a 5 Gbps port", USBLink{Speed: SpeedSuper, Capable: SpeedSuperPlus, PortKnown: true, PortUSB3: true}, VerdictSlowCable},
		{"unknown capability at USB 2.0", USBLink{Speed: SpeedHigh}, VerdictSlowMaybe},
		{"unknown capability at USB 3", USBLink{Speed: SpeedSuper}, VerdictFull},
		{"port unknown", USBLink{Speed: SpeedHigh, Capable: SpeedSuper}, VerdictSlowMaybe},
	}
	for _, c := range cases {
		if got := c.l.Judge(); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if !VerdictSlowPort.Slow() || VerdictSlowMaybe.Slow() || VerdictFull.Slow() {
		t.Error("Slow() is wrong")
	}
}

func TestAdvice(t *testing.T) {
	slow := USBLink{Speed: SpeedHigh, Capable: SpeedSuper, PortKnown: true}

	text, tip := Advice(slow, PortSummary{Known: true, USB3: 2, USB3TypeC: 1})
	for _, want := range []string{"5 Gbps", "480 Mbps", "40 MB/s", "450", "USB 2.0 only"} {
		if !strings.Contains(text, want) {
			t.Errorf("text %q lacks %q", text, want)
		}
	}
	for _, want := range []string{"2 USB 3 ports", "1 of them USB-C", "ports --watch"} {
		if !strings.Contains(tip, want) {
			t.Errorf("tip %q lacks %q", tip, want)
		}
	}

	_, tip = Advice(slow, PortSummary{Known: true, USB3: 0})
	if !strings.Contains(tip, "no USB 3 ports") {
		t.Errorf("no-USB-3 PC tip: %q", tip)
	}
	_, tip = Advice(slow, PortSummary{Known: true, USB3: 1, USB3TypeC: 1})
	if !strings.Contains(tip, "all USB-C") {
		t.Errorf("USB-C-only tip: %q", tip)
	}
	_, tip = Advice(slow, PortSummary{})
	if !strings.Contains(tip, "USB 3 port") {
		t.Errorf("unknown-ports tip: %q", tip)
	}

	if text, tip := Advice(USBLink{Speed: SpeedSuper, Capable: SpeedSuper}, PortSummary{}); tip != "" || !strings.Contains(text, "as fast as") {
		t.Errorf("full speed: %q / %q", text, tip)
	}
	if _, tip := Advice(USBLink{Speed: SpeedHigh, Capable: SpeedSuper, PortKnown: true, PortTwinUSB3: true}, PortSummary{}); !strings.Contains(tip, "push the plug") {
		t.Errorf("cable tip: %q", tip)
	}
	if text, _ := Advice(USBLink{Speed: SpeedHigh, Capable: SpeedSuper, ViaHub: true}, PortSummary{}); !strings.Contains(text, "hub") {
		t.Errorf("hub text: %q", text)
	}
}

func TestSummarize(t *testing.T) {
	const hub = `\\?\usb#root_hub30#4&1`
	ports := []PortInfo{
		// Socket 1: USB-A, USB 3 (ports 1 and 5 are its two halves)
		{Hub: hub, Number: 1, Root: true, UserConnectable: true, USB2: true, CompanionPort: 5},
		{Hub: hub, Number: 5, Root: true, UserConnectable: true, USB3: true, CompanionPort: 1},
		// Socket 2: USB-C, USB 3 (ports 2 and 6)
		{Hub: hub, Number: 2, Root: true, UserConnectable: true, TypeC: true, USB2: true, CompanionPort: 6},
		{Hub: hub, Number: 6, Root: true, UserConnectable: true, TypeC: true, USB3: true, CompanionPort: 2},
		// Socket 3: USB 2.0 only
		{Hub: hub, Number: 3, Root: true, UserConnectable: true, USB2: true},
		// The webcam inside the lid: not a socket
		{Hub: hub, Number: 4, Root: true, USB2: true},
		// A port of an external hub: not the PC's own
		{Hub: `\\?\usb#vid_05e3&pid_0610#x`, Number: 1, UserConnectable: true, USB3: true},
	}
	got := Summarize(ports)
	want := PortSummary{Known: true, USB3: 2, USB3TypeC: 1, USB2Only: 1, USB2OnlyKnown: true}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	// Without companion information the USB 2.0-only count can't be trusted.
	for i := range ports {
		ports[i].CompanionPort = 0
	}
	if got := Summarize(ports); got.USB2OnlyKnown || got.USB3 != 2 {
		t.Errorf("no companions: got %+v", got)
	}
	if got := Summarize(nil); got.Known {
		t.Error("no ports: should be unknown")
	}
}

func TestParsePortConnector(t *testing.T) {
	name := `USB#ROOT_HUB30#4&2#{f18a0e88-c30c-11d0-8815-00a0c906bed8}`
	b := make([]byte, 16+2*(len(name)+1))
	le := binary.LittleEndian
	le.PutUint32(b[0:], 2)
	le.PutUint32(b[4:], uint32(len(b)))
	le.PutUint32(b[8:], 1|8) // user-connectable, Type-C
	le.PutUint16(b[12:], 0)
	le.PutUint16(b[14:], 6)
	for i, c := range utf16.Encode([]rune(name)) {
		le.PutUint16(b[16+2*i:], c)
	}
	pc, err := ParsePortConnector(b)
	if err != nil {
		t.Fatal(err)
	}
	if !pc.UserConnectable || !pc.TypeC || pc.CompanionPort != 6 || pc.CompanionHubLink != name {
		t.Errorf("got %+v", pc)
	}
	if HubDevicePath(pc.CompanionHubLink) != `\\?\`+name {
		t.Errorf("device path %q", HubDevicePath(pc.CompanionHubLink))
	}
	if HubDevicePath(`\??\`+name) != `\\?\`+name || HubDevicePath(`\\?\`+name) != `\\?\`+name || HubDevicePath(" ") != "" {
		t.Error("HubDevicePath prefixes")
	}
	if _, err := ParsePortConnector(make([]byte, 15)); err == nil {
		t.Error("a short buffer must be an error")
	}
	// A lying ActualLength must not make it read past the buffer.
	le.PutUint32(b[4:], 1<<20)
	if _, err := ParsePortConnector(b); err != nil {
		t.Error(err)
	}
}

func TestHubPortCounts(t *testing.T) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[4:], 26)
	if n, err := HubPortCount(b); err != nil || n != 26 {
		t.Errorf("HubPortCount = %d, %v", n, err)
	}
	b[6] = 4
	if n, err := HubPortCountLegacy(b); err != nil || n != 4 {
		t.Errorf("HubPortCountLegacy = %d, %v", n, err)
	}
	if _, err := HubPortCount(b[:5]); err == nil {
		t.Error("short buffer")
	}
	if _, err := HubPortCountLegacy(b[:6]); err == nil {
		t.Error("short buffer")
	}
}

func TestIsRootHub(t *testing.T) {
	if !IsRootHub(`USB\ROOT_HUB30\4&2A3B&0&0`) || !IsRootHub(`usb\root_hub20\x`) || IsRootHub(`USB\VID_05E3&PID_0610\5&1`) {
		t.Error("IsRootHub")
	}
}

func TestSpeedStrings(t *testing.T) {
	for s := SpeedUnknown; s <= SpeedSuperPlus; s++ {
		if s.String() == "" {
			t.Errorf("speed %d has no name", s)
		}
	}
	if SpeedHigh.MaxMBps() >= SpeedSuper.MaxMBps() || SpeedUnknown.MaxMBps() != 0 {
		t.Error("MaxMBps ordering")
	}
}

// Fuzz-ish: no input may make the decoders panic.
func FuzzUSBDecoders(f *testing.F) {
	f.Add(exBuf(2, true, 0x210))
	f.Add(v2Buf(7, 15))
	f.Add(make([]byte, 16))
	f.Fuzz(func(t *testing.T, b []byte) {
		ParseConnectionInfoEx(b)
		ParseConnectionInfoV2(b)
		ParsePortConnector(b)
		HubPortCount(b)
		HubPortCountLegacy(b)
	})
}
