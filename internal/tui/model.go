// Package tui is DriveDex's interactive "type and results appear" search screen.
//
// It is built with Bubble Tea, which follows "the Elm architecture": the whole
// screen is a Model; every key press or finished background job becomes a
// message; Update(model, message) returns the new model (plus any background
// work to start); View(model) turns the model into text. Because Update is a
// plain function it can be tested without a terminal.
package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/query"
)

// Loaded is everything read from disk at startup.
type Loaded struct {
	Indexes  []*index.Index
	Warnings []error
}

// Actions are the things the screen can do to the outside world. They are
// fields so tests can replace them; DefaultActions (per platform) are the real ones.
type Actions struct {
	// Open opens a file with its default app (or a folder in Explorer).
	Open func(path string) error
	// ShowInFolder opens Explorer with the file selected.
	ShowInFolder func(path string) error
	// CopyText puts text on the clipboard.
	CopyText func(text string) error
	// PullPhone copies one file from the phone into the Downloads\DriveDex
	// folder and returns where it landed. nil = not available.
	PullPhone func(serial, remote string) (localPath string, err error)
}

// Config describes one run of the screen.
type Config struct {
	// Load reads the saved indexes (called on a background goroutine).
	Load func() (Loaded, error)
	// Env works out which drives/phones are connected (background goroutine).
	Env func(indexes []*index.Index) index.Env
	// Count is how many indexes Load will read, for the "Loading 3 indexes…" text.
	Count int
	// Query is the text to start with.
	Query string
	// DriveSourceID restricts results to one index (from --drive); "" = all.
	DriveSourceID string
	// ScopePath restricts results to this local folder (from --scope); "" = all.
	ScopePath string
	// IndexScope indexes the drive of ScopePath. If nil the "not indexed yet"
	// prompt is replaced by a plain message.
	IndexScope func() error
	Actions    Actions
	// Debounce is the wait after the last key press before searching (default 60 ms).
	Debounce time.Duration
	// Limit caps how many results are kept for scrolling (default 500).
	Limit int
	// HideHidden leaves hidden and system files out of the results
	// (config show_hidden = false).
	HideHidden bool
	// StaticCursor turns off the blinking text cursor (its timer messages make
	// tests slow and noisy).
	StaticCursor bool
}

type state int

const (
	stateLoading state = iota
	stateReady
	stateEmpty // nothing indexed at all
	stateScope // the --scope folder isn't covered by an index
	stateIndexing
	stateError
)

// Row is one displayed result.
type Row struct {
	Name, Dir   string
	Size, MTime int64
	IsDir       bool
	NoMeta      bool // size/date not in the index yet
	Offline     bool
	Kind        string // index.KindVolume / KindPhone / KindFolder
	Label       string
	// LocalPath is where the file is on this PC right now ("" when offline or a phone).
	LocalPath string
	// CopyPath is what Ctrl+Y copies.
	CopyPath string
	// PhoneSerial and PhonePath identify a file on a phone.
	PhoneSerial, PhonePath string
}

// Messages --------------------------------------------------------------------

type loadedMsg struct {
	loaded Loaded
	env    index.Env
	err    error
}

type debounceMsg struct{ seq int }

type searchDoneMsg struct {
	seq   int
	res   index.SearchResult
	rows  []Row
	took  time.Duration
	terms []string
	// ran is true when a search really ran (as opposed to an empty query).
	// (Not inferred from took > 0: on Windows a fast search can read as 0 elapsed.)
	ran bool
	// queryErr is set when the text isn't (yet) a valid search, e.g. "size:>".
	// The previous results stay on screen while the user finishes typing.
	queryErr string
	// noMeta counts entries skipped because the search filters on size/date
	// and the index doesn't know them.
	noMeta int64
}

type actionDoneMsg struct{ status string }

type indexedMsg struct{ err error }

