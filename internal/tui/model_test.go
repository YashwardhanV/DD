package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/yashwardhanv/dd/internal/index"
)

// ---- a tiny harness: drive the model without a terminal --------------------------

type harness struct {
	t *testing.T
	m Model
}

// exec runs a command and feeds its result back into the model, recursively.
// Spinner ticks are dropped (they only keep the loading animation going).
func (h *harness) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			h.exec(c)
		}
	case spinner.TickMsg:
	default:
		h.send(msg)
	}
}

func (h *harness) send(msg tea.Msg) {
	model, cmd := h.m.Update(msg)
	h.m = model.(Model)
	h.exec(cmd)
}

func (h *harness) typeText(s string) {
	for _, r := range s {
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func (h *harness) key(t tea.KeyType) { h.send(tea.KeyMsg{Type: t}) }

func (h *harness) ctrl(k string) {
	keys := map[string]tea.KeyType{"o": tea.KeyCtrlO, "y": tea.KeyCtrlY, "p": tea.KeyCtrlP, "c": tea.KeyCtrlC}
	h.send(tea.KeyMsg{Type: keys[k]})
}

func (h *harness) view() string { return h.m.View() }

func (h *harness) rowNames() []string {
	var out []string
	for _, r := range h.m.rows {
		out = append(out, r.Name)
	}
	return out
}

// ---- test data --------------------------------------------------------------------

type world struct {
	root   string // where the online drive "F:" really is (a temp folder)
	cfg    Config
	opened []string
	shown  []string
	copied []string
	pulled [][2]string
}

// testIndexes builds three sources: an online drive, an offline drive and an
// online phone, with files whose names exercise ranking, Unicode and spaces.
func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{root: t.TempDir()}

	vol := index.New()
	vol.Header = index.Header{SourceKind: index.KindVolume, SourceID: "vol-AAAAAAAA", Label: "SANDISK 32GB", LastMount: `E:\`, UpdatedAt: time.Now()}
	vol.Add(index.Entry{ID: 1, Name: "Docs", Flags: index.FlagDir})
	vol.Add(index.Entry{ID: 2, ParentID: 1, Name: "Jobs", Flags: index.FlagDir})
	vol.Add(index.Entry{ID: 3, ParentID: 2, Name: "Resume_Yash_2026.pdf", Size: 212 * 1024, ModTime: 1_786_000_000})
	vol.Add(index.Entry{ID: 4, ParentID: 1, Name: "My Notes.txt", Size: 10, ModTime: 1_786_000_100})
	vol.Add(index.Entry{ID: 5, Name: "résumé.pdf", Size: 5, ModTime: 1_786_000_200})
	vol.Add(index.Entry{ID: 6, Name: "NoMeta File.txt", Size: -1, Flags: index.FlagNoMeta})

	off := index.New()
	off.Header = index.Header{SourceKind: index.KindVolume, SourceID: "vol-BBBBBBBB", Label: "OLD DRIVE", LastMount: `G:\`}
	off.Add(index.Entry{ID: 1, Name: "Old", Flags: index.FlagDir})
	off.Add(index.Entry{ID: 2, ParentID: 1, Name: "resume_old.pdf", Size: 180 * 1024, ModTime: 1_700_000_000})

	ph := index.New()
	ph.Header = index.Header{SourceKind: index.KindPhone, SourceID: "adb-R58", Label: "Pixel 7", RootPath: "/sdcard"}
	ph.Add(index.Entry{ID: 1, Name: "Download", Flags: index.FlagDir})
	ph.Add(index.Entry{ID: 2, ParentID: 1, Name: "Resume.pdf", Size: 198 * 1024, ModTime: 1_785_000_000})
	ph.Add(index.Entry{ID: 3, Name: "IMG_0001.jpg", Size: 3000, ModTime: 1_785_000_100})

	// Real files for the online drive so Open / lazy metadata have something to point at.
	os.MkdirAll(filepath.Join(w.root, "Docs", "Jobs"), 0o755)
	os.WriteFile(filepath.Join(w.root, "NoMeta File.txt"), make([]byte, 777), 0o644)

	env := index.Env{Mounts: map[string]string{"vol-AAAAAAAA": w.root}, Phones: map[string]bool{"adb-R58": true}}
	w.cfg = Config{
		Load:         func() (Loaded, error) { return Loaded{Indexes: []*index.Index{vol, off, ph}}, nil },
		Env:          func([]*index.Index) index.Env { return env },
		Count:        3,
		Debounce:     time.Millisecond,
		StaticCursor: true,
		Actions: Actions{
			Open:         func(p string) error { w.opened = append(w.opened, p); return nil },
			ShowInFolder: func(p string) error { w.shown = append(w.shown, p); return nil },
			CopyText:     func(s string) error { w.copied = append(w.copied, s); return nil },
			PullPhone: func(serial, remote string) (string, error) {
				w.pulled = append(w.pulled, [2]string{serial, remote})
				return filepath.Join(w.root, "Downloads", "DriveDex", filepath.Base(remote)), nil
			},
		},
	}
	return w
}

// start loads the world and returns a ready harness.
func (w *world) start(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, m: New(w.cfg)}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 20})
	h.exec(h.m.loadCmd())
	if h.m.state != stateReady {
		t.Fatalf("state = %v after loading", h.m.state)
	}
	return h
}

// ---- tests ------------------------------------------------------------------------

func TestLoadingScreenThenReady(t *testing.T) {
	w := newWorld(t)
	h := &harness{t: t, m: New(w.cfg)}
	if v := h.view(); !strings.Contains(v, "Loading 3 indexes…") {
		t.Errorf("loading view = %q", v)
	}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 20})
	h.exec(h.m.loadCmd())
	v := h.view()
	if !strings.Contains(v, "Type to search 11 entries in 3 sources (1 offline)") {
		t.Errorf("ready footer wrong:\n%s", v)
	}
	if strings.Contains(v, "Loading") {
		t.Error("the loading text must go away")
	}
	one := New(Config{Load: w.cfg.Load, Count: 1})
	if !strings.Contains(one.View(), "Loading 1 index…") {
		t.Error("singular form")
	}
}

func TestTypingSearchesAcrossAllSourcesWithRankingAndOfflineMarking(t *testing.T) {
	w := newWorld(t)
	h := w.start(t)
	h.typeText("resume")

	// All three start with "resume", so the shortest name wins, then the next shortest.
	// ("résumé.pdf" doesn't contain "resume": the accents differ, so it isn't a match.)
	want := []string{"Resume.pdf", "resume_old.pdf", "Resume_Yash_2026.pdf"}
	if got := h.rowNames(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("rows = %q, want %q", got, want)
	}
	v := h.view()
	for _, s := range []string{"Resume.pdf", "phone:Pixel 7/Download/", "Resume_Yash_2026.pdf", "resume_old.pdf",
		"[OLD DRIVE · offline · last seen G:] \\Old\\", "212 KB", "198 KB", "3 matches"} {
		if !strings.Contains(v, s) {
			t.Errorf("view missing %q:\n%s", s, v)
		}
	}
	if !regexp.MustCompile(`3 matches · (<1|\d+) ms · 3 sources \(1 offline\)`).MatchString(v) {
		t.Errorf("footer wrong:\n%s", v)
	}
	if !h.m.rows[1].Offline || h.m.rows[0].Offline || h.m.rows[2].Offline {
		t.Error("offline flags wrong: only resume_old.pdf is on the unplugged drive")
	}
}

func TestUnicodeCaseInsensitive(t *testing.T) {
	h := newWorld(t).start(t)
	h.typeText("RÉSUMÉ")
	if got := h.rowNames(); len(got) != 1 || got[0] != "résumé.pdf" {
		t.Errorf("rows = %q", got)
	}
}

func TestOnlyTheLatestSearchWins(t *testing.T) {
	h := newWorld(t).start(t)
	h.typeText("res") // searched: 3 results
	staleSeq := h.m.seq - 1

	// The user types another letter; the previous search finishes late.
	h.m.input.SetValue("resu")
	h.m.seq++
	model, _ := h.m.Update(searchDoneMsg{seq: staleSeq, rows: []Row{{Name: "STALE"}}, res: index.SearchResult{Total: 99}})
	h.m = model.(Model)
	for _, r := range h.m.rows {
		if r.Name == "STALE" {
			t.Fatal("a result from an older search overwrote the newer ones")
		}
	}
	// A debounce timer from an older keystroke does nothing.
	if _, cmd := h.m.Update(debounceMsg{seq: staleSeq}); cmd != nil {
		t.Error("a stale debounce timer must not start a search")
	}
	// The current one does.
	if _, cmd := h.m.Update(debounceMsg{seq: h.m.seq}); cmd == nil {
		t.Error("the current debounce timer must start a search")
	}
}

func TestKeysThatDontChangeTheTextDontSearchAgain(t *testing.T) {
	h := newWorld(t).start(t)
	h.typeText("res")
	seq := h.m.seq
	h.key(tea.KeyLeft)
	h.key(tea.KeyRight)
	if h.m.seq != seq {
		t.Error("cursor keys must not trigger a new search")
	}
	h.key(tea.KeyBackspace)
	if h.m.seq != seq+1 || h.m.input.Value() != "re" {
		t.Errorf("backspace: seq %d value %q", h.m.seq, h.m.input.Value())
	}
}

func TestClearingTheTextClearsResultsImmediately(t *testing.T) {
	h := newWorld(t).start(t)
	h.typeText("res")
	if len(h.m.rows) == 0 {
		t.Fatal("expected results")
	}
	for i := 0; i < 3; i++ {
		h.key(tea.KeyBackspace)
	}
	if len(h.m.rows) != 0 || !strings.Contains(h.view(), "Type to search") {
		t.Errorf("rows = %d; view:\n%s", len(h.m.rows), h.view())
	}
}

func TestNoMatchesMessage(t *testing.T) {
	h := newWorld(t).start(t)
	h.typeText("zzzznothing")
	if !strings.Contains(h.view(), "No matches.") || !strings.Contains(h.view(), "0 matches") {
		t.Errorf("view:\n%s", h.view())
	}
}

func TestInitialQueryIsSearchedAfterLoading(t *testing.T) {
	w := newWorld(t)
	w.cfg.Query = "notes"
	h := w.start(t)
	if got := h.rowNames(); len(got) != 1 || got[0] != "My Notes.txt" {
		t.Errorf("rows = %q", got)
	}
	if !strings.Contains(h.view(), "notes") {
		t.Error("the initial text should be shown in the search box")
	}
}

func TestDriveFilter(t *testing.T) {
	w := newWorld(t)
	w.cfg.DriveSourceID = "vol-BBBBBBBB"
	h := w.start(t)
	h.typeText("pdf")
	if got := h.rowNames(); len(got) != 1 || got[0] != "resume_old.pdf" {
		t.Errorf("rows = %q", got)
	}
}

func TestLayoutFitsTheScreen(t *testing.T) {
	h := newWorld(t).start(t)
	h.typeText("e")
	for _, size := range [][2]int{{120, 20}, {80, 24}, {60, 12}, {40, 8}} {
		h.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		lines := strings.Split(h.view(), "\n")
		if len(lines) > size[1] {
			t.Errorf("%dx%d: %d lines", size[0], size[1], len(lines))
		}
		for i, l := range lines {
			if w := lipgloss.Width(l); w > size[0] {
				t.Errorf("%dx%d line %d is %d wide: %q", size[0], size[1], i, w, l)
			}
		}
	}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 20})
	v := h.view()
	if !strings.Contains(v, "▶ ") || strings.Count(v, "▶") != 1 {
		t.Errorf("exactly one selected row marker expected:\n%s", v)
	}
	if !strings.Contains(v, "↑↓ move · Enter open · Ctrl+O show in folder · Ctrl+Y copy path · Ctrl+P get from phone · Esc quit") {
		t.Errorf("help line missing:\n%s", v)
	}
	if !strings.Contains(v, "🔍") {
		t.Error("search line missing")
	}
}

func TestCursorMovementAndScrolling(t *testing.T) {
	w := newWorld(t)
	// 60 files named "item NN.txt" in an extra source.
	big := index.New()
	big.Header = index.Header{SourceKind: index.KindFolder, SourceID: "dir-big", RootPath: w.root, Label: "big"}
	for i := 0; i < 60; i++ {
		big.Add(index.Entry{ID: uint64(i + 1), Name: fmt.Sprintf("item %02d.txt", i), Size: 1})
	}
	inner := w.cfg.Load
	w.cfg.Load = func() (Loaded, error) {
		l, _ := inner()
		l.Indexes = append(l.Indexes, big)
		return l, nil
	}
	h := w.start(t)
	h.send(tea.WindowSizeMsg{Width: 100, Height: 15}) // 10 visible rows
	h.typeText("item")
	if len(h.m.rows) != 60 || h.m.visibleRows() != 10 {
		t.Fatalf("rows=%d visible=%d", len(h.m.rows), h.m.visibleRows())
	}

	h.key(tea.KeyUp) // already at the top
	if h.m.cursor != 0 {
		t.Error("cursor must not go above the first row")
	}
	for i := 0; i < 12; i++ {
		h.key(tea.KeyDown)
	}
	if h.m.cursor != 12 || h.m.offset != 3 { // 12 - 10 + 1
		t.Errorf("after 12 downs: cursor %d offset %d", h.m.cursor, h.m.offset)
	}
	h.key(tea.KeyPgDown)
	if h.m.cursor != 22 || h.m.offset != 13 {
		t.Errorf("page down: cursor %d offset %d", h.m.cursor, h.m.offset)
	}
	for i := 0; i < 20; i++ {
		h.key(tea.KeyPgDown)
	}
	if h.m.cursor != 59 || h.m.offset != 50 {
		t.Errorf("end: cursor %d offset %d", h.m.cursor, h.m.offset)
	}
	h.key(tea.KeyPgUp)
	if h.m.cursor != 49 {
		t.Errorf("page up: cursor %d", h.m.cursor)
	}
	// The window always shows the cursor's row.
	if !strings.Contains(h.view(), h.m.rows[h.m.cursor].Name) {
		t.Error("the selected row must be on screen")
	}
	if lines := strings.Count(h.view(), "\n") + 1; lines != 15 {
		t.Errorf("the view must always have exactly 15 lines, got %d", lines)
	}
}

func TestEnterOpensTheSelectedFile(t *testing.T) {
	w := newWorld(t)
	h := w.start(t)
	h.typeText("my notes")
	h.key(tea.KeyEnter)
	want := filepath.Join(w.root, "Docs", "My Notes.txt") // a path with a space
	if len(w.opened) != 1 || w.opened[0] != want {
		t.Errorf("opened = %q, want %q", w.opened, want)
	}
	// A folder opens too (Explorer).
	h.key(tea.KeyBackspace)
	h.typeText("")
	h.m.input.SetValue("jobs")
	h.m.seq++
	h.exec(h.m.searchCmd(h.m.seq))
	h.key(tea.KeyEnter)
	if got := w.opened[len(w.opened)-1]; got != filepath.Join(w.root, "Docs", "Jobs") {
		t.Errorf("folder opened as %q", got)
	}
}

func TestEnterOnOfflineAndPhoneRows(t *testing.T) {
	w := newWorld(t)
	h := w.start(t)
	h.typeText("resume_old")
	h.key(tea.KeyEnter)
	if got := h.m.status; got != `Drive "OLD DRIVE" isn't connected — plug it in.` {
		t.Errorf("status = %q", got)
	}
	if !strings.Contains(h.view(), `Drive "OLD DRIVE" isn't connected — plug it in.`) {
		t.Error("the message must appear on screen")
	}
	if len(w.opened) != 0 {
		t.Error("nothing may be opened for an offline drive")
	}
	// Status disappears at the next key press.
	h.typeText("x")
	if h.m.status != "" {
		t.Error("the status line should clear on the next key")
	}

	h.m.input.SetValue("img_0001")
	h.m.seq++
	h.exec(h.m.searchCmd(h.m.seq))
	h.key(tea.KeyEnter)
	if !strings.Contains(h.m.status, "Ctrl+P") || len(w.opened) != 0 {
		t.Errorf("a phone file can't be opened directly: status %q opened %q", h.m.status, w.opened)
	}
}

func TestCtrlOShowsInFolderWithTheExactPath(t *testing.T) {
	w := newWorld(t)
	h := w.start(t)
	h.typeText("my notes")
	h.ctrl("o")
	want := filepath.Join(w.root, "Docs", "My Notes.txt")
	if len(w.shown) != 1 || w.shown[0] != want {
		t.Errorf("shown = %q, want %q", w.shown, want)
	}
	h.m.input.SetValue("resume_old")
	h.m.seq++
	h.exec(h.m.searchCmd(h.m.seq))
	h.ctrl("o")
	if !strings.Contains(h.m.status, "isn't connected") || len(w.shown) != 1 {
		t.Errorf("offline: status %q shown %q", h.m.status, w.shown)
	}
}

func TestCtrlYCopiesThePath(t *testing.T) {
	w := newWorld(t)
	h := w.start(t)
	h.typeText("my notes")
	h.ctrl("y")
	if len(w.copied) != 1 || w.copied[0] != filepath.Join(w.root, "Docs", "My Notes.txt") || h.m.status != "Copied!" {
		t.Errorf("copied = %q status %q", w.copied, h.m.status)
	}
	if !strings.Contains(h.view(), "Copied!") {
		t.Error("'Copied!' must be shown")
	}

	// Offline drive: the path where the file was last seen.
	h.m.input.SetValue("resume_old")
	h.m.seq++
	h.exec(h.m.searchCmd(h.m.seq))
	h.ctrl("y")
	if got := w.copied[len(w.copied)-1]; got != index.JoinRoot(`G:\`, `Old\resume_old.pdf`) {
		t.Errorf("offline copy path = %q", got)
	}
	// Phone: a path you can paste into `drivedex copy`.
	h.m.input.SetValue("img_0001")
	h.m.seq++
	h.exec(h.m.searchCmd(h.m.seq))
	h.ctrl("y")
	if got := w.copied[len(w.copied)-1]; got != "phone:/IMG_0001.jpg" {
		t.Errorf("phone copy path = %q", got)
	}
	// A failing clipboard is reported, not fatal.
	w.cfg.Actions.CopyText = nil
	h.m.cfg.Actions.CopyText = func(string) error { return errors.New("no clipboard") }
	h.ctrl("y")
	if !strings.Contains(h.m.status, "no clipboard") {
		t.Errorf("status = %q", h.m.status)
	}
}

func TestCtrlPPullsAPhoneFile(t *testing.T) {
	w := newWorld(t)
	h := w.start(t)
	h.typeText("resume.pdf") // the phone's /sdcard/Download/Resume.pdf
	h.ctrl("p")
	if len(w.pulled) != 1 || w.pulled[0] != [2]string{"R58", "/sdcard/Download/Resume.pdf"} {
		t.Fatalf("pulled = %q", w.pulled)
	}
	wantLocal := filepath.Join(w.root, "Downloads", "DriveDex", "Resume.pdf")
	if len(w.shown) != 1 || w.shown[0] != wantLocal {
		t.Errorf("must show the pulled file in its folder: shown = %q", w.shown)
	}
	if h.m.status != "Saved to "+wantLocal {
		t.Errorf("status = %q", h.m.status)
	}

	// Not a phone row.
	h.m.input.SetValue("my notes")
	h.m.seq++
	h.exec(h.m.searchCmd(h.m.seq))
	h.ctrl("p")
	if !strings.Contains(h.m.status, "phone") || len(w.pulled) != 1 {
		t.Errorf("non-phone row: status %q", h.m.status)
	}
	// Pull failure.
	h.m.cfg.Actions.PullPhone = func(string, string) (string, error) { return "", errors.New("device offline") }
	h.m.input.SetValue("img_0001")
	h.m.seq++
	h.exec(h.m.searchCmd(h.m.seq))
	h.ctrl("p")
	if !strings.Contains(h.m.status, "Couldn't copy from the phone: device offline") {
		t.Errorf("status = %q", h.m.status)
	}
}

func TestCtrlPOnAnOfflinePhone(t *testing.T) {
	w := newWorld(t)
	env := index.Env{Mounts: map[string]string{"vol-AAAAAAAA": w.root}} // phone not connected
	w.cfg.Env = func([]*index.Index) index.Env { return env }
	h := w.start(t)
	h.typeText("img_0001")
	if !h.m.rows[0].Offline || !strings.Contains(h.m.rows[0].Dir, "· offline") {
		t.Errorf("row = %+v", h.m.rows[0])
	}
	h.ctrl("p")
	if h.m.status != `Phone "Pixel 7" isn't connected — plug it in.` || len(w.pulled) != 0 {
		t.Errorf("status = %q", h.m.status)
	}
}

func TestEscAndCtrlCQuit(t *testing.T) {
	h := newWorld(t).start(t)
	for _, k := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyCtrlC}} {
		_, cmd := h.m.Update(k)
		if cmd == nil {
			t.Fatalf("%v should quit", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%v: command is not tea.Quit", k)
		}
	}
}

