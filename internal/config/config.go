// Package config reads DriveDex's settings file.
//
// The file is plain text (TOML) at %APPDATA%\DriveDex\config.toml. It is
// created, with every option explained in comments, the first time DriveDex
// runs; you can open it with `drivedex config edit`. Every option has a
// default, so a missing or partial file just works.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

// EnvPath overrides the config file location (tests, portable use).
const EnvPath = "DRIVEDEX_CONFIG"

// Workers is "auto" or a number of parallel workers. In the file it may be
// written either way: workers = "auto"  or  workers = 8.
type Workers string

// UnmarshalTOML accepts both a string and an integer.
func (w *Workers) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*w = Workers(strings.TrimSpace(x))
	case int64:
		*w = Workers(fmt.Sprint(x))
	default:
		return fmt.Errorf("workers must be \"auto\" or a number, not %T", v)
	}
	return nil
}

// Config holds every option. The toml tags are the names used in the file.
type Config struct {
	DefaultLimit  int      `toml:"default_limit"`
	ShowHidden    bool     `toml:"show_hidden"`
	Workers       Workers  `toml:"workers"`
	SkipDirs      []string `toml:"skip_dirs"`
	SkipDirsExtra []string `toml:"skip_dirs_extra"`
	StaleAfter    string   `toml:"stale_after"`
	Notifications bool     `toml:"notifications"`
	AutoIndex     []string `toml:"auto_index"`
	AdbPath       string   `toml:"adb_path"`
	PhoneRoot     string   `toml:"phone_root"`
	Color         string   `toml:"color"`
}

// Defaults returns the built-in settings (BUILD.md stage 11).
func Defaults() Config {
	return Config{
		DefaultLimit:  50,
		ShowHidden:    true,
		Workers:       "auto",
		SkipDirs:      []string{"System Volume Information", "$RECYCLE.BIN", "$Recycle.Bin"},
		SkipDirsExtra: []string{},
		StaleAfter:    "24h",
		Notifications: true,
		AutoIndex:     []string{},
		AdbPath:       "",
		PhoneRoot:     "/sdcard/",
		Color:         "auto",
	}
}

// Template is the file written on first run: the defaults, explained.
const Template = `# DriveDex configuration
# Change a value, save, and the next DriveDex command uses it.
# Delete a line to go back to its default.

# How many results 'drivedex search' shows (0 = all).
default_limit   = 50

# Show hidden and system files in results.
show_hidden     = true

# Files/folders read or copied at once: "auto" (picked from the drive type) or a number.
workers         = "auto"          # or a number, e.g. 8

# Folder names that are never indexed (any depth, not case sensitive).
# A change applies the next time a drive is scanned in full (for an NTFS drive in
# turbo mode, run: drivedex index E: --full).
skip_dirs       = ["System Volume Information", "$RECYCLE.BIN", "$Recycle.Bin"]
# Your own additions to the list above, e.g. ["node_modules", ".git"].
skip_dirs_extra = []

# 'search' suggests re-indexing a connected drive when its index is older than this.
stale_after     = "24h"

# Windows pop-up when the background agent has indexed a drive.
# (The agent reads this file when it starts: after changing notifications or
# auto_index, run: drivedex agent stop   then   drivedex agent start)
notifications   = true

# The background agent indexes every external drive, phone and portable device
# by itself. List internal drives here to have it keep those indexed too, e.g. ["D:"].
auto_index      = []

# Full path of adb.exe, if DriveDex can't find it by itself.
adb_path        = ""

# Which folder of the phone is indexed.
phone_root      = "/sdcard/"

# Colored output: "auto", "always" or "never". (The NO_COLOR environment variable is also respected.)
color           = "auto"
`

// Path returns where the config file lives.
func Path() (string, error) {
	if p := os.Getenv(EnvPath); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the settings folder: %w", err)
	}
	return filepath.Join(dir, "DriveDex", "config.toml"), nil
}

// Error is a problem with the config file, worded for the person who has to fix it.
type Error struct {
	Path string
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Path, e.Msg) }

