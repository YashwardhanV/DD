// Package shellint adds DriveDex to the Windows Explorer right-click menu:
//
//	right-click a folder, a drive, or empty space in a folder
//	    -> "Search with DriveDex"   (opens the search screen limited to that place)
//	right-click a drive
//	    -> "Index with DriveDex"    (indexes it, and waits for Enter so the window stays)
//
// Everything is stored under HKEY_CURRENT_USER\Software\Classes, so no
// administrator rights are needed and other users of the PC are unaffected.
//
// On Windows 11 these entries appear under "Show more options" (or hold Shift
// while right-clicking): the new compact menu can only be extended by packaged
// apps, which is out of scope.
package shellint

import (
	"fmt"
	"strings"
)

// ClassesRoot is where per-user shell registrations live, under HKEY_CURRENT_USER.
const ClassesRoot = `Software\Classes`

// Entry is one right-click menu item.
type Entry struct {
	// Key is the registry key, relative to HKEY_CURRENT_USER.
	Key string
	// Title is the text shown in the menu.
	Title string
	// Command is the full command line Explorer runs.
	Command string
	// Icon is the icon reference ("<exe>",0).
	Icon string
}

// quoteArg wraps a path placeholder (%1, %V) in quotes for a command line.
//
// A trailing SPACE is added inside the quotes on purpose. Explorer hands over
// a drive root as `E:\`, and in `"E:\"` Windows reads the final `\"` as an
// escaped quote: the program would receive `E:"` with the rest of the command
// line glued on. With `"E:\ "` the backslash is no longer next to the quote.
// DriveDex trims surrounding spaces from paths, so the space is harmless for
// ordinary folders.
func quoteArg(placeholder string) string { return `"` + placeholder + ` "` }

// Entries returns the four menu items for the program at exe (an absolute path).
func Entries(exe string) []Entry {
	q := `"` + exe + `"`
	icon := q + ",0"
	search := func(key, placeholder string) Entry {
		return Entry{
			Key:     ClassesRoot + `\` + key,
			Title:   "Search with DriveDex",
			Command: q + " tui --scope " + quoteArg(placeholder),
			Icon:    icon,
		}
	}
	return []Entry{
		search(`Directory\shell\DriveDex`, "%1"),
		search(`Directory\Background\shell\DriveDex`, "%V"),
		search(`Drive\shell\DriveDex`, "%1"),
		{
			Key:     ClassesRoot + `\Drive\shell\DriveDexIndex`,
			Title:   "Index with DriveDex",
			Command: q + " index " + quoteArg("%1") + " --pause",
			Icon:    icon,
		},
	}
}

// Registry is the small part of the Windows registry this package needs
// (always under HKEY_CURRENT_USER). Tests use an in-memory fake.
type Registry interface {
	// SetValue creates the key if needed and sets a string value ("" = the key's (Default) value).
	SetValue(key, name, value string) error
	// GetValue reads a string value; exists is false if the key or value is missing.
	GetValue(key, name string) (value string, exists bool, err error)
	// DeleteTree removes a key and everything below it; removed is false if it wasn't there.
	DeleteTree(key string) (removed bool, err error)
}

// commandKey is the "command" subkey of a menu entry.
func commandKey(key string) string { return key + `\command` }

// Install writes all four entries. It is safe to run again (it overwrites
// them), which is also how a moved program is repaired.
func Install(reg Registry, exe string) error {
	for _, e := range Entries(exe) {
		if err := reg.SetValue(e.Key, "", e.Title); err != nil {
			return fmt.Errorf("writing %s: %w", e.Key, err)
		}
		if err := reg.SetValue(e.Key, "Icon", e.Icon); err != nil {
			return fmt.Errorf("writing %s: %w", e.Key, err)
		}
		if err := reg.SetValue(commandKey(e.Key), "", e.Command); err != nil {
			return fmt.Errorf("writing %s: %w", commandKey(e.Key), err)
		}
	}
	return nil
}

// Result describes what Uninstall did with one entry.
type Result struct {
	Key     string
	Removed bool
	Skipped string // why an entry was left alone ("" if it was removed or absent)
}

// Uninstall removes exactly the four entries DriveDex creates - nothing else.
// Before deleting an entry it checks the entry really is DriveDex's (its
// command mentions drivedex), so a different program's key with the same name
// is never touched.
func Uninstall(reg Registry) ([]Result, error) {
	var results []Result
	for _, e := range Entries("drivedex") { // only the key names matter here
		r := Result{Key: e.Key}
		cmd, exists, err := reg.GetValue(commandKey(e.Key), "")
		if err != nil {
			return results, fmt.Errorf("reading %s: %w", e.Key, err)
		}
		switch {
		case !exists:
			// Maybe only part of it exists: remove a partial entry of ours (a key
			// with a DriveDex title) but don't touch anything else.
			if title, ok, _ := reg.GetValue(e.Key, ""); ok && title == e.Title {
				r.Removed, err = reg.DeleteTree(e.Key)
			}
		case !strings.Contains(strings.ToLower(cmd), "drivedex"):
			r.Skipped = "it isn't a DriveDex entry (its command is " + cmd + ")"
		default:
			r.Removed, err = reg.DeleteTree(e.Key)
		}
		if err != nil {
			return results, fmt.Errorf("removing %s: %w", e.Key, err)
		}
		results = append(results, r)
	}
	return results, nil
}

// State is how one menu entry looks in the registry.
type State int

const (
	Missing State = iota // not installed
	Current              // installed and pointing at this program
	Stale                // installed but pointing at a different path (the program was moved)
)

// EntryStatus pairs an entry with its state.
type EntryStatus struct {
	Entry   Entry
	State   State
	Command string // what the registry currently contains
}

// Status checks all four entries against the program at exe.
func Status(reg Registry, exe string) ([]EntryStatus, error) {
	var out []EntryStatus
	for _, e := range Entries(exe) {
		cmd, exists, err := reg.GetValue(commandKey(e.Key), "")
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", e.Key, err)
		}
		st := EntryStatus{Entry: e, Command: cmd}
		switch {
		case !exists:
			st.State = Missing
		case cmd == e.Command:
			st.State = Current
		default:
			st.State = Stale
		}
		out = append(out, st)
	}
	return out, nil
}
