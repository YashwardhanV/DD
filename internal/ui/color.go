package ui

import (
	"os"
	"strings"
)

// colorOn is set once at startup by SetColor.
var colorOn bool

// SetColor turns ANSI colors on or off. It is called after flags are parsed.
// Colors are used only when enabled AND stdout is a real terminal AND the
// NO_COLOR convention (https://no-color.org) isn't set.
func SetColor(enabled bool) {
	colorOn = enabled && os.Getenv("NO_COLOR") == "" && IsTerminal(os.Stdout) && ansiSupported
}

// SetColorMode is SetColor with an override for the "color = always" setting:
// force turns colors on even when stdout is not a terminal (still only if the
// console can display them). enabled=false always wins.
func SetColorMode(enabled, force bool) {
	switch {
	case !enabled:
		colorOn = false
	case force:
		colorOn = ansiSupported
	default:
		SetColor(true)
	}
}

// IsTerminal reports whether f is an interactive console (not a pipe or file).
func IsTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func wrap(code, s string) string {
	if !colorOn || s == "" {
		return s
	}
	// Re-apply the style after any inner reset so nested styling keeps working.
	return "\x1b[" + code + "m" + strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m\x1b["+code+"m") + "\x1b[0m"
}

// Bold makes text bold (when colors are on).
func Bold(s string) string { return wrap("1", s) }

// Dim makes text faint (when colors are on).
func Dim(s string) string { return wrap("2", s) }

// Red, Green, Yellow color text (when colors are on).
func Red(s string) string    { return wrap("31", s) }
func Green(s string) string  { return wrap("32", s) }
func Yellow(s string) string { return wrap("33", s) }

// Highlight makes matched text stand out (bold yellow).
func Highlight(s string) string { return wrap("1;33", s) }