func TestLazyMetadataForVisibleRows(t *testing.T) {
	w := newWorld(t)
	h := w.start(t)
	h.typeText("nometa")
	if len(h.m.rows) != 1 {
		t.Fatalf("rows = %v", h.rowNames())
	}
	// The index has no size for this file; the screen looked it up (777 bytes).
	if r := h.m.rows[0]; r.Size != 777 || r.MTime == 0 {
		t.Errorf("row = %+v", r)
	}
	if !strings.Contains(h.view(), "777 B") {
		t.Errorf("view:\n%s", h.view())
	}
}

func TestUnknownMetadataShowsQuestionMark(t *testing.T) {
	w := newWorld(t)
	os.Remove(filepath.Join(w.root, "NoMeta File.txt")) // can't be looked up
	h := w.start(t)
	h.typeText("nometa")
	if v := h.view(); !strings.Contains(v, "?") || !strings.Contains(v, " -") {
		t.Errorf("unknown size/date should show ? and -:\n%s", v)
	}
}

func TestEmptyIndexesAndErrors(t *testing.T) {
	w := newWorld(t)
	w.cfg.Load = func() (Loaded, error) { return Loaded{}, nil }
	h := &harness{t: t, m: New(w.cfg)}
	h.exec(h.m.loadCmd())
	if !strings.Contains(h.view(), "Nothing indexed yet") || !strings.Contains(h.view(), "drivedex index") {
		t.Errorf("view:\n%s", h.view())
	}
	// Enter/Esc/q quit from this screen.
	for _, k := range []string{"esc", "enter"} {
		keys := map[string]tea.KeyType{"esc": tea.KeyEsc, "enter": tea.KeyEnter}
		if _, cmd := h.m.Update(tea.KeyMsg{Type: keys[k]}); cmd == nil {
			t.Errorf("%s should quit", k)
		}
	}

	w.cfg.Load = func() (Loaded, error) { return Loaded{}, errors.New("disk on fire") }
	h = &harness{t: t, m: New(w.cfg)}
	h.exec(h.m.loadCmd())
	if !strings.Contains(h.view(), "Error: disk on fire") {
		t.Errorf("view:\n%s", h.view())
	}
}

