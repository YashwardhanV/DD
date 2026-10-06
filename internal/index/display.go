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
