package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/agent"
	"github.com/yashwardhanv/dd/internal/config"
	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/doctor"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/install"
	"github.com/yashwardhanv/dd/internal/ntfs"
	"github.com/yashwardhanv/dd/internal/shellint"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// Hooks for the parts of doctor that ask Windows questions (replaced in tests).
var (
	busTypeFor = volume.BusType

	// journalStateFor reports the NTFS change-journal state of a drive. Needs Administrator.
	journalStateFor = func(root string) doctor.JournalState {
		sess, err := openNTFS(root)
		if err != nil {
			return doctor.JournalError
		}
		defer sess.Close()
		_, err = ntfs.QueryJournal(sess.Dev)
		switch {
		case err == nil:
			return doctor.JournalActive
		case errors.Is(err, ntfs.ErrJournalNotActive), errors.Is(err, ntfs.ErrJournalDeleteInProgress):
			return doctor.JournalInactive
		default:
			return doctor.JournalError
		}
	}

	// benchOptions is the speed-test setup; tests shrink it so they stay fast.
	benchOptions = doctor.BenchOptions{FreeBytes: copier.DefaultChecks().FreeBytes}
)

const doctorLong = `Checks your setup and explains, in plain English, why things might be slow or
not working: the settings file, the background agent, Administrator rights,
Android phones (adb), and every connected drive (file system, USB speed class,
hard disk or flash, how fresh its index is).

Each line is marked ✅ fine, ⚠️ worth a look or ❌ broken, and tells you the one
thing to do about it. Doctor only looks - it changes nothing.

With --bench it also measures how fast a drive accepts data: it writes one
256 MB temporary file to the drive, times it, and deletes it again.`