// ---- --scope ------------------------------------------------------------------------

func TestScopeRestrictsResultsToAFolder(t *testing.T) {
	w := newWorld(t)
	w.cfg.ScopePath = filepath.Join(w.root, "Docs") // Docs\ on the online drive
	h := w.start(t)
	h.typeText("pdf")
	// résumé.pdf is in the drive root (outside Docs); Resume_Yash_2026.pdf is inside; phone/other drive excluded.
	if got := h.rowNames(); len(got) != 1 || got[0] != "Resume_Yash_2026.pdf" {
		t.Errorf("rows = %q", got)
	}
	h.m.input.SetValue("txt")
	h.m.seq++
	h.exec(h.m.searchCmd(h.m.seq))
	if got := h.rowNames(); len(got) != 1 || got[0] != "My Notes.txt" {
		t.Errorf("rows = %q", got)
	}
}

func TestScopeOfTheWholeDrive(t *testing.T) {
	w := newWorld(t)
	w.cfg.ScopePath = w.root
	h := w.start(t)
	h.typeText("pdf")
	if got := h.rowNames(); len(got) != 2 { // both pdf files on that drive, nothing from other sources
		t.Errorf("rows = %q", got)
	}
}

func TestScopeNotIndexedShowsThePromptAndIndexesOnI(t *testing.T) {
	w := newWorld(t)
	w.cfg.ScopePath = filepath.Join(t.TempDir(), "unindexed")
	os.MkdirAll(w.cfg.ScopePath, 0o755)
	indexed := 0
	w.cfg.IndexScope = func() error { indexed++; return nil }

	h := &harness{t: t, m: New(w.cfg)}
	h.send(tea.WindowSizeMsg{Width: 100, Height: 20})
	h.exec(h.m.loadCmd())
	v := h.view()
	if h.m.state != stateScope || !strings.Contains(v, "This drive isn't indexed yet. Press I to index now (takes a moment), Esc to cancel.") {
		t.Fatalf("state %v view:\n%s", h.m.state, v)
	}
	h.typeText("x") // other keys do nothing
	if h.m.state != stateScope {
		t.Error("only I and Esc are accepted here")
	}

	// Press I: it indexes, reloads, and (in this fake) the scope is still not found -> the prompt returns.
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	if indexed != 1 {
		t.Errorf("IndexScope called %d times", indexed)
	}
	if h.m.state != stateScope {
		t.Errorf("state = %v (the fake index doesn't cover the folder)", h.m.state)
	}

	_, cmd := h.m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("Esc must quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("Esc must quit")
	}
}

