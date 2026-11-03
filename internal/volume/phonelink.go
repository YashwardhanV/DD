package volume

import (
	"errors"
	"strconv"
	"strings"
)

// ErrPhoneNotMatched means no present USB device carries the phone's serial
// (wireless adb, or a serial Windows couldn't use): the speed can't be told.
var ErrPhoneNotMatched = errors.New("the phone's serial number doesn't match a USB device")

// Why phones need their own lookup
//
// A drive letter leads to a disk, and the disk to the USB port (usblink_windows.go).
// A phone used over USB debugging has no drive letter: adb only knows its
// SERIAL NUMBER. Windows, though, names every USB device after the serial its
// USB descriptor reports, so a Pixel with serial 1A2B3C4D5E6F shows up as the
// device instance
//
//	USB\VID_18D1&PID_4EE7\1A2B3C4D5E6F
//
// Finding that instance among the present USB devices leads to the port it is
// plugged into, and from there to the same speed questions as for a drive.
//
// When the serial can't be matched (a wireless adb connection, or a phone
// whose serial Windows couldn't use, so Windows made up an ID such as
// "6&2C0E8B0&0&1"), the answer is "can't tell" - never a guess.

// MatchPhoneInstance picks, from the instance IDs of the present USB devices,
// the one that belongs to the phone with the given adb serial. Only a whole
// USB device counts - "USB\VID_xxxx&PID_yyyy\<serial>" - not one of its
// interfaces ("...&MI_01\..." - those carry no serial; the code that asks
// Windows walks from an interface up to its parent device instead).
// Serials are compared without regard to case (Windows stores some in upper
// case). ok is false when nothing matches or the match is ambiguous.
func MatchPhoneInstance(instanceIDs []string, serial string) (string, bool) {
	serial = strings.TrimSpace(serial)
	if serial == "" || strings.ContainsAny(serial, `\:`) {
		return "", false // wireless adb ("192.168.1.5:5555") or nonsense
	}
	match := ""
	for _, id := range instanceIDs {
		parts := strings.Split(id, `\`)
		if len(parts) != 3 || !strings.EqualFold(parts[0], "USB") {
			continue
		}
		hw := strings.ToUpper(parts[1])
		if !strings.HasPrefix(hw, "VID_") || strings.Contains(hw, "&MI_") {
			continue
		}
		if !strings.EqualFold(parts[2], serial) {
			continue
		}
		if match != "" && !strings.EqualFold(match, id) {
			return "", false // two devices claim the same serial: don't guess
		}
		match = id
	}
	return match, match != ""
}

// PhoneAdvice explains a phone's USB link in one line, like Advice does for
// drives, plus the phone-only case: a phone whose own USB port is USB 2.0
// (common, even on recent mid-range phones) can never go faster than about
// 40 MB/s, whatever port or cable is used.
func PhoneAdvice(l USBLink, ports PortSummary) (text, tip string) {
	if l.Speed != SpeedUnknown && l.Capable == SpeedHigh && l.Speed >= SpeedHigh {
		return "running at " + l.Speed.String() + " - this phone's port is USB 2.0: about 40 MB/s is the most any software can get", ""
	}
	text, tip = Advice(l, ports)
	return phoneWords(text), phoneWords(tip)
}

// phoneWords rewords drive advice for a phone.
func phoneWords(s string) string {
	s = strings.ReplaceAll(s, "this drive", "this phone")
	s = strings.ReplaceAll(s, "the drive", "the phone")
	s = strings.ReplaceAll(s, "a USB 3 drive", "a USB 3 phone")
	s = strings.ReplaceAll(s, "move your drive", "move your phone")
	s = strings.ReplaceAll(s, "move the drive", "move the phone")
	return s
}

// LinkSummary is the short form for tables and the line before a copy:
// "USB 2.0 · 480 Mbps - max ≈ 40 MB/s".
func LinkSummary(l USBLink) string {
	if l.Speed == SpeedUnknown {
		return "USB speed: can't tell"
	}
	if mb := l.Speed.MaxMBps(); mb > 0 {
		return l.Speed.String() + " - max ≈ " + strconv.FormatFloat(mb, 'f', -1, 64) + " MB/s"
	}
	return l.Speed.String()
}