// newDoctorCmd implements `drivedex doctor`.
func newDoctorCmd(version string) *cobra.Command {
	var bench string
	var yes bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check your setup and explain why things are slow",
		Long:  doctorLong,
		Example: `  drivedex doctor
  drivedex doctor --bench E:
  drivedex doctor --bench E: --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), version, bench, yes)
		},
	}
	cmd.Flags().StringVar(&bench, "bench", "", "also measure the write speed of this drive, e.g. E: (writes and deletes one 256 MB file)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "with --bench: don't ask before writing the test file")
	return cmd
}

func runDoctor(ctx context.Context, out io.Writer, in io.Reader, version, benchArg string, yes bool) error {
	// Check the --bench drive before doing anything else, so a typo fails fast.
	var benchRoot string
	if benchArg != "" {
		root, err := benchTarget(benchArg)
		if err != nil {
			return err
		}
		benchRoot = root
	}

	doctor.Build(collectFacts(ctx, version)).Print(out)

	if benchRoot != "" {
		fmt.Fprintf(out, "\n%s\n", ui.Bold("Speed test "+strings.TrimRight(benchRoot, `\/`)))
		if err := runBench(ctx, out, in, benchRoot, yes); err != nil {
			return err
		}
	}
	doctor.PrintTips(out)
	return nil
}

// benchTarget validates the --bench argument and returns the drive root ("E:\").
func benchTarget(arg string) (string, error) {
	letter, ok := volume.NormalizeLetter(arg)
	if !ok {
		return "", userErr(fmt.Sprintf("--bench %q isn't a drive letter", arg), "use something like --bench E:", nil)
	}
	root := letter + `\`
	v, err := volumeForPath(root)
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		return "", userErr("the speed test only works on Windows", "run DriveDex on your Windows PC", err)
	case err != nil:
		return "", userErr(fmt.Sprintf("can't find drive %s", letter), "run 'drivedex drives' to see what is connected", err)
	case !v.Ready:
		return "", userErr(fmt.Sprintf("drive %s isn't ready", letter), "is a disk inserted?", nil)
	}
	if v.Root != "" {
		root = v.Root
	}
	return root, nil
}

func runBench(ctx context.Context, out io.Writer, in io.Reader, root string, yes bool) error {
	opt := benchOptions
	size := opt.Size
	if size <= 0 {
		size = doctor.BenchSize
	}
	if !yes {
		q := fmt.Sprintf("This writes one temporary %d MB file (%s) to %s, times it, and deletes it afterwards. Nothing else on the drive is touched.\nContinue?",
			size>>20, doctor.BenchFileName, root)
		if !askYesNo(out, in, q) {
			fmt.Fprintln(out, "Cancelled. Nothing was written.")
			return nil
		}
	}

	status := ui.NewStatusLine(os.Stderr)
	defer status.Clear()
	opt.Progress = func(done, total int64) {
		status.Update(fmt.Sprintf("Writing test file… %d / %d MB", done>>20, total>>20))
	}
	res, err := doctor.Bench(ctx, root, opt)
	status.Clear()
	switch {
	case errors.Is(err, doctor.ErrNotEnoughSpace):
		return userErr(err.Error(), "free some space on the drive, or test a different drive", err)
	case errors.Is(err, doctor.ErrBenchFileExists):
		return userErr(err.Error(), "DriveDex never overwrites or deletes a file it didn't create: delete it yourself, then run again", err)
	case errors.Is(err, context.Canceled):
		return userErr("the speed test was cancelled", "the temporary file was removed", err)
	case err != nil:
		return userErr("the speed test failed: "+err.Error(), "check the drive isn't write-protected or full; run with --debug for details", err)
	}
	doctor.WriteBenchReport(out, root, res)
	fmt.Fprintln(out, ui.Dim("  (the test file was deleted)"))
	return nil
}

// collectFacts asks this PC everything doctor reports on.
func collectFacts(ctx context.Context, version string) doctor.Facts {
	f := doctor.Facts{Version: version, Now: time.Now(), StaleAfter: activeStaleAfter(), Elevated: isElevated()}

	// Settings: loaded again here (doctor starts with the defaults so it still
	// works when the file is broken) so any problem can be reported.
	if p, err := config.Path(); err == nil {
		f.ConfigPath = p
		if cfg, err := config.Load(p); err != nil {
			f.ConfigErr = err
		} else {
			f.StaleAfter = staleOf(cfg, f.StaleAfter)
		}
	} else {
		f.ConfigErr = err
	}

	// The index folder: can DriveDex save into it?
	store, err := openStore()
	if err != nil {
		f.IndexDirErr = err
	} else {
		f.IndexDir = store.Dir
		f.IndexDirErr = checkWritable(store.Dir)
	}
	infos := []index.Info(nil)
	if store != nil {
		infos, _ = store.List()
	}
	f.IndexCount = len(infos)

	if dir, err := agentDataDir(); err == nil {
		f.AgentKnown = true
		_, f.AgentRunning = currentAgent(dir)
	}
	if st, err := agent.Status(newStartupRegistry(), newStartupTasks()); err == nil {
		f.StartupKnown, f.StartupEnabled = true, st.Enabled()
	}
	if exe, err := shellExe(); err == nil {
		// The entries may point at this program or at the installed copy
		// (`drivedex install` run from a downloaded folder installs entries for
		// the copy in %LOCALAPPDATA%): either one is a correct menu.
		candidates := []string{exe}
		if dir, err := installDirFor(); err == nil {
			if inst := filepath.Join(dir, install.ExeName); !install.SamePath(inst, exe) {
				candidates = append(candidates, inst)
			}
		}
		for _, c := range candidates {
			sts, err := shellint.Status(newShellRegistry(), c)
			if err != nil {
				continue
			}
			installed := 0
			for _, s := range sts {
				if s.State == shellint.Current {
					installed++
				}
			}
			if !f.MenuKnown || installed > f.MenuInstalled {
				f.MenuKnown, f.MenuTotal, f.MenuInstalled = true, len(sts), installed
			}
		}
	}

	f.ADB = collectADB(ctx)
	f.Drives, f.DrivesErr = collectDrives(infos, f.Elevated)
	f.Ports = portSummary()
	return f
}

func staleOf(cfg config.Config, fallback time.Duration) time.Duration {
	if d, err := cfg.StaleDuration(); err == nil {
		return d
	}
	return fallback
}

// checkWritable proves DriveDex can create and delete a file in dir. It only
// ever touches DriveDex's own folder.
func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func collectADB(ctx context.Context) doctor.ADBFacts {
	adb, err := openADB()
	if err != nil {
		return doctor.ADBFacts{}
	}
	a := doctor.ADBFacts{Found: true, Path: adb.Path}
	a.Version, _ = adb.Version(ctx)
	devs, err := adb.Devices(ctx)
	if err != nil {
		a.Err = err
		return a
	}
	for _, d := range devs {
		a.Phones = append(a.Phones, doctor.PhoneFacts{Serial: d.Serial, Name: d.Name(), State: d.State})
	}
	return a
}

func collectDrives(infos []index.Info, elevated bool) ([]doctor.DriveFacts, error) {
	vols, err := listVolumes()
	if err != nil {
		return nil, err
	}
	byID := map[string]index.Info{}
	for _, in := range infos {
		if in.Err == nil {
			byID[in.Header.SourceID] = in
		}
	}
	var drives []doctor.DriveFacts
	for _, v := range vols {
		if !v.Ready || v.DriveType == volume.DriveRemote {
			continue
		}
		d := doctor.DriveFacts{Letter: v.Letter(), Label: v.Label, FileSystem: v.FileSystem}
		if bus, err := busTypeFor(v.Root); err == nil && bus.Bus != 0 {
			d.Bus = volume.BusName(bus.Bus)
		}
		if l, ok := usbLinkOf(v.Root); ok {
			d.USB = &l
		}
		if penalty, known := seekPenaltyFor(v.Root); known {
			d.Media = doctor.MediaFlash
			if penalty {
				d.Media = doctor.MediaHDD
			}
		}
		if in, ok := byID[volume.SourceID(v)]; ok && v.Serial != 0 {
			d.Index = &doctor.IndexFacts{UpdatedAt: in.Header.UpdatedAt, Entries: in.Header.EntryCount, Scanner: in.Header.Scanner}
		}
		if elevated && strings.EqualFold(v.FileSystem, "NTFS") {
			d.Journal = journalStateFor(v.Root)
		}
		drives = append(drives, d)
	}
	return drives, nil
}
