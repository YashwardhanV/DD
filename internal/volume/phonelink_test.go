package volume

import (
	"strings"
	"testing"
)

// Instance IDs as Windows lists them (recorded formats: Pixel, Samsung,
// Xiaomi, plus a composite interface, a hub, a mouse and a pen drive).
var presentUSB = []string{
	`USB\ROOT_HUB30\4&2A1B3C4D&0&0`,
	`USB\VID_046D&PID_C52B\5&1F0E4A7&0&2`,           // a mouse receiver: Windows-made ID, no serial
	`USB\VID_18D1&PID_4EE7\1A2B3C4D5E6F`,            // Google Pixel (adb), serial 1A2B3C4D5E6F
	`USB\VID_18D1&PID_4EE7&MI_01\6&3B1C2D3E&0&0001`, // ... one of its interfaces
	`USB\VID_04E8&PID_6860\R58M1234ABC`,             // Samsung (MTP + adb composite), serial R58M1234ABC
	`USB\VID_04E8&PID_6860&MI_02\7&1234ABCD&0&0002`, // ... its adb interface
	`USB\VID_2717&PID_FF48\ABC12345`,                // Xiaomi, adb serial abc12345 (Windows keeps upper case)
	`USB\VID_0781&PID_5581\4C530001230101115334`,    // SanDisk pen drive
	`USBSTOR\DISK&VEN_SANDISK&PROD_ULTRA\4C530001230101115334&0`,
}

func TestMatchPhoneInstance(t *testing.T) {
	cases := []struct {
		serial, want string
		ok           bool
	}{
		{"1A2B3C4D5E6F", `USB\VID_18D1&PID_4EE7\1A2B3C4D5E6F`, true},
		{"R58M1234ABC", `USB\VID_04E8&PID_6860\R58M1234ABC`, true},
		{"abc12345", `USB\VID_2717&PID_FF48\ABC12345`, true}, // case-insensitive
		{"emulator-5554", "", false},
		{"192.168.1.5:5555", "", false},  // wireless adb: no USB device
		{"6&3B1C2D3E&0&0001", "", false}, // interfaces never match
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := MatchPhoneInstance(presentUSB, c.serial)
		if got != c.want || ok != c.ok {
			t.Errorf("MatchPhoneInstance(%q) = %q, %v; want %q, %v", c.serial, got, ok, c.want, c.ok)
		}
	}
	// Two devices claiming the same serial: refuse to guess.
	dup := append([]string{`USB\VID_1111&PID_2222\R58M1234ABC`}, presentUSB...)
	if got, ok := MatchPhoneInstance(dup, "R58M1234ABC"); ok {
		t.Errorf("ambiguous serial matched %q", got)
	}
}

func TestPhoneAdvice(t *testing.T) {
	ports := PortSummary{Known: true, USB3: 2}
	// The phone itself is USB 2.0 only.
	text, tip := PhoneAdvice(USBLink{Speed: SpeedHigh, Capable: SpeedHigh}, ports)
	if !strings.Contains(text, "this phone's port is USB 2.0: about 40 MB/s is the most any software can get") || tip != "" {
		t.Errorf("USB 2.0 phone: %q / %q", text, tip)
	}
	// A USB 3 phone in a USB 2.0 port.
	text, tip = PhoneAdvice(USBLink{Speed: SpeedHigh, Capable: SpeedSuper, PortKnown: true}, ports)
	if !strings.Contains(text, "USB 2.0 only") || !strings.Contains(tip, "2 USB 3 ports") || strings.Contains(text+tip, " drive ") {
		t.Errorf("slow port: %q / %q", text, tip)
	}
	// Full speed.
	text, _ = PhoneAdvice(USBLink{Speed: SpeedSuper, Capable: SpeedSuper}, ports)
	if !strings.Contains(text, "as fast as this phone can") {
		t.Errorf("full speed: %q", text)
	}
}

func TestLinkSummary(t *testing.T) {
	if s := LinkSummary(USBLink{Speed: SpeedHigh}); s != "USB 2.0 · 480 Mbps - max ≈ 40 MB/s" {
		t.Errorf("%q", s)
	}
	if s := LinkSummary(USBLink{}); s != "USB speed: can't tell" {
		t.Errorf("%q", s)
	}
}
