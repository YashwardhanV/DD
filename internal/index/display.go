package index

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Env describes what is connected right now, so a stored entry can be shown
// with the right drive letter (or marked offline).
type Env struct {
	// Mounts maps a volume SourceID ("vol-1A2B3C4D") to its current drive
	// root ("F:\"). Volumes that are not plugged in are simply absent.
	Mounts map[string]string
	// Phones is the set of phone SourceIDs ("adb-<serial>") that are connected
	// and authorized.
	Phones map[string]bool
	// Devices is the set of portable-device SourceIDs ("dev-<12 hex>") that are connected.
	Devices map[string]bool
}

// Online reports whether the source behind h can be reached right now.
// Folder sources are always treated as online.
func (e Env) Online(h *Header) bool {
	switch h.SourceKind {
	case KindVolume:
		_, ok := e.Mounts[h.SourceID]
		return ok
	case KindPhone:
		return e.Phones[h.SourceID]
	case KindDevice:
		return e.Devices[h.SourceID]
	default:
		return true
	}
}

// DisplayPath formats the path of a result for people (BUILD.md section 7.6).
// rel is the path relative to the source root, with `\` separators.
//
//	volume, plugged in   F:\Photos\2024\img.jpg           (current letter, found by serial)
//	volume, unplugged    [SANDISK 32GB · offline · last seen E:] \Photos\2024\img.jpg
//	phone                phone:Pixel 7/DCIM/Camera/IMG.jpg   (+ " · offline")
//	portable device      device:Galaxy S21\Internal storage\DCIM\IMG.jpg   (+ " · offline")
//	folder               <RootPath>\<rel>
func DisplayPath(h *Header, rel string, env Env) string {
	switch h.SourceKind {
	case KindVolume:
		if root, ok := env.Mounts[h.SourceID]; ok {
			return filepath.Join(root, osSeparators(rel))
		}
		return fmt.Sprintf(`[%s · offline · last seen %s] \%s`, volumeLabel(h), lastSeenLetter(h.LastMount), rel)
	case KindPhone:
		p := "phone:" + phoneLabel(h) + "/" + strings.ReplaceAll(rel, `\`, "/")
		if !env.Online(h) {
			p += " · offline"
		}
		return p
	case KindDevice:
		p := DevicePath(h, rel)
		if !env.Online(h) {
			p += " · offline"
		}
		return p
	default:
		return filepath.Join(h.RootPath, osSeparators(rel))
	}
}

// osSeparators converts the index's `\` separators to this OS's separator.
// (On Windows this changes nothing; it only matters when developing elsewhere.)
func osSeparators(rel string) string {
	return strings.ReplaceAll(rel, `\`, string(filepath.Separator))
}

func volumeLabel(h *Header) string {
	if h.Label != "" {
		return h.Label
	}
	return h.SourceID
}

// DevicePath is how a file on a portable device is named for people:
// "device:Galaxy S21\Internal storage\DCIM\IMG.jpg" - the way Explorer shows
// it under This PC. It is a name, not a path any program can open.
func DevicePath(h *Header, rel string) string {
	p := "device:" + phoneLabel(h)
	if rel != "" {
		p += `\` + rel
	}
	return p
}

func phoneLabel(h *Header) string {
	if h.Label != "" {
		return h.Label
	}
	return h.SourceID
}

// lastSeenLetter turns the stored last mount "E:\" into "E:".
func lastSeenLetter(lastMount string) string {
	if lastMount == "" {
		return "?"
	}
	return strings.TrimRight(lastMount, `\/`)
}

// DisplaySplit is DisplayPath cut into two parts for a two-colour display: the
// FOLDER part (always ending in a separator) and the file NAME. For example
// `E:\Docs\Jobs\` and `Resume.pdf`; for an unplugged drive
// `[SANDISK 32GB · offline · last seen E:] \Old\` and `resume_old.pdf`.
func DisplaySplit(h *Header, rel string, env Env) (dir, name string) {
	parent, name := "", rel
	if i := strings.LastIndexByte(rel, '\\'); i >= 0 {
		parent, name = rel[:i], rel[i+1:]
	}
	dir = DisplayPath(h, parent, env)
	dir = strings.TrimSuffix(dir, " · offline") // the row shows offline-ness itself
	sep := string(filepath.Separator)
	switch {
	case h.SourceKind == KindPhone:
		sep = "/"
	case h.SourceKind == KindVolume && !env.Online(h), h.SourceKind == KindDevice:
		sep = `\` // an unplugged drive's path is always shown Windows-style, and so is a device's
	}
	if !strings.HasSuffix(dir, sep) && !strings.HasSuffix(dir, `\`) && !strings.HasSuffix(dir, "/") {
		dir += sep
	}
	return dir, name
}

// LocalPath returns where an entry is on THIS PC right now, so it can be
// opened or copied. ok is false when it can't be reached: an unplugged drive,
// or a phone or portable device (whose paths are not files on this PC).
func LocalPath(h *Header, rel string, env Env) (path string, ok bool) {
	switch h.SourceKind {
	case KindVolume:
		root, mounted := env.Mounts[h.SourceID]
		if !mounted {
			return "", false
		}
		return JoinRoot(root, rel), true
	case KindFolder:
		return JoinRoot(h.RootPath, rel), true
	}
	return "", false
}

// CopyPath is the path to hand to people and scripts for a result: its real
// path when the drive is connected, the path where it was last seen when it
// isn't, and "phone:/<path>" for a phone file (which can be pasted straight
// into `drivedex copy`).
func CopyPath(h *Header, rel string, env Env) string {
	if p, ok := LocalPath(h, rel, env); ok {
		return p
	}
	switch h.SourceKind {
	case KindPhone:
		return "phone:/" + strings.ReplaceAll(rel, `\`, "/")
	case KindDevice:
		return DevicePath(h, rel)
	case KindVolume:
		return JoinRoot(h.LastMount, rel)
	}
	return JoinRoot(h.RootPath, rel)
}