// Model is the whole screen's state.
type Model struct {
	cfg   Config
	state state
	spin  spinner.Model
	input textinput.Model

	width, height int

	indexes  []*index.Index
	warnings []error
	env      index.Env
	scope    *index.ScopeMatcher // nil = no --scope
	errText  string

	query    string
	queryErr string // why the current text isn't a valid search ("" = fine)
	noMeta   int64
	terms    []string
	seq      int // bumped on every change of the text; used to drop stale work
	rows     []Row
	total    int
	covered  int // indexes searched
	took     time.Duration
	searched bool // a search has finished for the current text (so "no rows" means "no matches")
	cursor   int
	offset   int
	status   string
}

// New creates the screen's model.
func New(cfg Config) Model {
	if cfg.Debounce <= 0 {
		cfg.Debounce = 60 * time.Millisecond
	}
	if cfg.Limit <= 0 {
		cfg.Limit = 500
	}
	in := textinput.New()
	in.Prompt = ""
	in.CharLimit = 200
	in.SetValue(cfg.Query)
	in.CursorEnd()
	if cfg.StaticCursor {
		in.Cursor.SetMode(cursor.CursorStatic)
	}
	in.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot

	return Model{cfg: cfg, state: stateLoading, spin: sp, input: in, width: 80, height: 24, query: cfg.Query}
}

// Init starts loading the indexes in the background.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, textinput.Blink, m.loadCmd())
}

func (m Model) loadCmd() tea.Cmd {
	load, envFn := m.cfg.Load, m.cfg.Env
	return func() tea.Msg {
		l, err := load()
		if err != nil {
			return loadedMsg{err: err}
		}
		env := index.Env{}
		if envFn != nil {
			env = envFn(l.Indexes)
		}
		return loadedMsg{loaded: l, env: env}
	}
}

// visibleRows is how many result lines fit: the screen minus the search line,
// two rules, the count line and the help line.
func (m Model) visibleRows() int { return max(1, m.height-5) }

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = max(10, msg.Width-8)
		m.clampScroll()
		m.ensureMeta()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		if m.state == stateLoading || m.state == stateIndexing {
			return m, cmd
		}
		return m, nil

	case loadedMsg:
		return m.onLoaded(msg)

	case indexedMsg:
		if msg.err != nil {
			m.state, m.errText = stateError, "Couldn't index that drive: "+msg.err.Error()
			return m, nil
		}
		m.state = stateLoading
		return m, tea.Batch(m.spin.Tick, m.loadCmd())

	case debounceMsg:
		if msg.seq != m.seq || m.state != stateReady {
			return m, nil // the user typed again: a newer timer is pending
		}
		return m, m.searchCmd(m.seq)

	case searchDoneMsg:
		if msg.seq != m.seq {
			return m, nil // an older search finishing late must not overwrite newer results
		}
		m.queryErr = msg.queryErr
		if msg.queryErr != "" {
			return m, nil // keep showing the last good results while the user finishes the filter
		}
		m.rows, m.total, m.covered, m.took, m.terms, m.noMeta = msg.rows, msg.res.Total, msg.res.Indexes, msg.took, msg.terms, msg.noMeta
		m.searched = msg.ran
		m.cursor, m.offset = 0, 0
		m.ensureMeta()
		return m, nil

	case actionDoneMsg:
		m.status = msg.status
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m Model) onLoaded(msg loadedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.state, m.errText = stateError, msg.err.Error()
		return m, nil
	}
	m.indexes, m.warnings, m.env = msg.loaded.Indexes, msg.loaded.Warnings, msg.env
	if len(m.indexes) == 0 {
		m.state = stateEmpty
		return m, nil
	}

	m.scope = nil
	if m.cfg.ScopePath != "" {
		abs, err := filepath.Abs(m.cfg.ScopePath)
		if err != nil {
			m.state, m.errText = stateError, err.Error()
			return m, nil
		}
		loc, ok := index.LocateScope(m.indexes, m.env, abs)
		if !ok {
			m.state = stateScope
			return m, nil
		}
		m.scope = &index.ScopeMatcher{SourceID: loc.Index.Header.SourceID, FolderID: loc.FolderID}
	}

	m.state = stateReady
	if strings.TrimSpace(m.input.Value()) != "" {
		return m, m.searchCmd(m.seq)
	}
	return m, nil
}

