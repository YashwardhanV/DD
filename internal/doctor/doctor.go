// Package doctor explains why DriveDex (or a drive) might be slow or not
// working. It is split in two so it can be tested anywhere:
//
//   - the command layer collects Facts about this PC (is the agent running? is
//     the phone authorized? what kind of disk is E:?) - the only part that
//     touches the real machine;
//   - Build turns Facts into a Report of ✅ / ⚠️ / ❌ lines, each with a
//     one-line fix - plain logic, no I/O.
//
// Doctor only ever LOOKS. The one thing that writes to a drive, the speed
// test, is a separate, opt-in function (Bench) that asks first.
package doctor

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// Level is how serious a line is.
type Level int

const (
	OK   Level = iota // ✅ all good
	Warn              // ⚠️ works, but could be better
	Fail              // ❌ something is broken
	Info              // ℹ️ just so you know
)

func (l Level) glyph() string {
	switch l {
	case OK:
		return "✅"
	case Warn:
		return "⚠️ "
	case Fail:
		return "❌"
	default:
		return "ℹ️ "
	}
}

// Line is one finding.
type Line struct {
	Level Level
	Text  string
	Tip   string // a one-line fix; "" when there is nothing to do
}

// Section is a titled group of lines ("Phone", "Drive E:", ...).
type Section struct {
	Title string
	Lines []Line
}

// Report is everything doctor found.
type Report struct {
	Sections []Section
}

// Counts returns how many ✅, ⚠️ and ❌ lines there are.
func (r Report) Counts() (ok, warn, fail int) {
	for _, s := range r.Sections {
		for _, l := range s.Lines {
			switch l.Level {
			case OK:
				ok++
			case Warn:
				warn++
			case Fail:
				fail++
			}
		}
	}
	return
}

// Print writes the report.
func (r Report) Print(w io.Writer) {
	for _, s := range r.Sections {
		fmt.Fprintf(w, "\n%s\n", ui.Bold(s.Title))
		for _, l := range s.Lines {
			fmt.Fprintf(w, "  %s %s\n", l.Level.glyph(), l.Text)
			if l.Tip != "" {
				fmt.Fprintf(w, "       %s\n", ui.Dim("→ "+l.Tip))
			}
		}
	}
	ok, warn, fail := r.Counts()
	fmt.Fprintf(w, "\n%d ok, %d to look at, %d problem%s.\n", ok, warn, fail, map[bool]string{true: "", false: "s"}[fail == 1])
}

// ---- facts ---------------------------------------------------------------------------

// Media says what kind of storage a drive is.
type Media int

const (
	MediaUnknown Media = iota
	MediaFlash         // pen drive, SSD, SD card: no moving parts
	MediaHDD           // spinning hard disk
)

// JournalState is the NTFS change-journal state of a drive.
type JournalState int

const (
	JournalUnchecked JournalState = iota // not looked at (not NTFS, or not Administrator)
	JournalActive
	JournalInactive
	JournalError
)

// IndexFacts describes a drive's saved index.
type IndexFacts struct {
	UpdatedAt time.Time
	Entries   int
	Scanner   string // "walk" | "parallel" | "mft"
}

// DriveFacts describes one connected, ready drive.
type DriveFacts struct {
	Letter     string // "E:"
	Label      string
	FileSystem string
	Bus        string // "USB", "SATA", "NVMe"...; "" = unknown
	Media      Media
	Index      *IndexFacts // nil = no saved index
	Journal    JournalState
	// USB is how a USB drive is linked (speed now, speed possible); nil for
	// other drives or when Windows didn't say.
	USB *volume.USBLink
}

// PhoneFacts is one device adb listed.
type PhoneFacts struct {
	Serial string
	Name   string
	State  string // "device", "unauthorized", "offline", ...
}

// ADBFacts is what is known about adb.
type ADBFacts struct {
	Found   bool
	Path    string
	Version string // first line of `adb version`
	Err     error  // adb exists but could not list devices
	Phones  []PhoneFacts
}

// Facts is everything doctor needs to know about this PC.
type Facts struct {
	Version    string
	Now        time.Time
	StaleAfter time.Duration

	IndexDir    string
	IndexDirErr error // nil = writable
	IndexCount  int
	ConfigPath  string
	ConfigErr   error

	AgentKnown     bool // false when the question can't be asked (not Windows)
	AgentRunning   bool
	StartupKnown   bool
	StartupEnabled bool
	MenuKnown      bool
	MenuInstalled  int // entries present and correct
	MenuTotal      int

	Elevated bool
	ADB      ADBFacts

	Drives    []DriveFacts
	DrivesErr error

	// Ports counts the PC's USB 3 sockets (Known is false off Windows).
	Ports volume.PortSummary
}

// ---- building the report ----------------------------------------------------------------

// Build turns facts into a report.
func Build(f Facts) Report {
	var r Report
	r.Sections = append(r.Sections, driveDexSection(f), privilegeSection(f), phoneSection(f))
	if f.Ports.Known {
		r.Sections = append(r.Sections, usbSection(f.Ports))
	}
	r.Sections = append(r.Sections, drivesSections(f)...)
	return r
}

