package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/agent"
	"github.com/yashwardhanv/dd/internal/config"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/install"
	"github.com/yashwardhanv/dd/internal/shellint"
	"github.com/yashwardhanv/dd/internal/ui"
)

// Hooks for the parts of install/uninstall that change Windows (replaced in tests).
var (
	// installDirFor is where the program is installed: %LOCALAPPDATA%\Programs\DriveDex
	// (DRIVEDEX_INSTALL_DIR overrides it - for tests, and for a portable install).
	installDirFor = func() (string, error) {
		if d := os.Getenv("DRIVEDEX_INSTALL_DIR"); d != "" {
			return d, nil
		}
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			return "", errors.New("%LOCALAPPDATA% isn't set")
		}
		return filepath.Join(local, "Programs", "DriveDex"), nil
	}
	newPathStore    = func() install.PathStore { return install.WindowsUserPath{} }
	scheduleRemoval = install.ScheduleRemoval
)

const installLong = `Sets DriveDex up on this PC, one step at a time. Before each change to your
system it says exactly what it will do and asks y/N:

  1. copy drivedex.exe and drivedex-agent.exe to %LOCALAPPDATA%\Programs\DriveDex
  2. add that folder to your user PATH (so 'drivedex' works in any new terminal)
  3. add the Explorer right-click menu (optional)
  4. start the background agent when you log in (optional)
  5. run 'drivedex doctor' to check everything

Nothing needs administrator rights, and 'drivedex uninstall' undoes it all.`

func newInstallCmd(version string) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install DriveDex on this PC (program folder, PATH, optional menu and agent)",
		Long:  installLong,
		Example: `  drivedex install
  drivedex install --yes   (answer yes to every question)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInstall(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), version, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask before each step")
	return cmd
}

const uninstallLong = `Undoes 'drivedex install', one step at a time, asking y/N before each:
the right-click menu entries, the start-up entry and agent, the PATH entry and
the program folder. Finally it asks whether to also delete your saved indexes
and settings (default: keep them).

It only ever removes things DriveDex itself created; nothing on your drives is touched.`

func newUninstallCmd() *cobra.Command {
	var yes, purge bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove DriveDex from this PC",
		Long:  uninstallLong,
		Example: `  drivedex uninstall
  drivedex uninstall --yes            (keeps your indexes and settings)
  drivedex uninstall --yes --purge    (removes those as well)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUninstall(cmd.OutOrStdout(), cmd.InOrStdin(), yes, purge)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask before each step (indexes and settings are still kept unless --purge)")
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the saved indexes and settings, without asking")
	return cmd
}

func installDir() (string, error) {
	d, err := installDirFor()
	if err != nil {
		return "", userErr("can't work out where to install: "+err.Error(), "installing is for Windows; or set DRIVEDEX_INSTALL_DIR to the folder you want", err)
	}
	return d, nil
}

// confirm asks a question unless --yes was given.
func confirm(out io.Writer, in io.Reader, yes bool, question string) bool {
	if yes {
		return true
	}
	return askYesNo(out, in, question)
}

func step(out io.Writer, n, total int, title string) {
	fmt.Fprintf(out, "\n%s\n", ui.Bold(fmt.Sprintf("Step %d of %d — %s", n, total, title)))
}

// ---- install --------------------------------------------------------------------------------

