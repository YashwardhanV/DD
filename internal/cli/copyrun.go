package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/fsinfo"
	"github.com/yashwardhanv/dd/internal/preflight"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// exitBlocked is the exit code when pre-flight stops a copy before it starts.
const exitBlocked = 3

// planRun are the options for carrying out a finished plan.
type planRun struct {
	workers int
	why     string // why that many workers (for the message)
	verify  copier.VerifyMode
	dryRun  bool
	dstArg  string // the destination as the user typed it (for messages)
	srcArg  string // the source as the user typed it (for --json)
	srcRoot string // a local source folder, "" when there isn't one (phone, search results)

	names   copier.NameOptions
	yes     bool      // continue past warnings without asking
	verbose bool      // list every affected path
	in      io.Reader // answers to questions (nil = no questions can be asked)
	json    *copyJSON // when set, the result is recorded here for --json

	srcDev      preflight.Device // for the time estimate (Class/ID; USB is filled here)
	plannedFrom string           // "scan" | "index" | "phone" | "search" (for --json)
}

// copyJSON is the result object `copy --json` prints (one JSON object on
// stdout; every human message goes to stderr instead). The agent's rules read
// it from their child process.
type copyJSON struct {
	OK           bool              `json:"ok"`
	Kind         string            `json:"kind"`
	Source       string            `json:"source"`
	Destination  string            `json:"destination"`
	PlannedFrom  string            `json:"planned_from,omitempty"`
	DryRun       bool              `json:"dry_run"`
	Verify       string            `json:"verify"`
	Files        int64             `json:"files"`
	Bytes        int64             `json:"bytes"`
	ToWrite      int64             `json:"to_write"`
	WriteBytes   int64             `json:"write_bytes"`
	Skipped      int64             `json:"skipped"`
	Written      int64             `json:"written"`
	BytesWritten int64             `json:"bytes_written"`
	Failed       int64             `json:"failed"`
	Cancelled    bool              `json:"cancelled"`
	ElapsedMs    int64             `json:"elapsed_ms"`
	Failures     []jsonFailure     `json:"failures,omitempty"`
	Preflight    *preflight.Report `json:"preflight,omitempty"`
	Error        string            `json:"error,omitempty"`
	ExitCode     int               `json:"exit_code"`
}

type jsonFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// printJSON prints a result object as one line of JSON.
func printJSON(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintln(w, string(b))
}

