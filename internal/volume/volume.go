// Package volume identifies the drives connected to this PC: drive letter,
// label, file system, size - and above all the volume SERIAL NUMBER.
//
// A drive letter is not an identity: a pen drive is E: today and G: tomorrow.
// The serial number is written to the drive when it is formatted and never
// changes, so DriveDex keys its saved indexes by serial and only looks up the
// current letter when it needs to show a path.
//
// The Windows-specific code lives in volume_windows.go; every other platform
// gets stubs (volume_other.go) so the rest of the program still compiles and
// can be tested anywhere.
package volume

import (
	"fmt"
	"regexp"
	"strings"
)

// Windows drive types (the values GetDriveType returns).
const (
	DriveRemovable uint32 = 2 // pen drives, SD cards
	DriveFixed     uint32 = 3 // internal and most USB hard disks
	DriveRemote    uint32 = 4 // network drives
)

// Volume describes one drive.
type Volume struct {
	Root       string // "E:\"
	Label      string
	FileSystem string // "NTFS", "exFAT", "FAT32", ...
	Serial     uint32
	DriveType  uint32
	TotalBytes uint64
	FreeBytes  uint64
	// Ready is false for a drive that exists but can't be read right now,
	// such as an empty card-reader slot. That is normal, not an error.
	Ready bool
}

// SourceID is the stable key of a volume's index file: "vol-1A2B3C4D".
//
// Known limitation: two cloned drives share a serial, and reformatting a drive
// gives it a new serial (so it gets a new index).
func SourceID(v Volume) string { return fmt.Sprintf("vol-%08X", v.Serial) }

// Letter returns the drive letter with a colon: "E:".
func (v Volume) Letter() string { return strings.TrimRight(v.Root, `\/`) }

// TypeName is a friendly name for the drive type.
func (v Volume) TypeName() string {
	switch v.DriveType {
	case DriveRemovable:
		return "Removable"
	case DriveFixed:
		return "Fixed"
	case DriveRemote:
		return "Network"
	default:
		return "Other"
	}
}

var driveRootRe = regexp.MustCompile(`^[A-Za-z]:[\\/]?$`)

// IsDriveRoot reports whether the user typed a whole drive ("E:", "E:\" or
// "e:/") rather than a folder on it.
func IsDriveRoot(path string) bool { return driveRootRe.MatchString(strings.TrimSpace(path)) }

// NormalizeLetter turns "e", "E:", "e:\" into "E:". ok is false if the text
// isn't a drive letter.
func NormalizeLetter(s string) (letter string, ok bool) {
	s = strings.TrimSpace(s)
	if len(s) == 1 {
		s += ":"
	}
	if !IsDriveRoot(s) {
		return "", false
	}
	return strings.ToUpper(s[:1]) + ":", true
}
