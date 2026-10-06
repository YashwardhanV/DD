package tui

import (
	"strings"
	"testing"
)

func TestQuerySyntaxWorksInTheScreen(t *testing.T) {
	h := newWorld(t).start(t)

	h.typeText("resume ext:pdf size:>200kb")
	if got := h.rowNames(); strings.Join(got, "|") != "Resume_Yash_2026.pdf" {
		t.Errorf("size filter: rows = %q", got)
	}

	// drive:phone narrows to the phone; ext: takes a list.
	h.m.input.SetValue("")
	h.typeText("drive:phone ext:pdf,jpg")
	if got := h.rowNames(); strings.Join(got, "|") != "Resume.pdf|IMG_0001.jpg" && strings.Join(got, "|") != "IMG_0001.jpg|Resume.pdf" {
		t.Errorf("drive:phone ext:pdf,jpg: rows = %q", got)
	}

	h.m.input.SetValue("")
	h.typeText("resume !yash !old")
	if got := h.rowNames(); len(got) != 1 || got[0] != "Resume.pdf" {
		t.Errorf("exclusion: rows = %q", got)
	}

	h.m.input.SetValue("")
	h.typeText("type:dir jobs")
	if got := h.rowNames(); len(got) != 1 || got[0] != "Jobs" {
		t.Errorf("type:dir: rows = %q", got)
	}
}

// While the user is half-way through typing a filter ("size:>"), the last good
// results stay and the footer says what is missing - no error screens, no flicker.
func TestHalfTypedFilterKeepsResultsAndExplains(t *testing.T) {
	h := newWorld(t).start(t)
	h.typeText("resume")
	before := h.rowNames()
	if len(before) == 0 {
		t.Fatal("no results to keep")
	}
	// (Set the text directly: typed one letter at a time, "resume s", "resume si"...
	// are all valid searches of their own, which would replace the results.)
	h.m.input.SetValue("resume size:")
	h.m.input.CursorEnd()
	h.typeText(">")
	if got := h.rowNames(); strings.Join(got, "|") != strings.Join(before, "|") {
		t.Errorf("rows changed while the filter was incomplete: %q -> %q", before, got)
	}
	if h.m.queryErr == "" || !strings.Contains(h.view(), h.m.queryErr) {
		t.Errorf("footer should explain the problem; queryErr=%q", h.m.queryErr)
	}
	h.typeText("1mb") // now valid
	if h.m.queryErr != "" {
		t.Errorf("error should clear once the filter is complete: %q", h.m.queryErr)
	}
}

func TestMetadataFilterTipOnNoMetaEntries(t *testing.T) {
	h := newWorld(t).start(t)
	h.typeText("nometa size:>1b")
	if h.m.noMeta == 0 {
		t.Fatal("the NoMeta entry should have been counted as skipped")
	}
	if !strings.Contains(h.view(), "--with-metadata") {
		t.Errorf("footer should mention --with-metadata:\n%s", h.view())
	}
}

func TestHideHiddenSetting(t *testing.T) {
	w := newWorld(t)
	w.cfg.HideHidden = true
	h := w.start(t)
	h.typeText("resume")
	if len(h.rowNames()) == 0 {
		t.Error("ordinary files must still show when hidden files are hidden")
	}
}

// On Windows a very fast search can measure as 0 elapsed time; "No matches." must
// not depend on the timer (it did once - the CI run on Windows caught it).
func TestNoMatchesHintDoesNotDependOnTheTimer(t *testing.T) {
	h := newWorld(t).start(t)
	h.typeText("zzzznothing")
	h.send(searchDoneMsg{seq: h.m.seq, ran: true, took: 0})
	if !strings.Contains(h.view(), "No matches.") {
		t.Errorf("a finished search with no rows must say so even when took == 0:\n%s", h.view())
	}
	// ...but not before any search has finished for this text.
	h.m.input.SetValue("")
	h.m.input.CursorEnd()
	h.typeText("a")
	h.m.searched = false
	h.m.rows = nil
	if strings.Contains(h.view(), "No matches.") {
		t.Error("no hint while the search hasn't finished")
	}
}
