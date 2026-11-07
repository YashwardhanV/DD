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

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/preflight"
	"github.com/yashwardhanv/dd/internal/ui"
)

// copyChecks supplies the free-space / file-system queries (replaced in tests).
var copyChecks = copier.DefaultChecks

// newCopyCmd implements `drivedex copy <src> <dst>`.
func newCopyCmd() *cobra.Command {
	var workers, onConflict, verify, onCase, onBad, sourceList, readOrder, compare string
	var dryRun, yes, asJSON, verbose, caseSensitive, noManifest bool

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
			mode, ok := copier.ParseVerifyMode(verify)
			if !ok {
				return userErr(fmt.Sprintf("--verify %q isn't valid", verify), "use --verify (the same as --verify=media), --verify=cache or --verify=off", nil)
			}
			names, err := nameOptions(onCase, onBad, caseSensitive)
			if err != nil {
				return err
			}
			if err := checkCopyModes(sourceList, readOrder, compare); err != nil {
				return err
			}
			stdout := cmd.OutOrStdout()
			human := stdout
			if asJSON {
				human = cmd.ErrOrStderr() // stdout carries only the JSON result
			}
			js := &copyJSON{}
			err = runCopy(cmd.Context(), human, args[0], args[1], copyFlags{
				workers: effectiveWorkers(cmd, workers), onConflict: onConflict, verify: mode, dryRun: dryRun,
				names: names, yes: yes, verbose: verbose, in: cmd.InOrStdin(), json: js,
				sourceList: sourceList, readOrder: readOrder, human: human, compare: compare, noManifest: noManifest,
			})
			if asJSON {
				finishJSON(stdout, js, err)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&workers, "workers", "auto", "files copied at once: auto (picked from the drive types) or a number")
	cmd.Flags().StringVar(&onConflict, "on-conflict", copier.OnConflictNewer, "when a file already exists: newer (replace only if the source is newer), skip, or overwrite")
	cmd.Flags().StringVar(&verify, "verify", "off", verifyHelp)
	cmd.Flags().Lookup("verify").NoOptDefVal = "media"
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be copied, skipped or overwritten, without writing anything")
	addNameFlags(cmd, &onCase, &onBad, &caseSensitive)
	addCompareFlags(cmd, &compare, &noManifest)
	cmd.Flags().StringVar(&sourceList, "source-list", sourceListAuto, "where the list of files to copy comes from: auto (the saved index of an NTFS drive when the change journal proves it is current - needs Administrator - otherwise a fresh listing), index (fail if it can't be used), or scan (always list the source)")
	cmd.Flags().StringVar(&readOrder, "read-order", copier.ReadOrderAuto, "order of small files: size (biggest first), disk (where they lie on the disk; experimental, for USB hard disks), or auto (= size for now)")
	cmd.Flags().BoolVar(&yes, "yes", false, "continue past pre-flight warnings without asking")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON result object on stdout (messages go to stderr)")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "list every affected file in the pre-flight report")
	return cmd
}

// addNameFlags adds the flags for names the destination can't hold as they are.
func addNameFlags(cmd *cobra.Command, onCase, onBad *string, caseSensitive *bool) {
	cmd.Flags().StringVar(onCase, "on-case-collision", copier.OnCaseRename, `when two names differ only in letter case (a.jpg and A.jpg, from a phone): rename (the second becomes "A (case 2).jpg"), skip, or fail`)
	cmd.Flags().StringVar(onBad, "on-bad-name", copier.OnBadReplace, "when a name can't be stored on Windows (notes:1.txt, CON.txt, a trailing dot): replace (with look-alike characters), skip, or fail")
	cmd.Flags().BoolVar(caseSensitive, "case-sensitive-dest", false, "make the folders the copy creates case-sensitive (NTFS, Windows 10 1803+), so a.jpg and A.jpg can both stay as they are; some older Windows programs mishandle such folders")
}

