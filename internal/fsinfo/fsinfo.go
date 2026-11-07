// Package fsinfo answers questions about the file system that holds a path:
// what kind it is (NTFS, exFAT, FAT32...), how big its allocation units are,
// how precisely it stores modification times, whether it stores them in local
// time, whether names are case sensitive, and whether it can be written to.
//
// Why a copy needs this: "is the file at the destination the same as the
// source?" is usually answered by comparing sizes and modification times. But
// FAT32 keeps times in 2-second steps and in LOCAL time (so a daylight-saving
// change shifts every file by an hour), NTFS keeps 100-nanosecond steps in UTC,
// and a phone gives whole seconds. Comparing times correctly means knowing both
// sides (SameTime). The cluster size decides how much space small files really
// take, and case sensitivity decides whether a.jpg and A.jpg can live in the
// same folder.
//
// Results are cached per volume root for the life of the process.
package fsinfo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Info describes one volume.
type Info struct {
	Root          string        // `E:\`
	FileSystem    string        // "NTFS", "exFAT", "FAT32", "FAT", "ReFS", "UDF", "android", "device", ...
	ClusterBytes  int64         // allocation unit: SectorsPerCluster * BytesPerSector
	SectorBytes   int64         // physical sector size (alignment needed for unbuffered reads)
	TimeRes       time.Duration // how precisely modification times are stored; 0 = unknown
	LocalTime     bool          // true for FAT/FAT32: times are local, not UTC
	CaseSensitive bool          // false on Windows (unless a folder has NTFS's per-folder flag)
	ReadOnly      bool          // the volume itself is read-only (FILE_READ_ONLY_VOLUME)
	MaxNameUnits  int           // longest name in UTF-16 units (255 on NTFS/exFAT/FAT32)
}

// IsFAT reports whether the volume is FAT12/16/32 (not exFAT).
func (i Info) IsFAT() bool {
	return strings.EqualFold(i.FileSystem, "FAT32") || strings.EqualFold(i.FileSystem, "FAT")
}

// IsNTFS reports whether the volume is NTFS.
func (i Info) IsNTFS() bool { return strings.EqualFold(i.FileSystem, "NTFS") }

// IsWindowsFS reports whether names on this volume follow Windows' rules
// (forbidden characters, reserved names such as CON). Every volume a Windows
// PC mounts with a drive letter does; phones, portable devices and the Linux
// file systems the tests run on don't.
func (i Info) IsWindowsFS() bool {
	switch strings.ToLower(i.FileSystem) {
	case "android", "device", "posix", "":
		return false
	}
	return true
}

// ForFileSystem returns the Info a volume of that type normally has. It is
// what tests use, and what a lookup falls back to when Windows can't be asked.
func ForFileSystem(name string) Info {
	i := Info{FileSystem: name, ClusterBytes: 4096, SectorBytes: 512, MaxNameUnits: 255}
	switch strings.ToUpper(name) {
	case "NTFS", "REFS":
		i.TimeRes = 100 * time.Nanosecond
	case "EXFAT":
		i.TimeRes = 10 * time.Millisecond
		i.ClusterBytes = 32 << 10 // typical for 32 GB - 256 GB cards and sticks
	case "FAT32", "FAT":
		i.TimeRes = 2 * time.Second
		i.LocalTime = true
		i.ClusterBytes = 16 << 10
	case "UDF":
		i.TimeRes = time.Microsecond
	default:
		i.TimeRes = time.Second
	}
	return i
}

// ForPhone describes an Android phone read over adb: `stat -c %Y` gives whole
// seconds (UTC), and Linux names are case sensitive.
func ForPhone() Info {
	return Info{FileSystem: "android", TimeRes: time.Second, CaseSensitive: true, MaxNameUnits: 255, ClusterBytes: 4096, SectorBytes: 512}
}

// ForDevice describes a portable device read through Windows (MTP/PTP). Its
// times are whole seconds at best and of unknown time zone, so they are never
// trusted for "already there" decisions (TimeRes 0 = unknown).
func ForDevice() Info {
	return Info{FileSystem: "device", CaseSensitive: true, MaxNameUnits: 255, ClusterBytes: 4096, SectorBytes: 512}
}

