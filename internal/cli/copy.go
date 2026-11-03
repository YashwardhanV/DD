package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/copier/trace"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/ui"
)

// copyChecks supplies the free-space / file-system queries (replaced in tests).
var copyChecks = copier.DefaultChecks

// newCopyCmd implements `drivedex copy <src> <dst>`.
func newCopyCmd() *cobra.Command {
	var workers, onConflict string
	var verify, dryRun bool
	var f copyFlags

	cmd := &cobra.Command{
		Use:   "copy <src> <dst>",
		Short: "Copy files fast, with resume - from drives, folders or an Android phone",
		Long: `Copies a file or folder to a destination folder using several workers at once,
which is much faster than Explorer for folders with many small files. A folder
is copied INTO the destination (dst\<folder name>\...), like Explorer does.

It is safe to stop with Ctrl+C and run the same command again: files that
already arrived are skipped, and a file never appears under its real name until
it is complete.

To copy from an Android phone, give a phone path: phone:/DCIM/Camera (paths are
relative to the phone's storage) or phone:<serial>:/DCIM/Camera when several
phones are connected. Phones are read with 'adb pull', not slow MTP.

Always use "Safely Remove Hardware" before unplugging the destination drive:
for speed, DriveDex does not force every file to be flushed to the disk.`,
		Example: `  drivedex copy E:\Photos D:\Backup --dry-run
  drivedex copy E:\Photos D:\Backup
  drivedex copy phone:/DCIM/Camera D:\PhoneBackup
  drivedex copy C:\Projects F:\ --on-conflict skip --verify`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.workers, f.onConflict, f.verify, f.dryRun = effectiveWorkers(cmd, workers), onConflict, verify, dryRun
			return runCopy(cmd.Context(), cmd.OutOrStdout(), args[0], args[1], f)
		},
	}
	cmd.Flags().StringVar(&workers, "workers", "auto", "files copied at once: auto (picked from the drive types) or a number")
	cmd.Flags().StringVar(&onConflict, "on-conflict", copier.OnConflictNewer, "when a file already exists: newer (replace only if the source is newer), skip, or overwrite")
	cmd.Flags().BoolVar(&verify, "verify", false, "check every copied file with a CRC-32C checksum: the original's is computed while copying, and the copy is re-read straight from the disk (bypassing the RAM cache), so the media itself is checked; with --v1 both files are re-read and the copy may come from RAM")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be copied, skipped or overwritten, without writing anything")
	cmd.Flags().BoolVar(&f.trace, "trace", false, "measure where the time goes and print a report with hints at the end")
	cmd.Flags().StringVar(&f.traceFile, "trace-file", "", "also save the trace report as JSON to this file (for benchmarks and bug reports)")
	cmd.Flags().BoolVar(&f.v1, "v1", false, "turn every DriveDex 2.0 copy feature off and copy exactly like version 1 (for comparisons)")
	cmd.Flags().StringVar(&f.plan, "plan", "auto", "where the file list comes from: auto (the saved index when it can be trusted), index, or walk (list the source again, like v1)")
	cmd.Flags().BoolVar(&f.fresh, "fresh", false, "list the source again instead of using the saved index (same as --plan walk)")
	cmd.Flags().StringVar(&f.unbuffered, "unbuffered", "", "files of 256 MB and more: on (straight to the disk, skipping the RAM cache, like robocopy /J), off, or auto; default from copy_unbuffered")
	cmd.Flags().BoolVar(&f.direct, "experimental-direct", false, "EXPERIMENTAL: into a brand-new folder, write small files straight to their names and keep a resume journal (.drivedex-resume) instead of renaming each file")
	cmd.Flags().StringVar(&f.phoneMode, "phone-mode", "", "phone copies: auto (tar stream when the phone supports it), tar, or pull (one adb pull per batch, like v1); default from phone_copy_mode")
	return cmd
}

