package phone

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Device is one entry of `adb devices -l`.
type Device struct {
	Serial string
	State  string // "device", "unauthorized", "offline", "no permissions", ...
	Model  string // from "model:Pixel_7", underscores turned into spaces
}

// Usable reports whether the phone is connected, unlocked and authorized.
func (d Device) Usable() bool { return d.State == "device" }

// Name is a friendly name for messages: the model, or the serial if unknown.
func (d Device) Name() string {
	if d.Model != "" {
		return d.Model
	}
	return d.Serial
}

// SourceID is the stable key of the phone's index file: "adb-<serial>".
func SourceID(serial string) string { return "adb-" + serial }

// Hint says what to do about a device that isn't usable.
func (d Device) Hint() string {
	switch d.State {
	case "unauthorized":
		return "Unlock your phone and tap Allow on the USB debugging prompt (then run 'adb kill-server' and replug if the prompt doesn't appear)."
	case "offline":
		return "The phone is connected but not responding: unplug it, plug it in again and unlock it (or run 'adb kill-server')."
	case "no permissions":
		return "Windows can't talk to this phone: install its USB driver, or try another cable/port."
	default:
		return ""
	}
}

// devicesTimeout stops a stuck adb server from hanging DriveDex forever.
const devicesTimeout = 15 * time.Second

// Version returns the first line of `adb version`, such as
// "Android Debug Bridge version 1.0.41".
func (a *ADB) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, devicesTimeout)
	defer cancel()
	out, err := a.run.Output(ctx, "version")
	if err != nil {
		return "", fmt.Errorf("running 'adb version': %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line, nil
		}
	}
	return "", nil
}

// Devices lists the phones adb can see.
func (a *ADB) Devices(ctx context.Context) ([]Device, error) {
	ctx, cancel := context.WithTimeout(ctx, devicesTimeout)
	defer cancel()
	out, err := a.run.Output(ctx, "devices", "-l")
	if err != nil {
		return nil, fmt.Errorf("running 'adb devices': %w", err)
	}
	return ParseDevices(string(out)), nil
}

// ParseDevices parses the output of `adb devices -l`:
//
//   - daemon started successfully          <- adb may print notices like this first
//     List of devices attached
//     R58M1234ABC   device usb:1-1 product:panther model:Pixel_7 device:panther transport_id:1
//     ABCDEF        unauthorized usb:1-1 transport_id:2
func ParseDevices(output string) []Device {
	var devs []Device
	seenHeader := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "*"):
			continue
		case strings.HasPrefix(line, "List of devices"):
			seenHeader = true
			continue
		case !seenHeader:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := Device{Serial: fields[0]}
		rest := strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		if strings.HasPrefix(rest, "no permissions") {
			d.State = "no permissions"
		} else {
			d.State = fields[1]
		}
		for _, f := range fields[2:] {
			if v, ok := strings.CutPrefix(f, "model:"); ok {
				d.Model = strings.ReplaceAll(v, "_", " ")
			}
		}
		devs = append(devs, d)
	}
	return devs
}

// ParseTarget understands what the user typed after "index": "phone" or
// "phone:<serial>" (case-insensitive prefix). ok is false for anything else.
func ParseTarget(arg string) (serial string, ok bool) {
	a := strings.TrimSpace(arg)
	if strings.EqualFold(a, "phone") {
		return "", true
	}
	if len(a) > 6 && strings.EqualFold(a[:6], "phone:") {
		return strings.TrimSpace(a[6:]), true
	}
	return "", false
}

// PickDevice chooses the phone to work with. serial is "" when the user just
// said "phone": then exactly one usable phone must be connected.
func PickDevice(devs []Device, serial string) (Device, error) {
	if serial != "" {
		for _, d := range devs {
			if strings.EqualFold(d.Serial, serial) {
				if !d.Usable() {
					return d, &NotUsableError{Device: d}
				}
				return d, nil
			}
		}
		return Device{}, &NoDeviceError{Wanted: serial, Connected: devs}
	}

	var usable []Device
	for _, d := range devs {
		if d.Usable() {
			usable = append(usable, d)
		}
	}
	switch {
	case len(usable) == 1:
		return usable[0], nil
	case len(usable) > 1:
		return Device{}, &MultipleDevicesError{Devices: usable}
	}
	// Nothing usable. If something is connected but unauthorized/offline, say so.
	for _, d := range devs {
		return d, &NotUsableError{Device: d}
	}
	return Device{}, &NoDeviceError{}
}

// NoDeviceError: no phone (or not the requested one) is connected.
type NoDeviceError struct {
	Wanted    string
	Connected []Device
}

func (e *NoDeviceError) Error() string {
	if e.Wanted != "" {
		return fmt.Sprintf("no phone with serial %q is connected", e.Wanted)
	}
	return "no Android phone is connected"
}

// NotUsableError: the phone is connected but can't be used yet.
type NotUsableError struct{ Device Device }

func (e *NotUsableError) Error() string {
	return fmt.Sprintf("the phone %s is %s", e.Device.Name(), e.Device.State)
}

// MultipleDevicesError: several phones are connected, so the user must choose.
type MultipleDevicesError struct{ Devices []Device }

func (e *MultipleDevicesError) Error() string {
	var parts []string
	for _, d := range e.Devices {
		parts = append(parts, fmt.Sprintf("%s (%s)", d.Name(), d.Serial))
	}
	return "several phones are connected: " + strings.Join(parts, ", ")
}