func driveDexSection(f Facts) Section {
	s := Section{Title: "DriveDex"}
	add := func(l Level, text, tip string) { s.Lines = append(s.Lines, Line{l, text, tip}) }

	add(OK, "version "+f.Version, "")

	switch {
	case f.IndexDirErr != nil:
		add(Fail, "can't save indexes in "+f.IndexDir+": "+f.IndexDirErr.Error(),
			"make sure the folder is writable, or set DRIVEDEX_DATA_DIR to another folder")
	case f.IndexCount == 0:
		add(Info, "index folder is writable ("+f.IndexDir+"); nothing indexed yet", "index a drive with: drivedex index E:")
	default:
		add(OK, fmt.Sprintf("index folder is writable (%s) - %d %s saved", f.IndexDir, f.IndexCount, plural(f.IndexCount, "index", "indexes")), "")
	}

	if f.ConfigErr != nil {
		add(Fail, "settings file has a problem: "+f.ConfigErr.Error(), "fix it with 'drivedex config edit', or delete "+f.ConfigPath+" to start again")
	} else {
		add(OK, "settings file is fine ("+f.ConfigPath+")", "")
	}

	if f.AgentKnown {
		if f.AgentRunning {
			add(OK, "background agent is running", "")
		} else {
			add(Info, "background agent is not running", "start it with: drivedex agent start (it indexes pen drives when you plug them in)")
		}
	}
	if f.StartupKnown {
		if f.StartupEnabled {
			add(OK, "the agent starts with Windows", "")
		} else {
			add(Info, "the agent does not start with Windows", "turn it on with: drivedex startup enable")
		}
	}
	if f.MenuKnown {
		switch {
		case f.MenuInstalled == f.MenuTotal && f.MenuTotal > 0:
			add(OK, "right-click menu is installed", "")
		case f.MenuInstalled == 0:
			add(Info, "right-click menu is not installed", "add it with: drivedex shell install")
		default:
			add(Warn, "right-click menu is partly installed or points to another drivedex.exe", "repair it with: drivedex shell install")
		}
	}
	return s
}

func privilegeSection(f Facts) Section {
	s := Section{Title: "Privileges"}
	if f.Elevated {
		s.Lines = append(s.Lines, Line{OK, "running as Administrator: NTFS turbo mode is available (reads the drive's file table directly - about 10x faster than walking folders)", ""})
	} else {
		s.Lines = append(s.Lines, Line{Info, "not running as Administrator: NTFS turbo mode (reading the drive's file table directly, about 10x faster) is off; normal scanning still works",
			"for turbo mode, run DriveDex in a PowerShell opened with \"Run as administrator\""})
	}
	return s
}

func phoneSection(f Facts) Section {
	s := Section{Title: "Phone"}
	add := func(l Level, text, tip string) { s.Lines = append(s.Lines, Line{l, text, tip}) }
	a := f.ADB
	if !a.Found {
		add(Info, "adb (Android Debug Bridge) was not found: phones are still indexed through Windows (\"File transfer\" mode), just more slowly",
			"optional, for faster phone indexing: run 'drivedex index phone' to see the setup steps (install Android \"platform-tools\", turn on USB debugging)")
		return s
	}
	v := a.Version
	if v == "" {
		v = "version unknown"
	}
	add(OK, "adb found: "+a.Path+" ("+v+")", "")
	if a.Err != nil {
		add(Fail, "adb couldn't list phones: "+a.Err.Error(), "try: adb kill-server, then run doctor again")
		return s
	}
	if len(a.Phones) == 0 {
		add(Info, "no phone connected over USB debugging", "plug it in by USB, turn on USB debugging, and tap Allow on the phone (without USB debugging, phones are read through Windows instead)")
		return s
	}
	for _, p := range a.Phones {
		name := p.Name
		if name == "" {
			name = p.Serial
		}
		switch p.State {
		case "device":
			add(OK, fmt.Sprintf("%s is connected and authorized", name), "")
		case "unauthorized":
			add(Fail, fmt.Sprintf("%s is waiting for permission", name), "unlock the phone and tap \"Allow\" on the \"Allow USB debugging?\" prompt (tick \"Always allow\")")
		case "offline":
			add(Warn, fmt.Sprintf("%s is listed as offline", name), "unplug and replug the cable, or run: adb kill-server")
		default:
			add(Warn, fmt.Sprintf("%s is in state %q", name, p.State), "unplug and replug the cable; if it persists run: adb kill-server")
		}
	}
	return s
}

func drivesSections(f Facts) []Section {
	if f.DrivesErr != nil {
		return []Section{{Title: "Drives", Lines: []Line{{Warn, "couldn't list drives: " + f.DrivesErr.Error(), "this check only works on Windows"}}}}
	}
	if len(f.Drives) == 0 {
		return []Section{{Title: "Drives", Lines: []Line{{Info, "no ready drives found", ""}}}}
	}
	var out []Section
	for _, d := range f.Drives {
		out = append(out, driveSection(d, f))
	}
	return out
}

