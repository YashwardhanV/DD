package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/shellint"
	"github.com/yashwardhanv/dd/internal/ui"
)

// Hooks for the Windows registry and the running program's path (replaced in tests).
var (
	newShellRegistry = func() shellint.Registry { return shellint.WindowsRegistry{} }
	shellExe         = func() (string, error) {
		p, err := os.Executable()
		if err != nil {
			return "", err
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		return filepath.Abs(p)
	}
)

const windows11Note = `On Windows 11 the entries appear under "Show more options" (or hold Shift while right-clicking): the new compact menu can only be extended by packaged apps.`

func newShellCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell",
		Short: "Add DriveDex to the Explorer right-click menu",
		Long: `Adds "Search with DriveDex" to the right-click menu of folders, drives and the
empty space inside a folder, and "Index with DriveDex" to drives. Everything is
stored for your user only (HKEY_CURRENT_USER), so no administrator rights are
needed, and 'drivedex shell uninstall' removes exactly what was added.`,
		Example: `  drivedex shell install
  drivedex shell status
  drivedex shell uninstall`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	var yes bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Add the right-click menu entries",
		Long: `Adds the DriveDex entries to Explorer's right-click menu. It first lists exactly
which registry keys it will create (all under your own user) and asks before
changing anything.`,
		Example: `  drivedex shell install
  drivedex shell install --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShellInstall(cmd.OutOrStdout(), cmd.InOrStdin(), yes)
		},
	}
	install.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	cmd.AddCommand(install,
		&cobra.Command{
			Use: "uninstall", Short: "Remove the right-click menu entries", Args: cobra.NoArgs,
			Long: `Removes the right-click entries DriveDex added, and only those. Keys that
belong to something else are never touched.`,
			Example: `  drivedex shell uninstall
  drivedex shell status`,
			RunE: func(cmd *cobra.Command, args []string) error { return runShellUninstall(cmd.OutOrStdout()) },
		},
		&cobra.Command{
			Use: "status", Short: "Show which right-click entries are installed", Args: cobra.NoArgs,
			Long: `Lists each right-click entry and whether it is installed, missing, or points
at a drivedex.exe somewhere else (because the program was moved).`,
			Example: `  drivedex shell status
  drivedex shell install`,
			RunE: func(cmd *cobra.Command, args []string) error { return runShellStatus(cmd.OutOrStdout()) },
		},
	)
	return cmd
}

func shellUnsupported(err error) error {
	if errors.Is(err, errors.ErrUnsupported) {
		return userErr("the Explorer menu only works on Windows", "run DriveDex on your Windows PC", err)
	}
	return nil
}

func runShellInstall(out io.Writer, in io.Reader, yes bool) error {
	exe, err := shellExe()
	if err != nil {
		return userErr("couldn't find where drivedex.exe is", "run with --debug for details", err)
	}
	return installShellEntries(out, in, yes, exe)
}

// installShellEntries adds the right-click entries so that they run exe. (`drivedex
// install` calls it with the copy it just installed, not with the program that is running.)
func installShellEntries(out io.Writer, in io.Reader, yes bool, exe string) error {
	if !yes {
		fmt.Fprintln(out, `This adds these entries to the right-click menu (for your user only, under HKEY_CURRENT_USER\`+shellint.ClassesRoot+`):`)
		for _, e := range shellint.Entries(exe) {
			fmt.Fprintf(out, "  %-22s %s\n", e.Title, strings.TrimPrefix(e.Key, shellint.ClassesRoot+`\`))
		}
		fmt.Fprintln(out, "They will run:", exe)
		fmt.Fprint(out, "Continue? [y/N] ")
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Cancelled. Nothing was changed.")
			return nil
		}
	}
	err := shellint.Install(newShellRegistry(), exe)
	if shellUnsupported(err) != nil {
		return shellUnsupported(err)
	}
	if err != nil {
		return userErr("couldn't add the right-click menu entries", "run with --debug for details", err)
	}
	fmt.Fprintln(out, "Done. Right-click a folder or drive to try it.")
	fmt.Fprintln(out, ui.Dim(windows11Note))
	fmt.Fprintln(out, ui.Dim("If you move drivedex.exe, run 'drivedex shell install' again from its new place."))
	return nil
}

func runShellUninstall(out io.Writer) error {
	results, err := shellint.Uninstall(newShellRegistry())
	if shellUnsupported(err) != nil {
		return shellUnsupported(err)
	}
	if err != nil {
		return userErr("couldn't remove the right-click menu entries", "run with --debug for details", err)
	}
	removed := 0
	for _, r := range results {
		switch {
		case r.Removed:
			removed++
		case r.Skipped != "":
			fmt.Fprintf(out, "%s left %s alone: %s\n", ui.Yellow("note:"), strings.TrimPrefix(r.Key, shellint.ClassesRoot+`\`), r.Skipped)
		}
	}
	if removed == 0 {
		fmt.Fprintln(out, "No DriveDex right-click entries were installed. Nothing to remove.")
		return nil
	}
	fmt.Fprintf(out, "Removed %d right-click menu %s.\n", removed, plural(removed, "entry", "entries"))
	return nil
}

func runShellStatus(out io.Writer) error {
	exe, err := shellExe()
	if err != nil {
		return userErr("couldn't find where drivedex.exe is", "run with --debug for details", err)
	}
	sts, err := shellint.Status(newShellRegistry(), exe)
	if shellUnsupported(err) != nil {
		return shellUnsupported(err)
	}
	if err != nil {
		return userErr("couldn't read the right-click menu entries", "run with --debug for details", err)
	}
	bad := 0
	for _, s := range sts {
		label := ui.Green("installed")
		switch s.State {
		case shellint.Missing:
			label, bad = ui.Yellow("missing  "), bad+1
		case shellint.Stale:
			label, bad = ui.Yellow("points to a different drivedex.exe"), bad+1
		}
		fmt.Fprintf(out, "  %-10s %-22s %s\n", label, s.Entry.Title, strings.TrimPrefix(s.Entry.Key, shellint.ClassesRoot+`\`))
	}
	switch {
	case bad == len(sts):
		fmt.Fprintln(out, "\nNot installed. Add it with: drivedex shell install")
	case bad > 0:
		fmt.Fprintln(out, "\nSome entries are missing or out of date. Fix with: drivedex shell install")
	default:
		fmt.Fprintln(out, "\nAll entries are installed. "+windows11Note)
	}
	return nil
}
