// Package portable finds and indexes "portable devices": phones, cameras,
// iPhones, e-readers and media players that Windows shows under This PC as a
// device rather than as a drive letter, because they talk MTP or PTP instead
// of being a disk.
//
// Unlike internal/phone (ADB, which needs USB debugging turned on), this works
// with any phone straight out of the box: plug it in, choose "File transfer"
// on the phone, and Windows lists it here. The price is speed - every folder
// is a separate request to the device - which is fine for the background agent.
//
// Detection (devices_windows.go) asks Windows for the WPD device interfaces,
// which is cheap enough to poll every few seconds. Reading the files
// (shell_windows.go) goes through the Windows Shell, the same layer Explorer
// uses. The walking logic itself (scanner.go) is portable and tested with a
// fake device.
package portable

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// WPDInterfaceGUID is GUID_DEVINTERFACE_WPD, the interface every portable device registers.
const WPDInterfaceGUID = "{6AC27878-A6FA-4155-BA85-F98F491D4F33}"

// TargetPrefix is how a portable device is named on the command line: "device" or "device:<which>".
const TargetPrefix = "device"

// Device is one connected portable device.
type Device struct {
	// InterfacePath is the WPD interface, e.g.
	// \\?\usb#vid_04e8&pid_6860&ms_comp_mtp&samsung_android#6&2b5c4a3c&0&0000#{6ac27878-...}
	InterfacePath string
	// InstanceID is the device's PnP instance ID, e.g.
	// USB\VID_04E8&PID_6860&MS_COMP_MTP&SAMSUNG_ANDROID\6&2B5C4A3C&0&0000
	InstanceID string
	// Serial is the USB serial number when the device has one ("R58M12345").
	// For an Android phone it is the same serial adb uses.
	Serial string
	// Name is what Windows calls the device ("Galaxy S21 5G").
	Name string
}

// SourceID is the stable key of the device's index: "dev-" plus 12 hex
// characters. It comes from the USB serial number, so the same phone keeps its
// index whichever port it is plugged into; a device without a serial number
// falls back to its instance ID (which Windows keeps per port).
func (d Device) SourceID() string {
	key := "instance:" + strings.ToLower(d.InstanceID)
	if d.Serial != "" {
		key = "serial:" + strings.ToLower(d.Serial)
	}
	sum := sha1.Sum([]byte(key))
	return "dev-" + hex.EncodeToString(sum[:])[:12]
}

// Label is the name to show for the device.
func (d Device) Label() string {
	if n := strings.TrimSpace(d.Name); n != "" {
		return n
	}
	return "Portable device"
}

// Arg is what to pass to `drivedex index` to index exactly this device.
func (d Device) Arg() string { return TargetPrefix + ":" + d.SourceID() }

// IsUSBInterface reports whether a WPD interface belongs to a real USB device.
//
// Windows also registers a WPD interface for every USB pen drive and memory
// card (\\?\swd#wpdbusenum#...), so that they show up in the Photos app; those
// already have a drive letter and are indexed as drives. Bluetooth file
// transfer devices are skipped as well: far too slow to list a whole phone.
func IsUSBInterface(path string) bool {
	return strings.HasPrefix(strings.ToLower(path), `\\?\usb#`)
}

// InstanceIDFromInterface turns a device interface path into the device's
// instance ID: \\?\usb#vid_1&pid_2#ABC#{guid}  ->  USB\VID_1&PID_2\ABC.
func InstanceIDFromInterface(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, `\\?\`)
	if !ok {
		return "", false
	}
	i := strings.LastIndexByte(rest, '#')
	if i <= 0 || !strings.HasPrefix(rest[i+1:], "{") {
		return "", false
	}
	parts := strings.Split(rest[:i], "#")
	if len(parts) < 3 || parts[0] == "" || parts[len(parts)-1] == "" {
		return "", false
	}
	return strings.ToUpper(strings.Join(parts, `\`)), true
}

// SerialFromInstanceID returns the USB serial number in an instance ID, or "".
// Windows uses the serial as the last part when the device has one
// (USB\VID_18D1&PID_4EE1\R58M12345); otherwise it makes up an ID that always
// contains '&' (USB\...\6&2B5C4A3C&0&0000).
func SerialFromInstanceID(id string) string {
	i := strings.LastIndexByte(id, '\\')
	if i < 0 {
		return ""
	}
	last := id[i+1:]
	if last == "" || strings.ContainsRune(last, '&') {
		return ""
	}
	return last
}

// ParseTarget recognises "device" and "device:<which>" on the command line.
// which is "" for plain "device".
func ParseTarget(arg string) (which string, ok bool) {
	a := strings.TrimSpace(arg)
	if strings.EqualFold(a, TargetPrefix) {
		return "", true
	}
	if len(a) > len(TargetPrefix)+1 && strings.EqualFold(a[:len(TargetPrefix)+1], TargetPrefix+":") {
		return strings.TrimSpace(a[len(TargetPrefix)+1:]), true
	}
	return "", false
}

// ErrNoDevice means no portable device is connected.
var ErrNoDevice = errors.New("no phone, camera or other portable device is connected")

// NotFoundError means devices are connected, but none matches what was asked for.
type NotFoundError struct {
	Which     string
	Connected []Device
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("no connected portable device matches %q", e.Which)
}

// MultipleError means several devices are connected and the user must choose.
type MultipleError struct{ Devices []Device }

func (e *MultipleError) Error() string {
	return fmt.Sprintf("%d portable devices are connected", len(e.Devices))
}

// PickDevice chooses the device the user meant. which may be its index ID
// ("dev-1a2b3c4d5e6f"), its serial number or its name (not case sensitive);
// "" picks the only connected device.
func PickDevice(devs []Device, which string) (Device, error) {
	if len(devs) == 0 {
		return Device{}, ErrNoDevice
	}
	if which == "" {
		if len(devs) > 1 {
			return Device{}, &MultipleError{Devices: devs}
		}
		return devs[0], nil
	}
	var byName []Device
	for _, d := range devs {
		if strings.EqualFold(d.SourceID(), which) || (d.Serial != "" && strings.EqualFold(d.Serial, which)) {
			return d, nil
		}
		if strings.EqualFold(d.Label(), which) {
			byName = append(byName, d)
		}
	}
	switch len(byName) {
	case 0:
		return Device{}, &NotFoundError{Which: which, Connected: devs}
	case 1:
		return byName[0], nil
	}
	return Device{}, &MultipleError{Devices: byName}
}