func (m Model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// States other than "ready" only understand a few keys.
	switch m.state {
	case stateScope:
		switch strings.ToLower(msg.String()) {
		case "i":
			if m.cfg.IndexScope != nil {
				m.state = stateIndexing
				return m, tea.Batch(m.spin.Tick, m.indexCmd())
			}
		case "esc", "ctrl+c", "q":
			return m, tea.Quit
		}
		return m, nil
	case stateReady:
	default:
		if k := msg.String(); k == "esc" || k == "ctrl+c" || k == "q" || k == "enter" {
			return m, tea.Quit
		}
		return m, nil
	}

	m.status = ""
	switch msg.String() {
	case "esc", "ctrl+c":
		return m, tea.Quit
	case "up":
		m.moveCursor(-1)
		return m, nil
	case "down":
		m.moveCursor(1)
		return m, nil
	case "pgup":
		m.moveCursor(-m.visibleRows())
		return m, nil
	case "pgdown":
		m.moveCursor(m.visibleRows())
		return m, nil
	case "enter":
		return m.doOpen()
	case "ctrl+o":
		return m.doShow()
	case "ctrl+y":
		return m.doCopy()
	case "ctrl+p":
		return m.doPhone()
	}

	// Anything else is typing.
	before := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() == before {
		return m, cmd // a cursor key etc.: the text didn't change
	}
	m.seq++
	m.query = m.input.Value()
	if strings.TrimSpace(m.query) == "" {
		m.rows, m.total, m.cursor, m.offset, m.terms = nil, 0, 0, 0, nil
		m.queryErr, m.noMeta, m.searched = "", 0, false
		return m, cmd
	}
	seq := m.seq
	tick := tea.Tick(m.cfg.Debounce, func(time.Time) tea.Msg { return debounceMsg{seq: seq} })
	return m, tea.Batch(cmd, tick)
}

func (m *Model) moveCursor(delta int) {
	if len(m.rows) == 0 {
		return
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.rows)-1)
	m.clampScroll()
	m.ensureMeta()
}

// clampScroll keeps the selected row inside the visible window.
func (m *Model) clampScroll() {
	v := m.visibleRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+v {
		m.offset = m.cursor - v + 1
	}
	m.offset = max(0, min(m.offset, max(0, len(m.rows)-v)))
}

// ensureMeta looks up the size and date of the visible rows that the index
// doesn't know (NTFS turbo-mode indexes store names only). Only rows on screen
// are touched, and only when their drive is connected.
func (m *Model) ensureMeta() {
	end := min(len(m.rows), m.offset+m.visibleRows())
	for i := m.offset; i < end; i++ {
		r := &m.rows[i]
		if !r.NoMeta || r.LocalPath == "" {
			continue
		}
		r.NoMeta = false // one attempt per row
		if st, err := os.Lstat(r.LocalPath); err == nil {
			r.MTime = st.ModTime().Unix()
			if st.IsDir() {
				r.Size = 0
			} else {
				r.Size = st.Size()
			}
		}
	}
}

// ---- background work ----------------------------------------------------------

func (m Model) indexCmd() tea.Cmd {
	fn := m.cfg.IndexScope
	return func() tea.Msg { return indexedMsg{err: fn()} }
}