// finishPlan checks, announces, (dry-)runs and reports one copy plan. Both
// `copy` and `search --copy-to` use it, so they behave identically.
func finishPlan(ctx context.Context, out *bufio.Writer, status *ui.StatusLine, plan *copier.Plan, o planRun) error {
	dst, workers, why := plan.DstRoot, o.workers, o.why
	js := o.json
	if js == nil {
		js = &copyJSON{}
	}
	js.Kind, js.Destination, js.DryRun, js.Verify, js.PlannedFrom = "copy", dst, o.dryRun, o.verify.String(), o.plannedFrom
	if js.Source == "" {
		js.Source = o.srcArg
	}
	counts := plan.Counts()
	if counts.Files == 0 {
		fmt.Fprintln(out, "Nothing to copy: the source has no files.")
		js.OK = true
		return nil
	}

	// --- pre-flight -------------------------------------------------------------
	checks := copyChecks()
	if checks.FileSystem != nil {
		// The file system as the checks see it wins (tests pretend FAT32 this way).
		if name, err := checks.FileSystem(fsinfo.NearestExisting(dst)); err == nil && name != "" && !strings.EqualFold(name, plan.DstFS.FileSystem) {
			plan.DstFS = fsinfo.ForFileSystem(name)
		}
	}
	slowPorts, anyUSB, srcDev, dstDev := usbFacts([]string{o.srcRoot, dst}, o.srcDev, dst)
	rep := preflight.Run(plan, preflight.Options{
		Names:     o.names,
		FreeBytes: checks.FreeBytes,
		DryRun:    o.dryRun,
		SlowPorts: slowPorts,
		Verify:    o.verify,
		Src:       srcDev,
		Dst:       dstDev,
		Rates:     learnedRates(),
	})
	js.Preflight = &rep
	counts = plan.Counts()
	js.Files, js.Bytes, js.ToWrite, js.WriteBytes, js.Skipped = counts.Files, counts.TotalBytes, counts.Copy+counts.Overwrite, counts.WriteBytes, counts.Skipped

	// --- announce -------------------------------------------------------------
	if counts.Skipped == 0 {
		fmt.Fprintf(out, "Copying %s files (%s) with %d workers…\n",
			ui.Thousands(counts.Files), ui.HumanSize(counts.TotalBytes), workers)
	} else {
		fmt.Fprintf(out, "%s files (%s): %s already up to date, copying %s (%s) with %d workers…\n",
			ui.Thousands(counts.Files), ui.HumanSize(counts.TotalBytes), ui.Thousands(counts.Skipped),
			ui.Thousands(counts.Copy+counts.Overwrite), ui.HumanSize(counts.WriteBytes), workers)
	}
	if !o.dryRun {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("(%d workers: %s)", workers, why)))
	}
	rep.Print(out, o.verbose || o.dryRun)
	if counts.Copy+counts.Overwrite > 0 {
		slowName := ""
		if len(slowPorts) > 0 {
			slowName = slowPorts[0].Drive
		}
		if txt := rep.Estimate.Text(slowName); txt != "" {
			fmt.Fprintln(out, ui.Dim(txt))
		}
		if txt := rep.Estimate.VerifyText(letterOrRoot(dst)); txt != "" {
			fmt.Fprintln(out, ui.Dim(txt))
		}
	}
	if rep.Blocked() {
		err := blockError(rep, o.dstArg)
		js.Error, js.ExitCode = err.Error(), exitBlocked
		return err
	}
	bigLanes := 0
	if anyUSB {
		bigLanes = usbBigLanes
	}

	if o.dryRun {
		js.OK = true
		return printDryRun(out, plan, counts)
	}
	if counts.Copy+counts.Overwrite == 0 {
		fmt.Fprintln(out, "Everything is already there. Nothing to do.")
		js.OK = true
		return nil
	}
	if rep.NeedsConfirmation() && !o.yes && interactive(o.in) {
		out.Flush()
		if !askYesNo(out, o.in, "Continue?") {
			out.Flush()
			fmt.Fprintln(out, "Cancelled. Nothing was copied. (Add --yes to continue past warnings without asking.)")
			js.Error, js.ExitCode = "cancelled at a pre-flight warning", exitBlocked
			return &UserError{Msg: "the copy was not started", Hint: "add --yes to continue past the warnings", Code: exitBlocked, Quiet: true}
		}
	}

	// --- run ------------------------------------------------------------------
	out.Flush()
	res, err := copier.Run(ctx, plan, copier.Options{
		Workers:           workers,
		BigLanes:          bigLanes,
		Verify:            o.verify,
		CaseSensitiveDirs: o.names.CaseSensitiveDest,
		Progress: func(p ui.CopyProgress) {
			status.Update(ui.FormatCopyProgress(p))
		},
	})
	status.Clear()
	if err != nil {
		js.Error, js.ExitCode = err.Error(), 1
		return userErr("the copy couldn't start", "check the destination can be written to: "+dst, err)
	}
	recordResult(js, res)
	recordHistory(plan, o, res, srcDev, dstDev)

	// --- report ---------------------------------------------------------------
	rate := ui.PerSecond(res.BytesWritten, res.Elapsed)
	fmt.Fprintf(out, "Copied %s files (%s) in %s · %s/s · %d skipped · %d failed\n",
		ui.Thousands(res.Written), ui.HumanSize(res.BytesWritten), ui.FormatDuration(res.Elapsed),
		ui.HumanSize(rate), res.Skipped, res.Failed)
	if res.NotRegular > 0 {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("%d links/special files were skipped (DriveDex never follows links).", res.NotRegular)))
	}
	if o.verify != copier.VerifyOff && res.VerifyMismatches == 0 && res.Failed == 0 && res.Written > 0 {
		if o.verify == copier.VerifyMedia {
			fmt.Fprintln(out, ui.Green("Verified:")+" every copied file was read back from the drive (not the PC's cache) and matches its original.")
		} else {
			fmt.Fprintln(out, ui.Green("Verified:")+" every copied file matches its original (read back through the Windows cache).")
		}
	}
	if res.Cancelled {
		fmt.Fprintln(out, "Stopped. Run the same command again to resume.")
		js.OK = true
		return nil
	}
	if res.Failed > 0 {
		fmt.Fprintln(out, ui.Red("These files could not be copied:"))
		for _, fl := range res.Failures {
			fmt.Fprintf(out, "  %s — %s\n", fl.Rel, fl.Err)
		}
		if more := res.Failed - int64(len(res.Failures)); more > 0 {
			fmt.Fprintf(out, "  … and %d more\n", more)
		}
		out.Flush()
		js.ExitCode = 1
		js.Error = fmt.Sprintf("%d files could not be copied", res.Failed)
		return userErr(fmt.Sprintf("%d %s could not be copied", res.Failed, plural(int(res.Failed), "file", "files")),
			"run the same command again to retry just those files", nil)
	}
	js.OK = true
	fmt.Fprintln(out, ui.Dim("Tip: use \"Safely Remove Hardware\" before unplugging the destination drive."))
	return nil
}

