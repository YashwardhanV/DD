// Package cli holds every DriveDex command (scan, index, search, copy, ...).
//
// Each command lives in its own file and is registered in NewRootCmd. The
// commands themselves stay thin: they parse flags, call into the real
// packages (scan, index, copier, ...) and print friendly output.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/config"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// Global flags shared by every command. They are filled in by cobra before a
// command runs.
var (
	flagDebug   bool
	flagNoColor bool
	flagPause   bool
	rootTUI     tuiFlags // flags of plain `drivedex` (same as `drivedex tui`)
)

// UserError is an error that is safe to show to the owner as-is: one friendly
// line plus an optional hint explaining how to fix it.
type UserError struct {
	Msg  string
	Hint string
	Err  error // underlying cause, shown only with --debug
}

func (e *UserError) Error() string { return e.Msg }
func (e *UserError) Unwrap() error { return e.Err }

// userErr is a small helper to build a UserError.
func userErr(msg, hint string, cause error) error {
	return &UserError{Msg: msg, Hint: hint, Err: cause}
}

// Execute builds the command tree, runs it, and returns the process exit code
// (0 = success, 1 = error).
func Execute(ctx context.Context, version string) int {
	// Make the Windows console show colors and Unicode properly (undone on exit).
	restore := ui.SetupConsole()
	defer restore()
	// Stop Windows popping up "There is no disk in the drive" for empty card readers.
	volume.InitProcess()

	return executeRoot(ctx, NewRootCmd(version), os.Stdin, os.Stdout)
}

// executeRoot runs the command tree and returns the exit code. If --pause was
// given it then waits for Enter, so a window opened by the Explorer right-click
// menu doesn't vanish before the output can be read.
func executeRoot(ctx context.Context, root *cobra.Command, in io.Reader, out io.Writer) int {
	code := 0
	if err := root.ExecuteContext(ctx); err != nil {
		printError(err)
		code = 1
	}
	if flagPause {
		waitForEnter(in, out)
	}
	return code
}

// waitForEnter prints a prompt and waits for the user to press Enter.
func waitForEnter(in io.Reader, out io.Writer) {
	fmt.Fprint(out, "\nPress Enter to close…")
	bufio.NewReader(in).ReadString('\n')
}

// NewRootCmd creates the `drivedex` command with all subcommands attached.
func NewRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "drivedex",
		Short: "Fast search and copy for external drives, pen drives and Android phones",
		Long: `DriveDex indexes your external drives and phones once, then searches the
saved index instantly - even for drives that are not plugged in.`,
		// We print errors ourselves (see printError) so they are friendly.
		SilenceErrors: true,
		SilenceUsage:  true,
		// `drivedex` with no arguments opens the interactive search.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return startTUI(cmd.Context(), rootTUI) },
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Settings first (they decide colors, limits, skip lists ...). The commands
			// that help you fix a broken settings file still work with the defaults.
			if commandsWithoutConfig(cmd) {
				activeConfig = config.Defaults()
				applyColor()
				return nil
			}
			err := loadActiveConfig()
			applyColor()
			return err
		},
	}

	addTUIFlags(root, &rootTUI)
	root.PersistentFlags().BoolVar(&flagDebug, "debug", false, "show technical details when something fails")
	root.PersistentFlags().BoolVar(&flagNoColor, "no-color", false, "turn off colored output")
	root.PersistentFlags().BoolVar(&flagPause, "pause", false, "wait for Enter before closing the window (used by the right-click menu)")

	root.AddCommand(newVersionCmd(version))
	root.AddCommand(newScanCmd())
	root.AddCommand(newIndexCmd())
	root.AddCommand(newSearchCmd())
	root.AddCommand(newDrivesCmd())
	root.AddCommand(newPortsCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newForgetCmd())
	root.AddCommand(newBenchScanCmd())
	root.AddCommand(newJournalCmd())
	root.AddCommand(newCopyCmd())
	root.AddCommand(newTUICmd())
	root.AddCommand(newAgentCmd())
	root.AddCommand(newStartupCmd())
	root.AddCommand(newShellCmd())
	root.AddCommand(newDoctorCmd(version))
	root.AddCommand(newConfigCmd())
	root.AddCommand(newInstallCmd(version))
	root.AddCommand(newUninstallCmd())
	return root
}

// newVersionCmd implements `drivedex version`.
func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the DriveDex version",
		Long: `Prints the DriveDex version, the operating system and CPU it was built for,
and the Go version it was built with. Include this when reporting a problem.`,
		Example: `  drivedex version
  drivedex doctor`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "drivedex %s (%s/%s, %s)\n",
				version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		},
	}
}

// printError shows an error to the user: one friendly line plus a hint.
// With --debug the full wrapped error chain is shown as well.
func printError(err error) {
	var ue *UserError
	if errors.As(err, &ue) {
		fmt.Fprintf(os.Stderr, "Error: %s\n", ue.Msg)
		if ue.Hint != "" {
			// Multi-line hints (such as setup steps) keep their lines aligned under "Fix:".
			fmt.Fprintf(os.Stderr, "  Fix: %s\n", strings.ReplaceAll(ue.Hint, "\n", "\n       "))
		}
		if flagDebug && ue.Err != nil {
			fmt.Fprintf(os.Stderr, "  Details: %v\n", ue.Err)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
}