func driveSection(d DriveFacts, f Facts) Section {
	title := "Drive " + d.Letter
	if d.Label != "" {
		title += "  " + d.Label
	}
	s := Section{Title: title}
	add := func(l Level, text, tip string) { s.Lines = append(s.Lines, Line{l, text, tip}) }

	switch strings.ToUpper(d.FileSystem) {
	case "NTFS":
		add(OK, "file system NTFS", "")
	case "EXFAT":
		add(OK, "file system exFAT", "")
	case "FAT32", "FAT":
		add(Warn, "file system "+d.FileSystem+": can't store a single file bigger than 4 GiB",
			"if you need big files, exFAT or NTFS can; reformatting erases the drive, so copy everything off first")
	case "":
		add(Info, "file system unknown", "")
	default:
		add(Info, "file system "+d.FileSystem, "")
	}

	bus := d.Bus
	if bus == "" {
		bus = "unknown connection"
	}
	switch d.Media {
	case MediaHDD:
		add(Info, "connected by "+bus+", a spinning hard disk: DriveDex reads with few workers so the head isn't thrashed", "")
	case MediaFlash:
		add(OK, "connected by "+bus+", flash storage (no moving parts): DriveDex reads with many workers at once", "")
	default:
		add(Info, "connected by "+bus+"; couldn't tell if it is a hard disk or flash, so DriveDex uses a middle setting", "")
	}

	if d.USB != nil {
		text, tip := volume.Advice(*d.USB, f.Ports)
		switch v := d.USB.Judge(); {
		case v.Slow():
			add(Warn, text, tip)
		case v == volume.VerdictFull:
			add(OK, text, "")
		case v == volume.VerdictSlowMaybe:
			add(Info, text, tip)
		}
	}

	if d.Index == nil {
		add(Warn, "not indexed yet", "index it with: drivedex index "+d.Letter)
	} else {
		age := f.Now.Sub(d.Index.UpdatedAt)
		text := fmt.Sprintf("indexed %s - %s entries (%s scan)", ui.Ago(d.Index.UpdatedAt, f.Now), ui.Thousands(int64(d.Index.Entries)), d.Index.Scanner)
		if f.StaleAfter > 0 && age > f.StaleAfter {
			add(Warn, text+", older than your stale_after setting", "refresh it with: drivedex index "+d.Letter)
		} else {
			add(OK, text, "")
		}
	}

	if strings.EqualFold(d.FileSystem, "NTFS") {
		switch d.Journal {
		case JournalActive:
			add(OK, "NTFS change journal is active: later refreshes only read what changed", "")
		case JournalInactive:
			add(Warn, "NTFS change journal is off: every refresh has to re-read the whole drive",
				"switch it on (Administrator) with: drivedex journal enable "+d.Letter)
		case JournalError:
			add(Warn, "couldn't read the NTFS change journal", "run with --debug for details")
		default:
			if !f.Elevated {
				add(Info, "NTFS change journal not checked (needs Administrator)", "")
			}
		}
	}
	return s
}

func usbSection(p volume.PortSummary) Section {
	s := Section{Title: "USB ports"}
	switch {
	case p.USB3 == 0:
		s.Lines = append(s.Lines, Line{Info, "this PC reports no USB 3 ports: drives top out at USB 2.0 speed (about 40 MB/s) here",
			"no software can make a USB 2.0 port faster; a USB 3 PCIe card (desktop) or a newer PC can"})
	default:
		text := fmt.Sprintf("this PC has %d USB 3 %s", p.USB3, plural(p.USB3, "port", "ports"))
		if p.USB3TypeC > 0 {
			text += fmt.Sprintf(" (%d USB-C)", p.USB3TypeC)
		}
		if p.USB2OnlyKnown && p.USB2Only > 0 {
			text += fmt.Sprintf(" and %d USB 2.0-only", p.USB2Only)
		}
		s.Lines = append(s.Lines, Line{OK, text, "to find which socket is which: drivedex ports --watch, then move a drive between them"})
	}
	return s
}

// Tips are printed at the end. They are information only - doctor changes nothing.
var Tips = []string{
	`Write caching: Device Manager → Disk drives → your drive → Properties → Policies → "Better performance" makes copies to it faster, but then you must always use "Safely Remove Hardware" before unplugging.`,
	"Windows Defender scans every file you copy. That is normal and good - don't turn it off. Expect copies of many small files to be slower because of it.",
	"Many tiny files copy much faster if you zip them into one file first (one big file = one long fast write instead of thousands of slow ones).",
}

// PrintTips writes the "Tips" section.
func PrintTips(w io.Writer) {
	fmt.Fprintf(w, "\n%s %s\n", ui.Bold("Tips"), ui.Dim("(information only - doctor changes nothing)"))
	for _, t := range Tips {
		fmt.Fprintf(w, "  • %s\n", t)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
