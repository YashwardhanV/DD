package cli

import (
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/config"
	"github.com/yashwardhanv/dd/internal/ui"
)

// activeConfig is the settings in force for the current command. It is loaded
// from config.toml before each command runs (see root.go); until then, and
// whenever the file can't be used, it holds the defaults.
var activeConfig = config.Defaults()

// loadActiveConfig reads config.toml (creating it with the defaults on the
// first run). A broken file is reported as a friendly error naming the line.
func loadActiveConfig() error {
	path, err := config.Path()
	if err != nil {
		activeConfig = config.Defaults()
		return nil // no settings folder: just use the defaults
	}
	cfg, err := config.Load(path)
	activeConfig = cfg
	if err != nil {
		return userErr("the settings file has a problem: "+err.Error(),
			"fix it with 'drivedex config edit' (or delete it to start again from the defaults)", err)
	}
	return nil
}

// commandsWithoutConfig are the commands that must keep working even when the
// settings file is broken (they are how you find and fix it).
func commandsWithoutConfig(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "config", "doctor", "version", "help", "completion", "__complete", "install", "uninstall":
			return true
		}
	}
	return false
}

// applyColor sets the output color mode from --no-color and the color setting.
func applyColor() {
	switch {
	case flagNoColor || activeConfig.Color == "never":
		ui.SetColorMode(false, false)
	case activeConfig.Color == "always":
		ui.SetColorMode(true, true)
	default:
		ui.SetColorMode(true, false)
	}
}

// effectiveWorkers returns the --workers value to use: the flag if the user
// typed it, otherwise the workers setting.
func effectiveWorkers(cmd *cobra.Command, flagValue string) string {
	if cmd.Flags().Changed("workers") {
		return flagValue
	}
	return string(activeConfig.Workers)
}

// activeSkipDirs is skip_dirs followed by skip_dirs_extra.
func activeSkipDirs() []string { return activeConfig.AllSkipDirs() }

// activeStaleAfter is how old an index may get before search suggests refreshing it.
func activeStaleAfter() time.Duration {
	d, err := activeConfig.StaleDuration()
	if err != nil {
		return 24 * time.Hour
	}
	return d
}

// activeAdbPath is the configured path of adb.exe ("" = look for it).
func activeAdbPath() string { return strings.TrimSpace(activeConfig.AdbPath) }

// activePhoneRoot is the phone folder that gets indexed, with a trailing slash ("/sdcard/").
func activePhoneRoot() string {
	root := strings.TrimSpace(activeConfig.PhoneRoot)
	if root == "" {
		root = "/sdcard/"
	}
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	return root
}

// activePhoneRootPath is the same folder without the trailing slash, as stored
// in an index header ("/sdcard").
func activePhoneRootPath() string {
	if root := strings.TrimRight(activePhoneRoot(), "/"); root != "" {
		return root
	}
	return "/"
}