func runInstall(ctx context.Context, out io.Writer, rawIn io.Reader, version string, yes bool) error {
	in := sharedReader(rawIn)
	self, err := shellExe()
	if err != nil {
		return userErr("couldn't find where drivedex.exe is", "run with --debug for details", err)
	}
	srcDir := filepath.Dir(self)
	dir, err := installDir()
	if err != nil {
		return err
	}
	installedExe := filepath.Join(dir, install.ExeName)
	installedAgent := filepath.Join(dir, install.AgentExeName)
	const total = 5

	fmt.Fprintf(out, "%s\nInstalling into %s\n", ui.Bold("DriveDex install"), dir)

	// --- 1. copy -----------------------------------------------------------------
	step(out, 1, total, "copy the program")
	restartAgent := false
	if install.SamePath(srcDir, dir) {
		fmt.Fprintln(out, "This is already the installed copy - nothing to copy.")
	} else {
		if !strings.EqualFold(filepath.Base(self), install.ExeName) {
			return userErr(fmt.Sprintf("install must be run from %s, not %s", install.ExeName, filepath.Base(self)),
				"run it from the unzipped DriveDex folder: .\\drivedex.exe install", nil)
		}
		for _, name := range install.ProgramFiles() {
			if _, err := os.Stat(filepath.Join(srcDir, name)); err != nil {
				return userErr(name+" wasn't found next to "+install.ExeName,
					"run install from the unzipped release folder (it has both programs), or run scripts\\build.ps1 first", err)
			}
		}
		running := agentIsRunning()
		q := fmt.Sprintf("Copy %s and %s into %s (the folder is created if needed)?", install.ExeName, install.AgentExeName, dir)
		if running && install.ProgramPresent(dir) {
			q = strings.TrimSuffix(q, "?") + "? The background agent is running: it will be stopped for the copy and started again."
		}
		if !confirm(out, in, yes, q) {
			fmt.Fprintln(out, "Cancelled. Nothing was changed.")
			return nil
		}
		if running && install.ProgramPresent(dir) {
			if err := runAgentStop(out); err != nil {
				return err
			}
			restartAgent = true
		}
		if err := install.CopyProgram(srcDir, dir, install.ProgramFiles()); err != nil {
			hint := "check the folder is writable and has free space"
			if install.IsInUse(err) {
				hint = "is DriveDex running from there? close it (and run 'drivedex agent stop'), then try again"
			}
			return userErr("couldn't copy the program: "+err.Error(), hint, err)
		}
		fmt.Fprintln(out, ui.Green("Copied."))
	}

	// --- 2. PATH -------------------------------------------------------------------
	step(out, 2, total, "add DriveDex to your PATH")
	onPath := false
	ps := newPathStore()
	if cur, err := ps.Get(); err == nil && install.PathContains(cur, dir) {
		fmt.Fprintln(out, dir, "is already on your PATH.")
		onPath = true
	} else if errors.Is(err, errors.ErrUnsupported) {
		return userErr("changing the PATH only works on Windows", "run DriveDex on your Windows PC", err)
	} else if confirm(out, in, yes, fmt.Sprintf("Add %s to your user PATH? (This changes HKEY_CURRENT_USER\\Environment - only your account - so 'drivedex' works in any NEW terminal.)", dir)) {
		changed, err := install.AddToUserPath(ps, dir)
		if err != nil {
			return userErr("couldn't change your PATH: "+err.Error(), "add the folder by hand: Settings → System → About → Advanced system settings → Environment Variables → Path → New", err)
		}
		onPath = true
		if changed {
			fmt.Fprintln(out, ui.Green("Done.")+" Open a NEW terminal window for it to take effect.")
		}
	} else {
		fmt.Fprintln(out, "Skipped. You can still run it with its full path:", installedExe)
	}

	// --- 3. right-click menu ----------------------------------------------------------
	step(out, 3, total, "right-click menu (optional)")
	if err := installShellEntries(out, in, yes, installedExe); err != nil {
		fmt.Fprintln(out, ui.Yellow("note:"), err) // optional step: report it and carry on
	}

	// --- 4. agent at log-in -------------------------------------------------------------
	step(out, 4, total, "start the background agent at log-in (optional)")
	if err := enableStartup(out, in, false, yes, installedAgent); err != nil {
		fmt.Fprintln(out, ui.Yellow("note:"), err)
	}
	if restartAgent {
		if dataDir, err := agentDataDir(); err == nil {
			if err := startAgentProgram(out, dataDir, installedAgent); err != nil {
				fmt.Fprintln(out, ui.Yellow("note:"), err)
			}
		}
	}

	// --- 5. doctor ------------------------------------------------------------------------
	step(out, 5, total, "check everything")
	if err := runDoctor(ctx, out, in, version, "", false); err != nil {
		return err
	}

	fmt.Fprintln(out)
	if onPath {
		fmt.Fprintln(out, ui.Green("DriveDex is installed.")+" Open a new terminal and type:  drivedex")
	} else {
		fmt.Fprintln(out, ui.Green("DriveDex is installed.")+" Run it with:  "+installedExe)
	}
	fmt.Fprintln(out, ui.Dim("To remove it again: drivedex uninstall"))
	return nil
}