// searchCmd runs the search on a background goroutine and tags the answer with
// seq, so Update can tell whether the user has typed again since.
func (m Model) searchCmd(seq int) tea.Cmd {
	text := m.input.Value()
	indexes, env, limit := m.indexes, m.env, m.cfg.Limit
	driveID := m.cfg.DriveSourceID
	var scope *index.ScopeMatcher
	if m.scope != nil {
		c := *m.scope
		scope = &c
	}
	hideHidden := m.cfg.HideHidden
	return func() tea.Msg {
		q, err := query.Parse(text, time.Now())
		if err != nil {
			return searchDoneMsg{seq: seq, queryErr: err.Error()}
		}
		if q.IsEmpty() {
			return searchDoneMsg{seq: seq}
		}
		qm := q.Matcher(env)
		qm.HideHidden = hideHidden
		var matcher index.Matcher = qm
		if driveID != "" {
			matcher = &index.SourceFilter{Inner: matcher, SourceID: driveID}
		}
		if scope != nil {
			scope.Inner = matcher
			matcher = scope
		}
		start := time.Now()
		res := index.Search(indexes, matcher, index.SearchOptions{Limit: limit})
		var noMeta int64
		if q.UsesMetadata() {
			noMeta = qm.SkippedNoMeta()
		}
		return searchDoneMsg{seq: seq, res: res, rows: buildRows(indexes, env, res), took: time.Since(start), terms: highlightTerms(q), noMeta: noMeta, ran: true}
	}
}

// highlightTerms are the words to highlight in the results: the name words and
// phrases of the search (not the excluded ones).
func highlightTerms(q *query.Query) []string { return q.Terms }

