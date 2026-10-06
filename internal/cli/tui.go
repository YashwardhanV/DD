package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/tui"
	"github.com/yashwardhanv/dd/internal/ui"
)

// runTUI shows the full-screen search. A variable so tests can capture the
// configuration instead of taking over a terminal.
var runTUI = tui.Run

// tuiFlags are the options of `drivedex tui` (and of plain `drivedex`).
type tuiFlags struct {
	drive, scope, query string
}

func addTUIFlags(cmd *cobra.Command, f *tuiFlags) {
	cmd.Flags().StringVar(&f.drive, "drive", "", "only search the drive currently at this letter, e.g. E:")
	cmd.Flags().StringVar(&f.scope, "scope", "", "only show results inside this folder")
	cmd.Flags().StringVar(&f.query, "query", "", "start with this search text")
}

const tuiLong = `Opens a full-screen search. Type, and results appear as you type - from every
saved index, including drives and phones that are not plugged in (shown dimmed).

  Enter       open the selected file (or folder)
  Ctrl+O      show it in its folder in Explorer
  Ctrl+Y      copy its full path to the clipboard
  Ctrl+P      copy a phone file to Downloads\DriveDex
  Up/Down, PageUp/PageDown   move
  Esc         quit

Running 'drivedex' with no arguments opens this screen too.`

// newTUICmd implements `drivedex tui`.
func newTUICmd() *cobra.Command {
	var f tuiFlags
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Interactive type-as-you-search screen",
		Long:  tuiLong,
		Example: `  drivedex tui
  drivedex tui --drive E:
  drivedex tui --scope E:\Photos --query vacation`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return startTUI(cmd.Context(), f) },
	}
	addTUIFlags(cmd, &f)
	return cmd
}

// startTUI builds the screen's configuration and runs it.
func startTUI(ctx context.Context, f tuiFlags) error {
	if !ui.IsTerminal(os.Stdin) || !ui.IsTerminal(os.Stdout) {
		return userErr("the interactive screen needs a terminal", "run it in PowerShell or Windows Terminal; for scripts use 'drivedex search <words>'", nil)
	}
	cfg, err := buildTUIConfig(ctx, f)
	if err != nil {
		return err
	}
	if err := runTUI(cfg); err != nil {
		return userErr("the interactive screen stopped unexpectedly", "try 'drivedex search <words>' instead; run with --debug for details", err)
	}
	return nil
}

func buildTUIConfig(ctx context.Context, f tuiFlags) (tui.Config, error) {
	store, err := openStore()
	if err != nil {
		return tui.Config{}, err
	}
	infos, err := store.List()
	if err != nil {
		return tui.Config{}, userErr("couldn't read the saved indexes", "check the folder exists and is readable: "+store.Dir, err)
	}

	cfg := tui.Config{
		Count: len(infos),
		Query: f.query,
		Load: func() (tui.Loaded, error) {
			l, err := store.LoadAll()
			return tui.Loaded{Indexes: l.Indexes, Warnings: l.Warnings}, err
		},
		Env:        func(ixs []*index.Index) index.Env { return currentEnv(hasPhone(ixs)) },
		Actions:    tuiActions(),
		HideHidden: !activeConfig.ShowHidden,
	}

	if f.drive != "" {
		id, err := sourceIDForLetter(f.drive)
		if err != nil {
			return tui.Config{}, err
		}
		cfg.DriveSourceID = id
	}
	if f.scope != "" {
		abs, err := resolveRoot(f.scope)
		if err != nil {
			return tui.Config{}, err
		}
		cfg.ScopePath = abs
		cfg.IndexScope = func() error { return indexForScope(ctx, abs) }
	}
	return cfg, nil
}

// indexForScope indexes what the --scope folder lives on: the whole drive if
// it is on a drive we can identify, otherwise just that folder. It runs while
// the full-screen UI is up, so it prints nothing.
func indexForScope(ctx context.Context, abs string) error {
	opts := scanOptions{quiet: true, workers: string(activeConfig.Workers)}
	if v, err := volumeForPath(abs); err == nil && v.Ready && v.Serial != 0 {
		return indexRoot(ctx, io.Discard, v.Root, volumeHeader(v), opts)
	}
	return indexRoot(ctx, io.Discard, abs, index.FolderHeader(abs), opts)
}

// tuiActions are the real actions for this OS, plus copying a phone file with adb.
func tuiActions() tui.Actions {
	a := tui.DefaultActions()
	a.PullPhone = func(serial, remote string) (string, error) {
		adb, err := openADB()
		if err != nil {
			return "", err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding your home folder: %w", err)
		}
		dir := filepath.Join(home, "Downloads", "DriveDex")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("creating %s: %w", dir, err)
		}
		if err := adb.Pull(context.Background(), serial, []string{remote}, dir); err != nil {
			return "", err
		}
		return filepath.Join(dir, path.Base(remote)), nil
	}
	return a
}
