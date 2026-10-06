package cli

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/config"
)

// openEditor opens a text file for the user. A variable so tests don't start Notepad.
var openEditor = func(path string) error {
	if runtime.GOOS != "windows" {
		return errors.ErrUnsupported
	}
	// Start Notepad and carry on: the terminal is free again straight away.
	cmd := exec.Command("notepad.exe", path)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

const configLong = `DriveDex's settings live in one plain-text file (config.toml). It is created
with every option explained the first time DriveDex runs. Change a value, save,
and the next DriveDex command uses it.`

// newConfigCmd implements `drivedex config path|show|edit`.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or edit DriveDex's settings file",
		Long:  configLong,
		Example: `  drivedex config path
  drivedex config show
  drivedex config edit`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print where the settings file is",
		Long: `Prints the full path of config.toml, so you can open it or back it up.
(Set the DRIVEDEX_CONFIG environment variable to use a file somewhere else.)`,
		Example: `  drivedex config path
  notepad (drivedex config path)   (PowerShell: open it yourself)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := configPath()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	}, &cobra.Command{
		Use:   "show",
		Short: "Print the settings DriveDex is using",
		Long: `Prints every setting with its current value - the file's values, plus the
default for anything the file leaves out.`,
		Example: `  drivedex config show
  drivedex config path`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runConfigShow(cmd.OutOrStdout()) },
	}, &cobra.Command{
		Use:   "edit",
		Short: "Open the settings file in Notepad",
		Long: `Opens config.toml in Notepad (creating it first if it doesn't exist yet).
Save and close it, then run any DriveDex command to use the new settings.`,
		Example: `  drivedex config edit
  drivedex config show   (check your changes)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runConfigEdit(cmd.OutOrStdout()) },
	})
	return cmd
}

func configPath() (string, error) {
	p, err := config.Path()
	if err != nil {
		return "", userErr("can't find the settings folder", "set the DRIVEDEX_CONFIG environment variable to the full path of a config.toml file", err)
	}
	return p, nil
}

func runConfigShow(out io.Writer) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(p) // also creates the file the first time
	if err != nil {
		return userErr("the settings file has a problem: "+err.Error(),
			"fix it with 'drivedex config edit' (or delete it to start again from the defaults)", err)
	}
	text, err := cfg.Marshal()
	if err != nil {
		return userErr("couldn't show the settings", "run with --debug for details", err)
	}
	fmt.Fprintf(out, "# settings from %s\n%s", p, text)
	return nil
}

func runConfigEdit(out io.Writer) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	// Make sure there is a file to edit. A broken one is exactly what the user
	// may be here to fix, so a load error is not a reason to stop.
	config.Load(p)
	if err := openEditor(p); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			return userErr("opening Notepad only works on Windows", "open this file in any text editor: "+p, err)
		}
		return userErr("couldn't open Notepad", "open this file yourself: "+p, err)
	}
	fmt.Fprintln(out, "Opened", p, "- save the file, then run any DriveDex command to use the changes.")
	return nil
}