type copyFlags struct {
	workers, onConflict string
	verify, dryRun      bool

	// v2.0
	trace      bool   // print the trace report
	traceFile  string // also write it as JSON
	v1         bool   // every v2 feature off
	plan       string // auto | index | walk
	fresh      bool   // = --plan walk
	phoneMode  string // auto | tar | pull ("" = the phone_copy_mode setting)
	unbuffered string // auto | on | off ("" = the copy_unbuffered setting)
	direct     bool   // 18e experiment
}

// fastPaths returns the stage 18 switches. "auto" for unbuffered I/O means
// OFF until the owner's benchmark shows it helps (BUILD-V2 18b).
func (f copyFlags) fastPaths() (copier.FastPaths, error) {
	if f.v1 {
		return copier.FastPaths{}, nil
	}
	u := strings.ToLower(strings.TrimSpace(f.unbuffered))
	if u == "" {
		u = activeConfig.CopyUnbuffered
	}
	switch u {
	case "", "auto", "on", "off":
	default:
		return copier.FastPaths{}, userErr(fmt.Sprintf("--unbuffered %q isn't valid", f.unbuffered), "use auto, on or off", nil)
	}
	return copier.FastPaths{SingleOpen: true, MediaVerify: true, Unbuffered: u == "on", Direct: f.direct}, nil
}

// phoneOptions returns the copier's phone settings from the flags and config.
func (f copyFlags) phoneOptions() (mode string, streams int, compress string, err error) {
	if f.v1 {
		return "pull", 1, "off", nil
	}
	mode = strings.ToLower(strings.TrimSpace(f.phoneMode))
	if mode == "" {
		mode = activeConfig.PhoneCopyMode
	}
	switch mode {
	case "", "auto", "tar", "pull":
	default:
		return "", 0, "", userErr(fmt.Sprintf("--phone-mode %q isn't valid", f.phoneMode), "use auto, tar or pull", nil)
	}
	return mode, activeConfig.PhoneTarStreams, activeConfig.PhonePullCompress, nil
}

// planMode turns --plan / --fresh / --v1 / copy_plan_from_index into "auto",
// "index" or "find" (list the source like v1), and the oldest index to trust
// (-1 = any age, 0 = the default).
func (f copyFlags) planMode() (mode string, anyAge bool, err error) {
	p := strings.ToLower(strings.TrimSpace(f.plan))
	switch p {
	case "", "auto", "index", "walk":
	default:
		return "", false, userErr(fmt.Sprintf("--plan %q isn't valid", f.plan), "use auto, index or walk", nil)
	}
	switch {
	case f.v1 || f.fresh || p == "walk":
		return "find", false, nil
	case p == "index":
		return "index", activeConfig.CopyPlanFromIndex == "always", nil
	}
	switch activeConfig.CopyPlanFromIndex {
	case "never":
		return "find", false, nil
	case "always":
		return "auto", true, nil
	}
	return "auto", false, nil
}

// traced reports whether the copy is measured at all.
func (f copyFlags) traced() bool { return f.trace || f.traceFile != "" }

// phoneSpec is a parsed "phone:..." copy source.
type phoneSpec struct {
	serial string // "" = the only connected phone
	path   string // as typed after the colon
}