// blockError is the error for a copy pre-flight stopped (exit code 3). The
// two classic checks keep their wording and error values.
func blockError(rep preflight.Report, dstArg string) error {
	for _, is := range rep.Issues {
		if is.Severity != preflight.Block {
			continue
		}
		switch is.Code {
		case "no-space":
			return &UserError{Msg: "the destination doesn't have enough free space", Hint: is.Hint + " (" + dstArg + ")",
				Err: fmt.Errorf("%w: %s", copier.ErrNotEnoughSpace, is.Message), Code: exitBlocked}
		case "fat32-4gib":
			return &UserError{Msg: "the destination is FAT32, which can't store a file of 4 GB or more", Hint: is.Hint,
				Err: fmt.Errorf("%w: %s", copier.ErrFileTooBigForFAT32, strings.Join(is.Paths, ", ")), Code: exitBlocked}
		}
		hint := is.Hint
		if hint == "" {
			hint = "see the ✗ lines above"
		}
		return &UserError{Msg: "the copy can't start: " + lowerFirst(strings.TrimSuffix(is.Message, ".")), Hint: hint, Code: exitBlocked}
	}
	return nil
}

func lowerFirst(s string) string {
	if s == "" || (len(s) > 1 && s[1] >= 'A' && s[1] <= 'Z') {
		return s // an acronym such as "FAT32" stays as it is
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// interactive reports whether questions can be asked: in is a terminal, or a
// reader a test (or a caller) supplied on purpose. A pipe, a file or no input
// at all means "not interactive": warnings are printed and the copy goes on,
// as a script would expect.
func interactive(in io.Reader) bool {
	if in == nil {
		return false
	}
	if f, ok := in.(*os.File); ok {
		return ui.IsTerminal(f)
	}
	return true
}

// recordResult copies a copier.Result into the JSON result object.
func recordResult(js *copyJSON, res copier.Result) {
	js.Written, js.BytesWritten, js.Failed = res.Written, res.BytesWritten, res.Failed
	js.Cancelled, js.ElapsedMs = res.Cancelled, res.Elapsed.Milliseconds()
	js.Failures = nil
	for _, f := range res.Failures {
		js.Failures = append(js.Failures, jsonFailure{Path: f.Rel, Error: f.Err})
	}
}

// usbFacts looks at the USB links of the drives involved: which are slower
// than they could be (pre-flight Info), whether any is USB at all (for the
// big-file lanes), and the two devices for the time estimate.
func usbFacts(roots []string, src preflight.Device, dst string) (slow []preflight.SlowPort, anyUSB bool, srcDev, dstDev preflight.Device) {
	srcDev, dstDev = src, deviceFor(dst)
	if srcDev.Class == preflight.ClassUnknown && len(roots) > 0 && roots[0] != "" {
		srcDev = deviceFor(roots[0])
	}
	seen := map[string]bool{}
	var sum *volume.PortSummary // asked only if a drive is slow
	for i, r := range roots {
		vol := driveOf(r)
		if vol == "" || seen[vol] {
			continue
		}
		seen[vol] = true
		l, ok := usbLinkOf(r)
		if !ok {
			continue
		}
		anyUSB = true
		if i == 0 {
			srcDev.USB, srcDev.HasUSB = l, true
		}
		if strings.EqualFold(vol, driveOf(dst)) {
			dstDev.USB, dstDev.HasUSB = l, true
		}
		if !l.Judge().Slow() {
			continue
		}
		if sum == nil {
			s := portSummary()
			sum = &s
		}
		text, tip := volume.Advice(l, *sum)
		slow = append(slow, preflight.SlowPort{Drive: vol, Advice: text, Tip: tip})
	}
	return slow, anyUSB, srcDev, dstDev
}

// deviceFor describes the drive holding path for the time estimate: a
// spinning disk, a pen drive or card, or an SSD.
func deviceFor(path string) preflight.Device {
	d := preflight.Device{Label: letterOrRoot(path)}
	vol := driveOf(path)
	if vol == "" {
		return d
	}
	if v, err := volumeForPath(path); err == nil {
		d.ID = volume.SourceID(v)
	}
	penalty, known := seekPenaltyFor(vol + `\`)
	switch {
	case known && penalty:
		d.Class = preflight.ClassHDD
	case known:
		d.Class = preflight.ClassSSD
		if bus, err := busTypeFor(vol + `\`); err == nil && (bus.IsUSB() || bus.Bus == volume.BusTypeSd || bus.Bus == volume.BusTypeMmc) {
			fs := fsinfo.OrDefault(vol + `\`)
			if !fs.IsNTFS() {
				d.Class = preflight.ClassFlash // FAT/exFAT on USB or a card: a pen drive or memory card
			}
		}
	}
	return d
}

// learnedRates returns the transfer rates learned from earlier copies (P6's
// history), or nil before any exist.
var learnedRates = func() preflight.Rates { return nil }

// recordHistory adds a finished copy to the run history (P6). Until then it
// does nothing.
var recordHistory = func(plan *copier.Plan, o planRun, res copier.Result, src, dst preflight.Device) {}