func TestScopeIndexingThenSearchWorks(t *testing.T) {
	w := newWorld(t)
	scopeDir := filepath.Join(w.root, "Fresh")
	os.MkdirAll(scopeDir, 0o755)
	w.cfg.ScopePath = scopeDir
	// Before indexing, "Fresh" isn't in the index; IndexScope "adds" it.
	fresh := false
	inner := w.cfg.Load
	w.cfg.Load = func() (Loaded, error) {
		l, _ := inner()
		if fresh {
			l.Indexes[0].Add(index.Entry{ID: 90, Name: "Fresh", Flags: index.FlagDir})
			l.Indexes[0].Add(index.Entry{ID: 91, ParentID: 90, Name: "new thing.txt", Size: 3})
		}
		return l, nil
	}
	w.cfg.IndexScope = func() error { fresh = true; return nil }

	h := w.start2(t)
	if h.m.state != stateScope {
		t.Fatalf("state = %v", h.m.state)
	}
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'I'}})
	if h.m.state != stateReady {
		t.Fatalf("after indexing, state = %v", h.m.state)
	}
	h.typeText("thing")
	if got := h.rowNames(); len(got) != 1 || got[0] != "new thing.txt" {
		t.Errorf("rows = %q", got)
	}
}

func TestScopeNotIndexedWithoutAnIndexerJustExplains(t *testing.T) {
	w := newWorld(t)
	w.cfg.ScopePath = filepath.Join(t.TempDir(), "nowhere")
	h := &harness{t: t, m: New(w.cfg)}
	h.exec(h.m.loadCmd())
	v := h.view()
	if !strings.Contains(v, "isn't indexed yet") || strings.Contains(v, "Press I") {
		t.Errorf("view:\n%s", v)
	}
}