// buildRows turns search hits into displayable rows.
func buildRows(indexes []*index.Index, env index.Env, res index.SearchResult) []Row {
	caches := make([]map[uint64]string, len(indexes))
	rows := make([]Row, 0, len(res.Hits))
	for _, h := range res.Hits {
		ix := indexes[h.Index]
		if caches[h.Index] == nil {
			caches[h.Index] = map[uint64]string{}
		}
		rel := ix.RelPathCached(h.Slot, caches[h.Index])
		hd := &ix.Header
		dir, name := index.DisplaySplit(hd, rel, env)
		online := env.Online(hd)
		r := Row{
			Name: name, Dir: dir, Size: ix.Sizes[h.Slot], MTime: ix.MTimes[h.Slot],
			IsDir: ix.Flags[h.Slot]&index.FlagDir != 0, NoMeta: ix.Flags[h.Slot]&index.FlagNoMeta != 0,
			Offline: !online, Kind: hd.SourceKind, Label: hd.Label,
		}
		switch hd.SourceKind {
		case index.KindPhone:
			if !online {
				r.Dir += " · offline"
			}
			r.PhoneSerial = strings.TrimPrefix(hd.SourceID, "adb-")
			r.PhonePath = strings.TrimRight(hd.RootPath, "/") + "/" + strings.ReplaceAll(rel, `\`, "/")
			r.CopyPath = "phone:/" + strings.ReplaceAll(rel, `\`, "/")
			r.NoMeta = false // a phone path isn't a file on this PC
		case index.KindDevice:
			if !online {
				r.Dir += " · offline"
			}
			r.CopyPath = index.DevicePath(hd, rel)
			r.NoMeta = false // nor is a portable device's
		case index.KindVolume:
			if root, mounted := env.Mounts[hd.SourceID]; mounted {
				r.LocalPath = index.JoinRoot(root, rel)
				r.CopyPath = r.LocalPath
			} else {
				r.CopyPath = index.JoinRoot(hd.LastMount, rel) // where it was last seen
			}
		default:
			r.LocalPath = index.JoinRoot(hd.RootPath, rel)
			r.CopyPath = r.LocalPath
		}
		rows = append(rows, r)
	}
	return rows
}

// ---- actions --------------------------------------------------------------------

func (m Model) selected() (Row, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return Row{}, false
	}
	return m.rows[m.cursor], true
}

// offlineMessage says why an action isn't possible and what to do.
func offlineMessage(r Row) string {
	if r.Kind == index.KindPhone {
		return fmt.Sprintf("Phone %q isn't connected — plug it in.", r.Label)
	}
	if r.Kind == index.KindDevice {
		return fmt.Sprintf("%q isn't connected — plug it in.", r.Label)
	}
	return fmt.Sprintf("Drive %q isn't connected — plug it in.", r.Label)
}

// notLocalMessage explains what to do with a result that isn't a file on this PC.
func notLocalMessage(r Row) string {
	if r.Kind == index.KindDevice {
		return fmt.Sprintf("That result is on %s: open it in Explorer under This PC (Ctrl+Y copies its path).", r.Label)
	}
	return "That result is on a phone: press Ctrl+P to copy it to your Downloads folder."
}

func (m Model) doOpen() (tea.Model, tea.Cmd) {
	r, ok := m.selected()
	if !ok {
		return m, nil
	}
	if r.Offline {
		m.status = offlineMessage(r)
		return m, nil
	}
	if r.LocalPath == "" {
		m.status = notLocalMessage(r)
		return m, nil
	}
	open := m.cfg.Actions.Open
	return m, func() tea.Msg {
		if open == nil {
			return actionDoneMsg{"Opening files isn't available here."}
		}
		if err := open(r.LocalPath); err != nil {
			return actionDoneMsg{"Couldn't open it: " + err.Error()}
		}
		return actionDoneMsg{""}
	}
}

func (m Model) doShow() (tea.Model, tea.Cmd) {
	r, ok := m.selected()
	if !ok {
		return m, nil
	}
	if r.Offline {
		m.status = offlineMessage(r)
		return m, nil
	}
	if r.LocalPath == "" {
		m.status = notLocalMessage(r)
		return m, nil
	}
	show := m.cfg.Actions.ShowInFolder
	return m, func() tea.Msg {
		if show == nil {
			return actionDoneMsg{"Showing files in Explorer isn't available here."}
		}
		if err := show(r.LocalPath); err != nil {
			return actionDoneMsg{"Couldn't open Explorer: " + err.Error()}
		}
		return actionDoneMsg{""}
	}
}

func (m Model) doCopy() (tea.Model, tea.Cmd) {
	r, ok := m.selected()
	if !ok {
		return m, nil
	}
	cp := m.cfg.Actions.CopyText
	return m, func() tea.Msg {
		if cp == nil {
			return actionDoneMsg{"The clipboard isn't available here."}
		}
		if err := cp(r.CopyPath); err != nil {
			return actionDoneMsg{"Couldn't copy to the clipboard: " + err.Error()}
		}
		return actionDoneMsg{"Copied!"}
	}
}

func (m Model) doPhone() (tea.Model, tea.Cmd) {
	r, ok := m.selected()
	if !ok {
		return m, nil
	}
	if r.Kind != index.KindPhone {
		m.status = "Ctrl+P is for results on a phone."
		return m, nil
	}
	if r.Offline {
		m.status = offlineMessage(r)
		return m, nil
	}
	pull, show := m.cfg.Actions.PullPhone, m.cfg.Actions.ShowInFolder
	if pull == nil {
		m.status = "Copying from a phone isn't available here."
		return m, nil
	}
	m.status = "Copying " + r.Name + " from the phone…"
	return m, func() tea.Msg {
		local, err := pull(r.PhoneSerial, r.PhonePath)
		if err != nil {
			return actionDoneMsg{"Couldn't copy from the phone: " + err.Error()}
		}
		if show != nil {
			show(local)
		}
		return actionDoneMsg{"Saved to " + local}
	}
}

// Run shows the screen full-screen until the user quits.
func Run(cfg Config) error {
	_, err := tea.NewProgram(New(cfg), tea.WithAltScreen(), tea.WithContext(context.Background())).Run()
	return err
}

// summary line pieces (used by the view)
func sourcesText(total, offline int) string {
	s := fmt.Sprintf("%d %s", total, plural(total, "source", "sources"))
	if offline > 0 {
		s += fmt.Sprintf(" (%d offline)", offline)
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
