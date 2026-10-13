package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/scan"
	"github.com/yashwardhanv/dd/internal/ui"
)

// openADB finds adb and returns a handle for it. A variable so tests can
// supply a fake phone.
var openADB = func() (*phone.ADB, error) {
	path, err := phone.FindADB(activeAdbPath())
	if err != nil {
		return nil, adbNotFound(err)
	}
	return phone.NewADB(path), nil
}

// adbNotFound is the friendly error for "adb isn't installed".
func adbNotFound(cause error) error {
	return userErr("adb (Android Debug Bridge) was not found, so 'drivedex index phone' can't be used yet",
		"any phone in \"File transfer\" mode can be indexed without it: drivedex index device\n"+
			"For the faster way, over USB debugging, set the phone up like this:\n"+phone.SetupSteps, cause)
}

// phoneHeader is the starting index header for a phone.
func phoneHeader(d phone.Device) index.Header {
	return index.Header{
		SourceKind: index.KindPhone,
		SourceID:   phone.SourceID(d.Serial),
		Label:      d.Name(),
		FileSystem: "android",
		RootPath:   activePhoneRootPath(),
		LastMount:  d.Serial,
		Scanner:    "adb",
	}
}

// runIndexPhone implements `drivedex index phone[:<serial>]`.
func runIndexPhone(ctx context.Context, out io.Writer, serial string) error {
	adb, err := openADB()
	if err != nil {
		return err
	}
	devs, err := adb.Devices(ctx)
	if err != nil {
		return userErr("couldn't ask adb for the connected phones", "run 'adb kill-server' and try again", err)
	}
	dev, err := phone.PickDevice(devs, serial)
	if err != nil {
		return phoneSelectionError(err)
	}

	header := phoneHeader(dev)
	scanner := phone.NewScanner(adb, dev.Serial, activePhoneRoot())
	fmt.Fprintln(out, ui.Dim(fmt.Sprintf("Reading the file list straight from %s over USB debugging…", dev.Name())))
	return withIndexLock(header.SourceID, header.Label, func() error {
		return runIndexJob(ctx, out, header, scanner, "phone "+dev.Name(), false)
	})
}

// phoneSelectionError explains why no phone could be chosen.
func phoneSelectionError(err error) error {
	var (
		none     *phone.NoDeviceError
		multi    *phone.MultipleDevicesError
		notUsble *phone.NotUsableError
	)
	switch {
	case errors.As(err, &multi):
		hint := "choose one with 'drivedex index phone:<serial>'. Connected:"
		for _, d := range multi.Devices {
			hint += fmt.Sprintf("\n    phone:%s   (%s)", d.Serial, d.Name())
		}
		return userErr(err.Error(), hint, nil)
	case errors.As(err, &notUsble):
		hint := notUsble.Device.Hint()
		if hint == "" {
			hint = "unplug and replug the phone, unlock it, and check USB debugging is on"
		}
		return userErr(err.Error(), hint, nil)
	case errors.As(err, &none):
		hint := "plug the phone in with a data cable, unlock it and allow USB debugging. Setup steps:\n" + phone.SetupSteps
		if len(none.Connected) > 0 {
			hint = "run 'adb devices' to see what is connected"
		}
		return userErr(err.Error(), hint, nil)
	}
	return err
}

// printPhoneNotes explains anything unusual about a phone scan.
func printPhoneNotes(out io.Writer, scanner scan.Scanner, st scan.Stats) {
	if ps, ok := scanner.(*phone.Scanner); ok && ps.NamesOnly() {
		fmt.Fprintln(out, ui.Yellow("note:")+" this phone's 'stat' command didn't work, so only file names were indexed (sizes and dates are unknown).")
	}
	if st.Errors > 0 {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("Skipped %d lines from the phone that couldn't be understood.", st.Errors)))
	}
	if st.Files == 0 {
		fmt.Fprintln(out, ui.Yellow("note:")+" the phone reported no files. Is its storage empty, or is the screen locked?")
	}
	fmt.Fprintln(out, ui.Dim("Android hides some folders (such as Android/data) from adb; empty folders are not indexed."))
}

// phoneEnv adds the connected, authorized phones to env. Called only when
// there is at least one phone index to show, so people without phones never
// pay for starting adb.
func phoneEnv(env *index.Env) {
	adb, err := openADB()
	if err != nil {
		return // no adb: every phone simply shows as offline
	}
	ctx, cancel := context.WithTimeout(context.Background(), phoneCheckTimeout)
	defer cancel()
	devs, err := adb.Devices(ctx)
	if err != nil {
		return
	}
	for _, d := range devs {
		if d.Usable() {
			env.Phones[phone.SourceID(d.Serial)] = true
		}
	}
}