func agentIsRunning() bool {
	dir, err := agentDataDir()
	if err != nil {
		return false
	}
	_, running := currentAgent(dir)
	return running
}

// ---- uninstall --------------------------------------------------------------------------------

func runUninstall(out io.Writer, rawIn io.Reader, yes, purge bool) error {
	in := sharedReader(rawIn)
	self, err := shellExe()
	if err != nil {
		return userErr("couldn't find where drivedex.exe is", "run with --debug for details", err)
	}
	dir, err := installDir()
	if err != nil {
		return err
	}
	const total = 5
	fmt.Fprintf(out, "%s\n", ui.Bold("DriveDex uninstall"))

	// --- 1. right-click menu ---------------------------------------------------------------
	step(out, 1, total, "right-click menu")
	if sts, err := shellint.Status(newShellRegistry(), self); err != nil {
		fmt.Fprintln(out, "Couldn't check the right-click entries:", err)
	} else {
		present := 0
		for _, s := range sts {
			if s.State != shellint.Missing {
				present++
			}
		}
		switch {
		case present == 0:
			fmt.Fprintln(out, "Not installed.")
		case confirm(out, in, yes, fmt.Sprintf("Remove the %d DriveDex right-click menu entries (your user's registry only)?", present)):
			if err := runShellUninstall(out); err != nil {
				return err
			}
		default:
			fmt.Fprintln(out, "Kept.")
		}
	}

	// --- 2. agent and start-up -----------------------------------------------------------------
	step(out, 2, total, "background agent")
	running := agentIsRunning()
	st, stErr := agent.Status(newStartupRegistry(), newStartupTasks())
	enabled := stErr == nil && st.Enabled()
	switch {
	case !running && !enabled:
		fmt.Fprintln(out, "The agent is not running and not set to start at log-in.")
	case confirm(out, in, yes, "Stop the background agent and stop it starting at log-in?"):
		if running {
			if err := runAgentStop(out); err != nil {
				return err
			}
		}
		if enabled {
			if err := runStartupDisable(out); err != nil {
				return err
			}
		}
	default:
		fmt.Fprintln(out, "Kept. (The program folder can't be removed while the agent is running.)")
	}

	// --- 3. PATH --------------------------------------------------------------------------------
	step(out, 3, total, "PATH entry")
	ps := newPathStore()
	if cur, err := ps.Get(); err != nil {
		fmt.Fprintln(out, "Couldn't read your PATH:", err)
	} else if !install.PathContains(cur, dir) {
		fmt.Fprintln(out, "DriveDex isn't on your PATH.")
	} else if confirm(out, in, yes, fmt.Sprintf("Remove %s from your user PATH?", dir)) {
		if _, err := install.RemoveFromUserPath(ps, dir); err != nil {
			return userErr("couldn't change your PATH: "+err.Error(), "remove the folder by hand: Settings → System → About → Advanced system settings → Environment Variables → Path", err)
		}
		fmt.Fprintln(out, ui.Green("Removed.")+" (Terminals that are already open keep the old PATH until you close them.)")
	} else {
		fmt.Fprintln(out, "Kept.")
	}

	// --- 4. program folder -----------------------------------------------------------------------
	step(out, 4, total, "program folder")
	switch {
	case !install.ProgramPresent(dir):
		fmt.Fprintln(out, "No installed program found in", dir)
	case confirm(out, in, yes, fmt.Sprintf("Delete drivedex.exe and drivedex-agent.exe from %s (and the folder, if it ends up empty)?", dir)):
		if install.SamePath(filepath.Dir(self), dir) {
			// This very program lives there and Windows won't delete a running .exe:
			// a tiny helper does it a moment after we exit.
			names := append(install.ProgramFiles(), install.ProgramFiles()[0]+".old")
			if err := scheduleRemoval(dir, names); err != nil {
				return userErr("couldn't schedule the removal: "+err.Error(), "delete the folder by hand once this window closes: "+dir, err)
			}
			fmt.Fprintln(out, ui.Green("Scheduled.")+" The program files are deleted a few seconds after this command ends.")
		} else {
			left, err := install.RemoveProgram(dir)
			switch {
			case err != nil:
				hint := "close DriveDex and try again, or delete the files by hand: " + dir
				if install.IsInUse(err) {
					hint = "something is still using them - run 'drivedex agent stop', close other DriveDex windows, then run uninstall again"
				}
				return userErr("couldn't remove everything from "+dir, hint, err)
			case len(left) > 0:
				fmt.Fprintf(out, "%s %s was left in place because it contains other files: %s\n", ui.Yellow("note:"), dir, strings.Join(left, ", "))
			default:
				fmt.Fprintln(out, ui.Green("Removed."))
			}
		}
	default:
		fmt.Fprintln(out, "Kept.")
	}

	// --- 5. saved indexes and settings --------------------------------------------------------
	step(out, 5, total, "saved indexes and settings")
	if err := removeOwnData(out, in, purge); err != nil {
		return err
	}

	fmt.Fprintln(out, "\nDone.", ui.Dim("Files on your drives were never touched."))
	return nil
}

