package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/yashwardhanv/dd/internal/ui"
)

var (
	boldStyle      = lipgloss.NewStyle().Bold(true)
	dimStyle       = lipgloss.NewStyle().Faint(true)
	highlightStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3")) // yellow
	warnStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	errStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	okStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
)

const helpText = "↑↓ move · Enter open · Ctrl+O show in folder · Ctrl+Y copy path · Ctrl+P get from phone · Esc quit"

// View draws the screen.
func (m Model) View() string {
	switch m.state {
	case stateLoading:
		n := m.cfg.Count
		what := "indexes"
		if n == 1 {
			what = "index"
		}
		if n > 0 {
			return fmt.Sprintf("\n  %s Loading %d %s…\n", m.spin.View(), n, what)
		}
		return fmt.Sprintf("\n  %s Loading indexes…\n", m.spin.View())
	case stateIndexing:
		return fmt.Sprintf("\n  %s Indexing %s … this takes a moment.\n", m.spin.View(), m.cfg.ScopePath)
	case stateEmpty:
		return "\n  Nothing indexed yet.\n\n  Index a drive first, for example:  drivedex index E:\\\n\n  " + dimStyle.Render("Esc to quit") + "\n"
	case stateScope:
		msg := "This drive isn't indexed yet."
		keys := "Esc to cancel"
		if m.cfg.IndexScope != nil {
			msg += " Press I to index now (takes a moment), Esc to cancel."
			keys = ""
		}
		return "\n  " + msg + "\n\n  " + dimStyle.Render(m.cfg.ScopePath+"  "+keys) + "\n"
	case stateError:
		return "\n  " + errStyle.Render("Error: "+m.errText) + "\n\n  " + dimStyle.Render("Esc to quit") + "\n"
	}

	w := max(m.width, 20)
	rule := " " + dimStyle.Render(strings.Repeat("─", max(w-2, 10)))

	var b strings.Builder
	b.WriteString(" 🔍 " + m.input.View() + "\n")
	b.WriteString(rule + "\n")
	b.WriteString(m.renderRows(w))
	b.WriteString(rule + "\n")
	foot, footStyle := m.footerLine()
	b.WriteString(" " + footStyle.Render(runewidth.Truncate(foot, w-2, "…")) + "\n")
	b.WriteString(" " + dimStyle.Render(runewidth.Truncate(helpText, w-2, "…")))
	return b.String()
}

// footerLine is the status message, or the match count (plain text + its style,
// so the caller can cut it to the screen width before colouring it).
func (m Model) footerLine() (string, lipgloss.Style) {
	if m.status != "" {
		style := warnStyle
		if m.status == "Copied!" || strings.HasPrefix(m.status, "Saved to") {
			style = okStyle
		}
		return m.status, style
	}
	offline := 0
	seen := map[string]bool{}
	for _, ix := range m.indexes {
		if !m.env.Online(&ix.Header) && !seen[ix.Header.SourceID] {
			seen[ix.Header.SourceID] = true
			offline++
		}
	}
	if strings.TrimSpace(m.input.Value()) == "" {
		entries := 0
		for _, ix := range m.indexes {
			entries += ix.LiveLen()
		}
		return fmt.Sprintf("Type to search %s entries in %s", ui.Thousands(int64(entries)), sourcesText(len(m.indexes), offline)), lipgloss.NewStyle()
	}
	if m.queryErr != "" {
		return m.queryErr, warnStyle
	}
	if m.noMeta > 0 {
		return fmt.Sprintf("%s matches · some NTFS entries have no size/date: run 'drivedex index E: --with-metadata'", ui.Thousands(int64(m.total))), warnStyle
	}
	return fmt.Sprintf("%s matches · %s · %s", ui.Thousands(int64(m.total)), ui.FormatMillis(m.took), sourcesText(m.covered, offline)), lipgloss.NewStyle()
}

// layout holds the column widths for one screen width.
type layout struct {
	nameW, dirW int
	showSize    bool
	showDate    bool
}

// metaWidths: the size column and the date column (each preceded by two spaces).
const (
	sizeW = 9
	dateW = 16
)