// start2 is start() but tolerates a non-ready first state.
func (w *world) start2(t *testing.T) *harness {
	h := &harness{t: t, m: New(w.cfg)}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 20})
	h.exec(h.m.loadCmd())
	return h
}

// ---- helpers -------------------------------------------------------------------------

func TestHighlightMarksEveryMatchCaseInsensitively(t *testing.T) {
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(old)

	base := lipgloss.NewStyle().Bold(true)
	hl := lipgloss.NewStyle().Underline(true)
	got := highlight("Resume_Résumé.PDF", []string{"résumé", "pdf"}, base, hl)

	// Split the output into styled pieces and sort them into highlighted / normal text.
	piece := regexp.MustCompile("\x1b\\[([0-9;]*)m([^\x1b]+)\x1b\\[0m")
	var marked, normal strings.Builder
	for _, m := range piece.FindAllStringSubmatch(got, -1) {
		if strings.Contains(m[1], "4") { // underline = our highlight style
			marked.WriteString(m[2])
		} else {
			normal.WriteString(m[2])
		}
	}
	if marked.String() != "RésuméPDF" || normal.String() != "Resume_." {
		t.Errorf("highlighted %q, normal %q (output %q)", marked.String(), normal.String(), got)
	}
	// The visible text itself is unchanged.
	if plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(got, ""); plain != "Resume_Résumé.PDF" {
		t.Errorf("highlighting must not change the text: %q", plain)
	}
	// No terms: nothing highlighted.
	if got := highlight("abc", nil, base, hl); strings.Contains(got, "4m") || strings.Contains(got, "4;") {
		t.Errorf("no terms must highlight nothing: %q", got)
	}
}