// removeOwnData offers to delete the indexes, agent log and settings. These are
// the user's data, so unlike the other steps `--yes` does not cover it: only an
// explicit "y" at the question or --purge deletes them.
func removeOwnData(out io.Writer, in io.Reader, purge bool) error {
	indexDir, _ := index.DefaultDir()
	dataDir, _ := agent.DataDir()
	cfgPath, _ := config.Path()
	files := install.OwnedData(indexDir, dataDir, cfgPath)
	if len(files) == 0 {
		fmt.Fprintln(out, "No saved indexes or settings found.")
		return nil
	}
	var size int64
	indexes, logs, hasConfig := 0, 0, false
	for _, f := range files {
		size += f.Size
		switch base := strings.ToLower(filepath.Base(f.Path)); {
		case strings.HasSuffix(base, ".ddx"):
			indexes++
		case strings.HasPrefix(base, "agent."):
			logs++
		case f.Path == cfgPath:
			hasConfig = true
		}
	}
	var found []string
	if indexes > 0 {
		found = append(found, fmt.Sprintf("%d saved %s", indexes, plural(indexes, "index", "indexes")))
	}
	if logs > 0 {
		found = append(found, "the agent's log")
	}
	if hasConfig {
		found = append(found, "your settings")
	}
	if len(found) == 0 {
		found = append(found, "DriveDex's working files")
	}
	fmt.Fprintf(out, "Found %s (%s in all):\n", strings.Join(found, ", "), ui.HumanSize(size))
	fmt.Fprintln(out, ui.Dim("  "+indexDir))
	if cfgPath != "" {
		fmt.Fprintln(out, ui.Dim("  "+cfgPath))
	}
	if !purge && !askYesNo(out, in, "Delete them too? (Indexes can be rebuilt with 'drivedex index', but that takes a while. Default is to keep them.)") {
		fmt.Fprintln(out, "Kept.")
		return nil
	}
	cfgDir := ""
	if cfgPath != "" {
		cfgDir = filepath.Dir(cfgPath)
	}
	if err := install.RemoveOwnedData(files, indexDir, dataDir, cfgDir); err != nil {
		return userErr("couldn't delete everything: "+err.Error(), "close other DriveDex windows and try again", err)
	}
	fmt.Fprintln(out, ui.Green("Deleted."))
	return nil
}