// computeLayout decides which columns fit. Wide screens show everything; narrow
// ones drop the date, then the size, then shrink the folder - but a line never
// exceeds the screen width.
func computeLayout(width, longestName int) layout {
	l := layout{showSize: width >= 50, showDate: width >= 70}
	overhead := 1 + 2 // left margin and the selection marker
	if l.showSize {
		overhead += 2 + sizeW
	}
	if l.showDate {
		overhead += 2 + dateW
	}
	room := max(width-overhead-2, 4) // 2 = gap between name and folder
	l.nameW = min(max(longestName, 8), max(room*55/100, 4))
	l.nameW = min(l.nameW, room)
	l.dirW = max(room-l.nameW, 0)
	return l
}

// renderRows draws the visible result lines (always exactly visibleRows() lines,
// so the footer stays put).
func (m Model) renderRows(width int) string {
	visible := m.visibleRows()
	end := min(len(m.rows), m.offset+visible)

	longest := 0
	for i := m.offset; i < end; i++ {
		longest = max(longest, runewidth.StringWidth(m.rows[i].Name))
	}
	lay := computeLayout(width, longest)

	var b strings.Builder
	for i := m.offset; i < end; i++ {
		b.WriteString(m.renderRow(m.rows[i], i == m.cursor, lay))
		b.WriteByte('\n')
	}
	// Pad with blank lines (or a hint) so the screen never jumps.
	for i := end - m.offset; i < visible; i++ {
		if i == 0 && len(m.rows) == 0 && strings.TrimSpace(m.input.Value()) != "" && m.searched {
			b.WriteString(" " + dimStyle.Render("  No matches.") + "\n")
			continue
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func (m Model) renderRow(r Row, selected bool, lay layout) string {
	marker := "  "
	if selected {
		marker = "▶ "
	}
	name := runewidth.Truncate(r.Name, lay.nameW, "…")
	namePad := strings.Repeat(" ", max(0, lay.nameW-runewidth.StringWidth(name)))

	base := boldStyle
	if r.Offline {
		base = dimStyle
	}
	line := " " + marker + highlight(name, m.terms, base, highlightStyle) + namePad

	if lay.dirW > 0 {
		d := truncateLeft(r.Dir, lay.dirW)
		line += "  " + dimStyle.Render(d) + strings.Repeat(" ", max(0, lay.dirW-runewidth.StringWidth(d)))
	}

	size := "<dir>"
	date := ui.FormatTime(r.MTime)
	if !r.IsDir {
		size = ui.HumanSize(r.Size)
	}
	if r.NoMeta || (!r.IsDir && r.Size < 0) {
		size, date = "?", "-"
	}
	if lay.showSize {
		line += "  " + dimStyle.Render(fmt.Sprintf("%*s", sizeW, size))
	}
	if lay.showDate {
		line += "  " + dimStyle.Render(fmt.Sprintf("%-*s", dateW, date))
	}
	return line
}

// truncateLeft keeps the END of a long folder path (the part nearest the
// file), cutting the front: "…\Jobs\2026\".
func truncateLeft(s string, w int) string {
	if runewidth.StringWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return runewidth.Truncate(s, w, "")
	}
	rs := []rune(s)
	for len(rs) > 0 && runewidth.StringWidth(string(rs))+1 > w {
		rs = rs[1:]
	}
	return "…" + string(rs)
}

// highlight renders name with the characters matching any search term in the
// highlight style and everything else in the base style. Matching is
// case-insensitive and works rune by rune, so it is safe for any Unicode name.
func highlight(name string, terms []string, base, hl lipgloss.Style) string {
	runes := []rune(name)
	lower := make([]rune, len(runes))
	for i, r := range runes {
		lower[i] = unicode.ToLower(r)
	}
	marked := make([]bool, len(runes))
	for _, t := range terms {
		tr := []rune(t)
		if len(tr) == 0 {
			continue
		}
		for i := 0; i+len(tr) <= len(lower); i++ {
			match := true
			for j := range tr {
				if lower[i+j] != tr[j] {
					match = false
					break
				}
			}
			if match {
				for j := range tr {
					marked[i+j] = true
				}
			}
		}
	}
	var b strings.Builder
	for i := 0; i < len(runes); {
		j := i
		for j < len(runes) && marked[j] == marked[i] {
			j++
		}
		seg := string(runes[i:j])
		if marked[i] {
			b.WriteString(hl.Render(seg))
		} else {
			b.WriteString(base.Render(seg))
		}
		i = j
	}
	return b.String()
}