// nameOptions checks the name flags.
func nameOptions(onCase, onBad string, caseSensitive bool) (copier.NameOptions, error) {
	switch onCase {
	case copier.OnCaseRename, copier.OnCaseSkip, copier.OnCaseFail:
	default:
		return copier.NameOptions{}, userErr(fmt.Sprintf("--on-case-collision %q isn't valid", onCase), "use rename, skip or fail", nil)
	}
	switch onBad {
	case copier.OnBadReplace, copier.OnBadSkip, copier.OnBadFail:
	default:
		return copier.NameOptions{}, userErr(fmt.Sprintf("--on-bad-name %q isn't valid", onBad), "use replace, skip or fail", nil)
	}
	return copier.NameOptions{OnCaseCollision: onCase, OnBadName: onBad, CaseSensitiveDest: caseSensitive}, nil
}

// verifyHelp explains --verify, including its honest limit.
const verifyHelp = `check every copied file: --verify (= media) hashes the source while copying, flushes the copy to the drive and reads it back WITHOUT the Windows cache; --verify=cache reads it back through the cache (cheaper, but only proves DriveDex wrote the right bytes); off. Honest limit: some cheap USB devices confirm a flush without writing, and a cache inside the device can still answer the read - only unplugging and replugging defeats that (doctor --capacity does). Phones are checked with the phone's own sha256sum/sha1sum/md5sum`

type copyFlags struct {
	workers, onConflict string
	verify              copier.VerifyMode
	dryRun              bool
	names               copier.NameOptions
	yes, verbose        bool
	in                  io.Reader
	json                *copyJSON
	sourceList          string
	readOrder           string
	human               io.Writer // where messages go (stdout, or stderr with --json)
	compare             string    // --compare
	noManifest          bool      // --no-manifest
	sync                *syncRun  // set by 'drivedex sync'
}

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
	if !copier.ValidPolicy(f.onConflict) {
		return userErr(fmt.Sprintf("--on-conflict %q isn't valid", f.onConflict), "use newer, skip or overwrite", nil)
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
	var srcDev preflight.Device
	plannedFrom := "scan"
	srcRoot := "" // the local source, for the USB speed check ("" for a phone)
	var srcID string
	var jstart *journalStart
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
		srcDev = preflight.Device{Label: dev.Name(), ID: phone.SourceID(dev.Serial), Class: preflight.ClassPhone}
		srcID = phone.SourceID(dev.Serial)
		plannedFrom = "phone"
		remote := copier.PhonePathRoot(spec.path, activePhoneRoot())
		status.Update("Reading the file list from " + dev.Name() + " …")
		plan, err = copier.PlanPhone(ctx, adb, dev.Serial, remote, dst, f.onConflict, max(workers, 4))
		status.Clear()
		if err != nil {
			return planError(srcArg, err)
		}
		if f.verify != copier.VerifyOff {
			tool, terr := adb.HashTool(ctx, dev.Serial)
			if terr != nil || tool == "" {
				fmt.Fprintln(out, ui.Dim("note: this phone has no sha256sum, sha1sum or md5sum, so its files can't be verified; they are still copied atomically."))
				f.verify = copier.VerifyOff
			} else {
				plan.Phone.HashTool = tool
				fmt.Fprintln(out, ui.Dim("Verifying with the phone's own "+tool+" (each file is hashed on the phone and on the PC)."))
			}
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
		}
		srcID = srcIDFor(src)
		if f.compare != copier.CompareFast && f.compare != copier.CompareStrict && !f.noManifest {
			jstart = startJournal(src) // before listing: every later change shows in the journal
		}
		status.Update("Reading the file list from " + src + " …")
		out.Flush()
		plan, plannedFrom, err = planLocalSource(ctx, out, src, dst, f, workers)
		status.Clear()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintln(out, "Stopped. Nothing was copied.")
				return nil
			}
			return planError(srcArg, err)
		}
	}

	return finishPlan(ctx, out, status, plan, planRun{
		workers: workers, why: why, verify: f.verify, dryRun: f.dryRun, dstArg: dstArg, srcArg: srcArg, srcRoot: srcRoot,
		names: f.names, yes: f.yes, verbose: f.verbose, in: f.in, json: f.json,
		srcDev: srcDev, plannedFrom: plannedFrom, readOrder: f.readOrder,
		compare: f.compare, noManifest: f.noManifest, srcID: srcID, jstart: jstart, sync: f.sync,
	})
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
	var ue *UserError
	if errors.As(err, &ue) {
		return err // already in plain words (e.g. --source-list index)
	}
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