// parsePhoneSource understands "phone:/DCIM/Camera" and "phone:<serial>:/DCIM/Camera".
func parsePhoneSource(arg string) (spec phoneSpec, ok bool, err error) {
	a := strings.TrimSpace(arg)
	if len(a) < 6 || !strings.EqualFold(a[:6], "phone:") {
		return phoneSpec{}, false, nil
	}
	rest := a[6:]
	switch {
	case strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, `\`):
		return phoneSpec{path: rest}, true, nil
	}
	// phone:<serial>:/path - the serial may itself contain ':' (192.168.1.5:5555),
	// so split at the first ":/" (or ":\").
	for i := 0; i+1 < len(rest); i++ {
		if rest[i] == ':' && (rest[i+1] == '/' || rest[i+1] == '\\') {
			if i == 0 {
				break
			}
			return phoneSpec{serial: rest[:i], path: rest[i+1:]}, true, nil
		}
	}
	return phoneSpec{}, true, userErr(fmt.Sprintf("can't understand the phone path %q", arg),
		"write it like phone:/DCIM/Camera or phone:<serial>:/DCIM/Camera", nil)
}

func runCopy(ctx context.Context, stdout io.Writer, srcArg, dstArg string, f copyFlags) error {
	began := time.Now()
	var rec *trace.Recorder
	if f.traced() {
		rec = trace.New()
		ctx = trace.WithRecorder(ctx, rec)
	}
	if !copier.ValidPolicy(f.onConflict) {
		return userErr(fmt.Sprintf("--on-conflict %q isn't valid", f.onConflict), "use newer, skip or overwrite", nil)
	}
	planMode, anyAge, err := f.planMode()
	if err != nil {
		return err
	}
	phoneMode, tarStreams, pullCompress, err := f.phoneOptions()
	if err != nil {
		return err
	}
	fast, err := f.fastPaths()
	if err != nil {
		return err
	}
	n, autoWorkers, err := parseWorkers(f.workers)
	if err != nil {
		return err
	}
	dst, err := resolveDestination(dstArg)
	if err != nil {
		return err
	}
	out := bufio.NewWriterSize(stdout, 64*1024)
	defer out.Flush()
	status := ui.NewStatusLine(os.Stderr)

	// --- plan -----------------------------------------------------------------
	var plan *copier.Plan
	srcRoot := ""                    // the local source, for the USB speed check ("" for a phone)
	phoneLink := 0.0                 // the phone's USB link in MB/s (0 = unknown)
	var afterRun func(copier.Result) // e.g. refresh the phone index from what the plan learned
	var tune *copier.TuneOptions     // stage 19 autotuner (nil = fixed worker count)
	spec, isPhone, err := parsePhoneSource(srcArg)
	if err != nil {
		return err
	}
	workers, why := n, "set with --workers"
	if isPhone {
		adb, err := openADB()
		if err != nil {
			return err
		}
		devs, err := adb.Devices(ctx)
		if err != nil {
			return userErr("couldn't ask adb for the connected phones", "run 'adb kill-server' and try again", err)
		}
		dev, err := phone.PickDevice(devs, spec.serial)
		if err != nil {
			return phoneSelectionError(err)
		}
		if autoWorkers {
			dstPenalty, dstKnown := seekPenaltyFor(dst)
			workers = copier.AutoWorkers(false, true, dstPenalty, dstKnown)
			why = autoWhy(workers)
		}
		remote := copier.PhonePathRoot(spec.path, activePhoneRoot())
		phoneLink = printPhoneLink(out, dev.Name(), dev.Serial)
		status.Update("Reading the file list from " + dev.Name() + " …")
		req := copier.PhonePlanRequest{
			ADB: adb, Serial: dev.Serial, Remote: remote, DstRoot: dst, Policy: f.onConflict, Workers: max(workers, 4),
			Index: loadPhoneIndex(dev.Serial), Mode: planMode,
		}
		if anyAge {
			req.MaxAge = -1
		}
		res, err := copier.PlanPhoneSmart(ctx, req)
		status.Clear()
		if err != nil {
			if errors.Is(err, copier.ErrIndexNotUsable) {
				return userErr(err.Error(), "drop --plan index to let DriveDex list the phone, or run: drivedex index phone", nil)
			}
			return planError(srcArg, err)
		}
		plan = res.Plan
		fmt.Fprintln(out, ui.Dim(res.Why))
		afterRun = func(r copier.Result) { refreshPhoneIndex(out, dev.Serial, res, r) }
		if f.verify {
			fmt.Fprintln(out, ui.Dim("note: --verify isn't available for phone copies (there is nothing on the PC to compare with); files are still copied atomically."))
			f.verify = false
		}
	} else {
		src, err := resolveSource(srcArg)
		if err != nil {
			return err
		}
		srcRoot = src
		if autoWorkers {
			srcPenalty, srcKnown := seekPenaltyFor(src)
			dstPenalty, dstKnown := seekPenaltyFor(dst)
			workers = copier.AutoWorkers(srcPenalty, srcKnown, dstPenalty, dstKnown)
			why = autoWhy(workers)
			// Stage 19: the count then adjusts itself while copying.
			if activeConfig.CopyAutotune && !f.v1 {
				minW, maxW := copier.TuneBounds((srcKnown && srcPenalty) || (dstKnown && dstPenalty))
				tune = &copier.TuneOptions{Min: minW, Max: maxW}
			}
		}
		status.Update("Reading the file list from " + src + " …")
		localMode := planMode
		if localMode == "find" {
			localMode = "walk"
		}
		res, err := planLocalCopy(ctx, src, dst, f.onConflict, max(workers, 2), localMode, anyAge)
		status.Clear()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(out, "Stopped. Nothing was copied.")
				return nil
			}
			if errors.Is(err, copier.ErrIndexNotUsable) {
				return userErr(err.Error(), "drop --plan index to let DriveDex list the source, or index the drive first: drivedex index "+src, nil)
			}
			return planError(srcArg, err)
		}
		plan = res.Plan
		if res.Plan.Source != copier.PlanWalk || f.trace {
			fmt.Fprintln(out, ui.Dim(res.Why))
		}
		if res.Sweeper != nil {
			sweepStart := time.Now()
			afterRun = func(copier.Result) { refreshLocalIndex(out, res.Scope, res.Sweeper.Result(), sweepStart) }
		}
	}

	return finishPlan(ctx, out, status, plan, planRun{
		workers: workers, why: why, verify: f.verify, dryRun: f.dryRun, dstArg: dstArg, srcRoot: srcRoot,
		trace: rec, showTrace: f.trace, traceFile: f.traceFile, began: began, phoneLinkMBps: phoneLink,
		afterRun: afterRun, phoneMode: phoneMode, tarStreams: tarStreams, pullCompress: pullCompress, fast: fast,
		tune: tune,
	})
}

// planRun are the options for carrying out a finished plan.
type planRun struct {
	workers int
	why     string // why that many workers (for the message)
	verify  bool
	dryRun  bool
	dstArg  string // the destination as the user typed it (for messages)
	srcRoot string // a local source folder, "" when there isn't one (phone, search results)

	trace     *trace.Recorder // nil = not traced
	showTrace bool            // print the report
	traceFile string          // write the report as JSON here
	began     time.Time       // when the command started (planning is part of the copy's time)

	phoneLinkMBps float64 // a phone source's USB link speed (0 = unknown)

	afterRun func(copier.Result) // runs after the copy (nil = nothing)

	phoneMode    string // auto | tar | pull
	tarStreams   int
	pullCompress string
	fast         copier.FastPaths
	tune         *copier.TuneOptions
}

// finishPlan checks, announces, (dry-)runs and reports one copy plan. Both
// `copy` and `search --copy-to` use it, so they behave identically.
func finishPlan(ctx context.Context, out *bufio.Writer, status *ui.StatusLine, plan *copier.Plan, o planRun) error {
	dst, dstArg, workers, why := plan.DstRoot, o.dstArg, o.workers, o.why
	f := copyFlags{verify: o.verify, dryRun: o.dryRun}
	counts := plan.Counts()
	if counts.Files == 0 {
		fmt.Fprintln(out, "Nothing to copy: the source has no files.")
		return nil
	}

	// --- safety checks --------------------------------------------------------
	if err := plan.Check(copyChecks()); err != nil {
		return checkError(dstArg, err)
	}

	// --- announce -------------------------------------------------------------
	if counts.Skipped == 0 {
		fmt.Fprintf(out, "Copying %s files (%s) with %d workers…\n",
			ui.Thousands(counts.Files), ui.HumanSize(counts.TotalBytes), workers)
	} else {
		fmt.Fprintf(out, "%s files (%s): %s already up to date, copying %s (%s) with %d workers…\n",
			ui.Thousands(counts.Files), ui.HumanSize(counts.TotalBytes), ui.Thousands(counts.Skipped),
			ui.Thousands(counts.Copy+counts.Overwrite), ui.HumanSize(counts.WriteBytes), workers)
	}
	if !f.dryRun {
		line := fmt.Sprintf("(%d workers: %s)", workers, why)
		if o.tune != nil {
			line += fmt.Sprintf(" - adjusts itself between %d and %d while copying", o.tune.Min, o.tune.Max)
		}
		fmt.Fprintln(out, ui.Dim(line))
	}
	// A USB 3 drive stuck at USB 2.0 speed is worth knowing BEFORE a long copy.
	anyUSB := warnSlowUSB(out, []string{o.srcRoot, dst}, counts.WriteBytes)
	bigLanes := 0
	if anyUSB {
		bigLanes = usbBigLanes
	}

	if f.dryRun {
		return printDryRun(out, plan, counts)
	}
	if counts.Copy+counts.Overwrite == 0 {
		fmt.Fprintln(out, "Everything is already there. Nothing to do.")
		return nil
	}

	// --- run ------------------------------------------------------------------
	out.Flush()
	res, err := copier.Run(ctx, plan, copier.Options{
		Workers:  workers,
		BigLanes: bigLanes,
		Verify:   f.verify,
		Trace:    o.trace,

		PhoneMode:    o.phoneMode,
		TarStreams:   o.tarStreams,
		PullCompress: o.pullCompress,
		LateChecks:   copyChecks(),
		Fast:         o.fast,
		Tune:         o.tune,
		Progress: func(p ui.CopyProgress) {
			status.Update(ui.FormatCopyProgress(p))
		},
	})
	status.Clear()
	if err != nil {
		return userErr("the copy couldn't start", "check the destination can be written to: "+dst, err)
	}
	defer func() { reportTrace(out, plan, o, workers, res) }()
	if o.afterRun != nil {
		o.afterRun(res)
	}

	// --- report ---------------------------------------------------------------
	rate := ui.PerSecond(res.BytesWritten, res.Elapsed)
	fmt.Fprintf(out, "Copied %s files (%s) in %s · %s/s · %d skipped · %d failed\n",
		ui.Thousands(res.Written), ui.HumanSize(res.BytesWritten), ui.FormatDuration(res.Elapsed),
		ui.HumanSize(rate), res.Skipped, res.Failed)
	if res.PhoneLane != "" {
		fmt.Fprintln(out, ui.Dim("Phone files came by "+laneWords(res.PhoneLane)+"."))
	}
	if res.StreamRejected > 0 {
		fmt.Fprintln(out, ui.Yellow("note:")+fmt.Sprintf(" the phone's tar stream held %d unexpected %s (not asked for, a link, or a name outside the folder); %s ignored.",
			res.StreamRejected, plural(int(res.StreamRejected), "entry", "entries"), plural(int(res.StreamRejected), "it was", "they were")))
	}
	if res.Found > 0 {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("%d %s found while copying (added or changed since the index was made).",
			res.Found, plural(int(res.Found), "file was", "files were"))))
	}
	if res.Vanished > 0 {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("%d %s had been deleted since the index was made (vanished) - nothing to copy, not an error.",
			res.Vanished, plural(int(res.Vanished), "file", "files"))))
	}
	if res.NotRegular > 0 {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("%d links/special files were skipped (DriveDex never follows links).", res.NotRegular)))
	}
	if f.verify && res.VerifyMismatches == 0 && res.Failed == 0 {
		fmt.Fprintln(out, ui.Green("Verified:")+" every copied file matches its original (checksums).")
	}
	if res.Cancelled {
		fmt.Fprintln(out, "Stopped. Run the same command again to resume.")
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
		return userErr(fmt.Sprintf("%d %s could not be copied", res.Failed, plural(int(res.Failed), "file", "files")),
			"run the same command again to retry just those files", nil)
	}
	fmt.Fprintln(out, ui.Dim("Tip: use \"Safely Remove Hardware\" before unplugging the destination drive."))
	return nil
}

// usbBigLanes is how many big files are copied at once when a USB drive is
// involved (the other workers keep going with small files). A USB link moves
// one big stream as fast as eight, and finished files are what a resume after
// an unplug keeps.
const usbBigLanes = 2

func autoWhy(workers int) string {
	switch workers {
	case 2:
		return "a spinning hard disk is involved"
	case 8:
		return "both ends are flash/SSD"
	default:
		return "drive type unknown"
	}
}

// resolveSource turns the source argument into an absolute path that exists.
func resolveSource(arg string) (string, error) {
	p := strings.TrimSpace(arg)
	if len(p) == 2 && p[1] == ':' {
		p += `\`
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", arg, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", userErr(fmt.Sprintf("can't open %s", arg), "check the path exists (and that the drive or phone is plugged in)", err)
	}
	return abs, nil
}

// resolveDestination turns the destination argument into an absolute path; the
// folder doesn't have to exist yet, but it can't be an existing file.
func resolveDestination(arg string) (string, error) {
	p := strings.TrimSpace(arg)
	if strings.HasPrefix(strings.ToLower(p), "phone:") {
		return "", userErr("DriveDex never writes to a phone", "the destination must be a folder on this PC", nil)
	}
	if len(p) == 2 && p[1] == ':' {
		p += `\`
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", arg, err)
	}
	if st, err := os.Stat(abs); err == nil && !st.IsDir() {
		return "", userErr(fmt.Sprintf("%s is a file, not a folder", arg), "give a destination folder; it is created if it doesn't exist", nil)
	}
	return abs, nil
}

func planError(srcArg string, err error) error {
	if errors.Is(err, copier.ErrDestinationInsideSource) {
		return userErr(fmt.Sprintf("can't copy %s into itself", srcArg),
			"choose a destination folder outside the folder being copied", err)
	}
	if errors.Is(err, copier.ErrRemoteNotFound) {
		return userErr(fmt.Sprintf("%s doesn't exist on the phone", srcArg),
			"paths are relative to the phone's storage, e.g. phone:/DCIM/Camera; search with 'drivedex search' to find the exact folder", err)
	}
	return userErr(fmt.Sprintf("couldn't read the files in %s", srcArg), "check the drive or phone is connected and unlocked", err)
}

func checkError(dstArg string, err error) error {
	switch {
	case errors.Is(err, copier.ErrNotEnoughSpace):
		return userErr("the destination doesn't have enough free space", "free up space on "+dstArg+" or choose another destination", err)
	case errors.Is(err, copier.ErrFileTooBigForFAT32):
		return userErr("the destination is FAT32, which can't store a file of 4 GB or more",
			"copy to an NTFS or exFAT drive instead (or reformat this one as exFAT - back it up first)", err)
	}
	return userErr("the copy can't go ahead", "run with --debug for details", err)
}

// printDryRun lists what would happen, then a summary. Nothing is written.
func printDryRun(out io.Writer, plan *copier.Plan, c copier.Counts) error {
	for _, it := range plan.Items {
		fmt.Fprintf(out, "  %-28s %s  %s\n", it.Decision.String(), it.Rel, ui.Dim("("+ui.HumanSize(it.Size)+")"))
		if it.Blocked != "" {
			fmt.Fprintf(out, "  %s %s\n", ui.Red("! blocked:"), it.Blocked)
		}
	}
	fmt.Fprintf(out, "\nWould copy %s files (%s), overwrite %s, skip %s. Nothing was written (dry run).\n",
		ui.Thousands(c.Copy), ui.HumanSize(c.WriteBytes), ui.Thousands(c.Overwrite), ui.Thousands(c.Skipped))
	return nil
}

// laneWords describes a phone lane: "tar" -> "one continuous tar stream".
func laneWords(lane string) string {
	switch {
	case lane == "tar":
		return "tar stream (many files per request; big files by adb pull)"
	case strings.HasPrefix(lane, "pull ("):
		return "adb pull " + strings.TrimPrefix(lane, "pull ")
	}
	return "adb pull"
}
