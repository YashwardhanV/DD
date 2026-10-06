// Package cli holds every DriveDex command (scan, index, search, copy, ...).
//
// Each command lives in its own file and is registered in NewRootCmd. The
// commands themselves stay thin: they parse flags, call into the real
// packages (scan, index, copier, ...) and print friendly output.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"
)

// Global flags shared by every command. They are filled in by cobra before a
// command runs.
var (
	flagDebug   bool
	flagNoColor bool
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
	root := NewRootCmd(version)
	if err := root.ExecuteContext(ctx); err != nil {
		printError(err)
		return 1
	}
	return 0
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
	}

	root.PersistentFlags().BoolVar(&flagDebug, "debug", false, "show technical details when something fails")
	root.PersistentFlags().BoolVar(&flagNoColor, "no-color", false, "turn off colored output")

	root.AddCommand(newVersionCmd(version))
	return root
}

// newVersionCmd implements `drivedex version`.
func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print the DriveDex version",
		Long:    "Prints the version, the operating system / CPU it was built for, and the Go version.",
		Example: `  drivedex version`,
		Args:    cobra.NoArgs,
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
			fmt.Fprintf(os.Stderr, "  Fix: %s\n", ue.Hint)
		}
		if flagDebug && ue.Err != nil {
			fmt.Fprintf(os.Stderr, "  Details: %v\n", ue.Err)
		}
		return
	}
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
}
