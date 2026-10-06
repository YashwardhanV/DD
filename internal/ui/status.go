package ui

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"unicode/utf8"
)

// envWidth parses a COLUMNS-style value, defaulting to 80.
func envWidth(s string) int {
	if n, err := strconv.Atoi(s); err == nil && n > 20 {
		return n
	}
	return 80
}

// StatusLine shows one line of text that is rewritten in place (using "\r"),
// like "Indexing E:\ ... 182,334 entries (41,200/s)".
//
// It only does anything when the output is an interactive terminal, so piping
// DriveDex into a file never fills it with progress noise.
type StatusLine struct {
	w       io.Writer
	enabled bool
	lastLen int
}

// NewStatusLine returns a status line writing to f (normally os.Stderr).
func NewStatusLine(f *os.File) *StatusLine {
	if f == nil {
		return &StatusLine{} // permanently disabled
	}
	return &StatusLine{w: f, enabled: IsTerminal(f)}
}

// Enabled reports whether updates are shown at all.
func (s *StatusLine) Enabled() bool { return s.enabled }

// Update replaces the current line with text. Text wider than the terminal is
// cut (a wrapped line can't be overwritten with "\r").
func (s *StatusLine) Update(text string) {
	if !s.enabled {
		return
	}
	if max := termWidth() - 1; utf8.RuneCountInString(text) > max {
		r := []rune(text)
		text = string(r[:max-1]) + "…"
	}
	n := utf8.RuneCountInString(text)
	pad := ""
	if s.lastLen > n {
		pad = fmt.Sprintf("%*s", s.lastLen-n, "") // blank out leftovers of a longer previous line
	}
	fmt.Fprintf(s.w, "\r%s%s", text, pad)
	if s.lastLen > n {
		fmt.Fprintf(s.w, "\r%s", text) // put the cursor back after the text
	}
	s.lastLen = n
}

// Clear erases the line so normal output can continue cleanly below.
func (s *StatusLine) Clear() {
	if !s.enabled || s.lastLen == 0 {
		return
	}
	fmt.Fprintf(s.w, "\r%*s\r", s.lastLen, "")
	s.lastLen = 0
}