var (
	mu    sync.Mutex
	cache = map[string]Info{}

	// lookup asks the system about the volume whose root is root. Tests
	// replace it (see SetLookupForTest).
	lookup = systemLookup
	// rootOf maps an existing path to its volume root.
	rootOf = volumeRoot
)

// ErrUnknown means the volume could not be examined.
var ErrUnknown = errors.New("couldn't examine the volume")

// Of returns the Info of the volume holding path. The path does not have to
// exist yet: its nearest existing ancestor is used (a copy's destination folder
// is usually created later).
func Of(path string) (Info, error) {
	mu.Lock()
	rootFn, lookFn := rootOf, lookup
	mu.Unlock()
	root, err := rootFn(NearestExisting(path))
	if err != nil {
		return Info{}, err
	}
	key := strings.ToLower(root)
	mu.Lock()
	if i, ok := cache[key]; ok {
		mu.Unlock()
		return i, nil
	}
	mu.Unlock()
	i, err := lookFn(root)
	if err != nil {
		return Info{}, err
	}
	i.Root = root
	mu.Lock()
	cache[key] = i
	mu.Unlock()
	return i, nil
}

// OrDefault is Of, falling back to a cautious description when the volume
// can't be examined: times are compared as tolerantly as on FAT, so a copy
// still works and never trusts a time it can't interpret.
func OrDefault(path string) Info {
	if i, err := Of(path); err == nil {
		return i
	}
	i := ForFileSystem("")
	i.TimeRes = 2 * time.Second
	return i
}

// SetLookupForTest makes Of answer with fn for every volume and clears the
// cache. Every path maps to the root of its drive letter (or "/" without one).
// It returns a function that restores the real lookup.
func SetLookupForTest(fn func(root string) (Info, error)) (restore func()) {
	mu.Lock()
	old, oldRoot := lookup, rootOf
	lookup = fn
	rootOf = func(p string) (string, error) {
		if v := filepath.VolumeName(p); v != "" {
			return v + `\`, nil
		}
		return string(filepath.Separator), nil
	}
	cache = map[string]Info{}
	mu.Unlock()
	return func() {
		mu.Lock()
		lookup, rootOf = old, oldRoot
		cache = map[string]Info{}
		mu.Unlock()
	}
}

// NearestExisting returns path, or its closest ancestor that exists.
func NearestExisting(path string) string {
	p := filepath.Clean(path)
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// ---------------------------------------------------------------------------
// Comparing modification times
// ---------------------------------------------------------------------------

// hour is the size of a daylight-saving shift, in nanoseconds.
const hour = int64(time.Hour)

// Tolerance is how far apart two modification times may be and still mean the
// same moment: the coarser of the two file systems' resolutions, as tolerant
// as FAT (2 s) if either resolution is unknown, and at least one second if
// either time is "coarse" (known to whole seconds only, for example because it
// came from a phone or from an index written before exact times existed).
func Tolerance(src, dst Info, coarse bool) time.Duration {
	tol := max(src.TimeRes, dst.TimeRes)
	if src.TimeRes == 0 || dst.TimeRes == 0 {
		tol = max(tol, 2*time.Second)
	}
	if coarse && tol < time.Second {
		tol = time.Second
	}
	return tol
}

// SameTime reports whether two modification times a and b (nanoseconds since
// 1970, UTC as Windows reports them) denote the same moment, allowing for the
// coarser of the two file systems.
//
// dstShift reports the special case of a difference of exactly one hour
// (within the tolerance) when either side stores LOCAL time (FAT/FAT32): that
// is what a daylight-saving change does to every file on such a drive, although
// the files themselves did not change. The caller decides what to do with it:
// the default compare mode treats such files as unchanged when their sizes
// match, strict mode doesn't.
func SameTime(a, b int64, src, dst Info, coarse bool) (same bool, dstShift bool) {
	tol := int64(Tolerance(src, dst, coarse))
	d := a - b
	if d < 0 {
		d = -d
	}
	if d <= tol {
		return true, false
	}
	if src.LocalTime || dst.LocalTime {
		off := d - hour
		if off < 0 {
			off = -off
		}
		if off <= tol {
			return false, true
		}
	}
	return false, false
}