// Load reads the config file at path. If the file doesn't exist it is created
// from Template (best effort: a read-only folder is not an error) and the
// defaults are returned. A broken file yields an *Error naming the line.
func Load(path string) (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	// "Not there" includes "a parent of the path is a file, not a folder".
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr == nil {
			os.WriteFile(path, []byte(Template), 0o644)
		}
		return cfg, nil
	}
	if err != nil {
		return cfg, &Error{Path: path, Msg: "can't read the file: " + err.Error()}
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return Defaults(), &Error{Path: path, Msg: fmt.Sprintf("line %d: %s", pe.Position.Line, firstLine(pe.Message))}
		}
		return Defaults(), &Error{Path: path, Msg: firstLine(err.Error())}
	}
	if err := cfg.Validate(); err != nil {
		return Defaults(), &Error{Path: path, Msg: err.Error()}
	}
	cfg.normalize()
	return cfg, nil
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// Validate checks every value, with messages that say what to write instead.
func (c Config) Validate() error {
	if c.DefaultLimit < 0 {
		return errors.New("default_limit must be 0 or more (0 shows every result)")
	}
	if _, _, err := ParseWorkers(string(c.Workers)); err != nil {
		return err
	}
	if _, err := c.StaleDuration(); err != nil {
		return err
	}
	switch strings.ToLower(c.Color) {
	case "auto", "always", "never":
	default:
		return fmt.Errorf("color must be \"auto\", \"always\" or \"never\", not %q", c.Color)
	}
	for _, a := range c.AutoIndex {
		if !validLetter(a) {
			return fmt.Errorf("auto_index entries must be drive letters like \"D:\", not %q", a)
		}
	}
	if pr := strings.TrimSpace(c.PhoneRoot); pr != "" && !strings.HasPrefix(pr, "/") {
		return fmt.Errorf("phone_root must start with / (for example \"/sdcard/\"), not %q", c.PhoneRoot)
	}
	return nil
}

func validLetter(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) == 1 {
		s += ":"
	}
	s = strings.TrimRight(s, `\/`)
	return len(s) == 2 && s[1] == ':' && (s[0] >= 'A' && s[0] <= 'Z' || s[0] >= 'a' && s[0] <= 'z')
}

// normalize tidies values after validation.
func (c *Config) normalize() {
	c.Color = strings.ToLower(strings.TrimSpace(c.Color))
	if strings.TrimSpace(c.PhoneRoot) == "" {
		c.PhoneRoot = Defaults().PhoneRoot
	}
	for i, a := range c.AutoIndex {
		a = strings.TrimSpace(a)
		if len(a) == 1 {
			a += ":"
		}
		c.AutoIndex[i] = strings.ToUpper(strings.TrimRight(a, `\/`))
	}
}

// ParseWorkers turns "auto" or "8" into (8, false) / (0, true).
func ParseWorkers(s string) (n int, auto bool, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "auto" {
		return 0, true, nil
	}
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || fmt.Sprint(n) != s || n < 1 || n > 256 {
		return 0, false, fmt.Errorf("workers must be \"auto\" or a number from 1 to 256, not %q", s)
	}
	return n, false, nil
}

// StaleDuration parses stale_after ("24h", "90m", "7d" - days are allowed too).
func (c Config) StaleDuration() (time.Duration, error) {
	s := strings.TrimSpace(c.StaleAfter)
	if s == "" {
		return 24 * time.Hour, nil
	}
	if strings.HasSuffix(s, "d") {
		var days float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%g", &days); err == nil && days >= 0 {
			// More days than a Duration can hold (about 106,000) would wrap
			// around to a negative age; such a setting means "never stale".
			if d := days * float64(24*time.Hour); d < math.MaxInt64 {
				return time.Duration(d), nil
			}
			return time.Duration(math.MaxInt64), nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("stale_after must be a time like \"24h\", \"90m\" or \"7d\", not %q", c.StaleAfter)
	}
	return d, nil
}

// AllSkipDirs is skip_dirs followed by skip_dirs_extra.
func (c Config) AllSkipDirs() []string {
	out := make([]string, 0, len(c.SkipDirs)+len(c.SkipDirsExtra))
	out = append(out, c.SkipDirs...)
	return append(out, c.SkipDirsExtra...)
}

// Marshal renders the config as TOML (used by `config show`).
func (c Config) Marshal() (string, error) {
	var sb strings.Builder
	if err := toml.NewEncoder(&sb).Encode(c); err != nil {
		return "", err
	}
	return sb.String(), nil
}