func TestTruncateLeftKeepsTheEnd(t *testing.T) {
	if got := truncateLeft(`E:\Docs\`, 20); got != `E:\Docs\` {
		t.Errorf("short paths are untouched: %q", got)
	}
	got := truncateLeft(`E:\Docs\Jobs\2026\`, 10)
	if lipgloss.Width(got) > 10 || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, `2026\`) {
		t.Errorf("long path: %q", got)
	}
	if got := truncateLeft("abc", 1); lipgloss.Width(got) > 1 {
		t.Errorf("tiny width: %q", got)
	}
}

func TestComputeLayoutNeverExceedsTheScreen(t *testing.T) {
	for width := 12; width <= 200; width++ {
		for _, longest := range []int{3, 20, 80} {
			l := computeLayout(width, longest)
			total := 1 + 2 + l.nameW
			if l.dirW > 0 {
				total += 2 + l.dirW
			}
			if l.showSize {
				total += 2 + sizeW
			}
			if l.showDate {
				total += 2 + dateW
			}
			if total > width && width >= 16 {
				t.Fatalf("width %d longest %d: line would be %d wide (%+v)", width, longest, total, l)
			}
		}
	}
	if l := computeLayout(120, 20); !l.showSize || !l.showDate {
		t.Error("a wide screen shows size and date")
	}
	if l := computeLayout(55, 20); !l.showSize || l.showDate {
		t.Error("a medium screen drops the date first")
	}
	if l := computeLayout(40, 20); l.showSize || l.showDate {
		t.Error("a narrow screen shows names and folders only")
	}
}

func TestBuildRowsForEachSourceKind(t *testing.T) {
	w := newWorld(t)
	l, _ := w.cfg.Load()
	env := w.cfg.Env(l.Indexes)
	res := index.Search(l.Indexes, index.NewTermMatcher("resume"), index.SearchOptions{Limit: 10})
	rows := buildRows(l.Indexes, env, res)
	byName := map[string]Row{}
	for _, r := range rows {
		byName[r.Name] = r
	}

	pc := byName["Resume_Yash_2026.pdf"]
	if pc.Offline || pc.LocalPath != filepath.Join(w.root, "Docs", "Jobs", "Resume_Yash_2026.pdf") || pc.CopyPath != pc.LocalPath || pc.Kind != index.KindVolume {
		t.Errorf("online drive row = %+v", pc)
	}
	off := byName["resume_old.pdf"]
	if !off.Offline || off.LocalPath != "" || off.Label != "OLD DRIVE" || !strings.HasPrefix(off.Dir, "[OLD DRIVE · offline") {
		t.Errorf("offline drive row = %+v", off)
	}
	ph := byName["Resume.pdf"]
	if ph.Offline || ph.LocalPath != "" || ph.PhoneSerial != "R58" || ph.PhonePath != "/sdcard/Download/Resume.pdf" || ph.CopyPath != "phone:/Download/Resume.pdf" || ph.Dir != "phone:Pixel 7/Download/" {
		t.Errorf("phone row = %+v", ph)
	}
}

func TestSpinnerTicksAreIgnoredOnceReady(t *testing.T) {
	h := newWorld(t).start(t)
	if _, cmd := h.m.Update(spinner.TickMsg{}); cmd != nil {
		t.Error("the spinner must stop ticking after loading")
	}
}
