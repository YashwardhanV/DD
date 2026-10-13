package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/portable"
	"github.com/yashwardhanv/dd/internal/scan"
	"github.com/yashwardhanv/dd/internal/ui"
)

// listDevices and openDevice reach the portable devices (phones in "File
// transfer" mode, iPhones, cameras). Variables so tests can supply a fake device.
var (
	listDevices = portable.List
	openDevice  = func(d portable.Device) func(ctx context.Context) (portable.Folder, error) {
		return func(context.Context) (portable.Folder, error) { return portable.OpenDevice(d) }
	}
)

// deviceHeader is the starting index header for a portable device.
func deviceHeader(d portable.Device) index.Header {
	return index.Header{
		SourceKind: index.KindDevice,
		SourceID:   d.SourceID(),
		Label:      d.Label(),
		FileSystem: "MTP",
		LastMount:  d.InterfacePath,
		Scanner:    "shell",
	}
}

// runIndexDevice implements `drivedex index device[:<which>]`.
func runIndexDevice(ctx context.Context, out io.Writer, which string) error {
	devs, err := listDevices()
	if errors.Is(err, errors.ErrUnsupported) {
		return userErr("portable devices only work on Windows", "run DriveDex on your Windows PC", err)
	}
	if err != nil {
		return userErr("couldn't list the portable devices", "unplug the device, plug it in again and retry", err)
	}
	dev, err := portable.PickDevice(devs, which)
	if err != nil {
		return deviceSelectionError(err)
	}
	header := deviceHeader(dev)
	scanner := &portable.Scanner{Open: openDevice(dev)}
	fmt.Fprintln(out, ui.Dim(fmt.Sprintf("Reading %s through Windows, one folder at a time (a phone with many photos can take a few minutes)…", dev.Label())))
	return withIndexLock(header.SourceID, header.Label, func() error {
		return runIndexJob(ctx, out, header, scanner, dev.Label(), false)
	})
}

// deviceSelectionError explains why no device could be chosen.
func deviceSelectionError(err error) error {
	var (
		multi    *portable.MultipleError
		notFound *portable.NotFoundError
	)
	list := func(devs []portable.Device) string {
		s := ""
		for _, d := range devs {
			s += fmt.Sprintf("\n    %s   (%s)", d.Arg(), d.Label())
		}
		return s
	}
	switch {
	case errors.As(err, &multi):
		return userErr(err.Error(), "choose one with 'drivedex index device:<name or ID>'. Connected:"+list(multi.Devices), nil)
	case errors.As(err, &notFound):
		return userErr(err.Error(), "connected:"+list(notFound.Connected), nil)
	case errors.Is(err, portable.ErrNoDevice):
		return userErr(err.Error(), "plug it in with a data cable, unlock it, and on a phone choose \"File transfer\" in the USB notification", nil)
	}
	return err
}

// printDeviceNotes explains anything unusual about a device scan.
func printDeviceNotes(out io.Writer, st scan.Stats) {
	if st.Errors > 0 {
		fmt.Fprintln(out, ui.Yellow("note:")+fmt.Sprintf(" %d %s couldn't be read (the device may have been locked or unplugged during the scan):",
			st.Errors, plural(int(st.Errors), "folder", "folders")))
		for _, e := range st.FirstErrors {
			fmt.Fprintln(out, ui.Dim("  "+e))
		}
	}
	if st.Files == 0 {
		fmt.Fprintln(out, ui.Yellow("note:")+" the device showed no files. Is it unlocked?")
	}
}

// deviceEnv adds the connected portable devices to env. Asking Windows is
// cheap (no device is opened), so it is always done.
func deviceEnv(env *index.Env) {
	devs, err := listDevices()
	if err != nil {
		return // every device simply shows as offline
	}
	for _, d := range devs {
		env.Devices[d.SourceID()] = true
	}
}
