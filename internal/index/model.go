// Package index is DriveDex's in-memory and on-disk model of "every file on a
// drive".
//
// Instead of storing full paths, every entry stores its own name plus the ID
// of its parent folder - exactly how NTFS keeps files internally. That makes
// the index small, makes renaming a folder a one-entry change, and means a
// full path is only built for the few results we actually show.
//
// The index is kept as a "struct of arrays" (one slice per field) because
// searching only needs the lowercase names - reading them back-to-back is much
// friendlier to the CPU cache than hopping between big structs.
package index

import (
	"runtime"
	"strings"
	"sync"
	"time"
)

// Entry flags. They are stored as one byte per entry.
const (
	FlagDir     uint8 = 1 << 0
	FlagHidden  uint8 = 1 << 1
	FlagSystem  uint8 = 1 << 2
	FlagNoMeta  uint8 = 1 << 3 // size/mtime unknown (filled lazily on display)
	FlagDeleted uint8 = 1 << 7 // tombstone, in-memory only; never written to disk
)

// Entry is one file or folder.
type Entry struct {
	ID       uint64 // unique within one index. Walkers: sequential from 1. NTFS: file reference number.
	ParentID uint64 // 0 = direct child of the root. The root itself is never stored as an entry.
	Name     string // single path component, original case, UTF-8
	Size     int64  // bytes; -1 = unknown; 0 for directories
	ModTime  int64  // unix seconds; 0 = unknown
	Flags    uint8
}

// IsDir reports whether the entry is a folder.
func (e Entry) IsDir() bool { return e.Flags&FlagDir != 0 }

// Header describes where an index came from. It is stored as JSON inside the
// .ddx file so new fields can be added without breaking old files.
type Header struct {
	FormatVersion int       `json:"format_version"` // 1
	SourceKind    string    `json:"source_kind"`    // "folder" | "volume" | "phone" | "device"
	SourceID      string    `json:"source_id"`      // "dir-<12 hex>" | "vol-1A2B3C4D" | "adb-<serial>" | "dev-<12 hex>"
	Label         string    `json:"label"`          // volume label, device product name, or phone model
	FileSystem    string    `json:"file_system"`    // "NTFS" | "exFAT" | "FAT32" | "android" | ...
	RootPath      string    `json:"root_path"`      // folder sources: absolute path; phone: "/sdcard"
	LastMount     string    `json:"last_mount"`     // last seen drive root like "E:\" (volume) or adb serial (phone)
	Scanner       string    `json:"scanner"`        // "walk" | "parallel" | "mft" | "adb"
	EntryCount    int       `json:"entry_count"`
	TotalBytes    int64     `json:"total_bytes"` // volume capacity, if known
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	ScanMillis    int64     `json:"scan_millis"`
	UsnJournalID  uint64    `json:"usn_journal_id,omitempty"` // stage 6
	NextUsn       int64     `json:"next_usn,omitempty"`       // stage 6
}

// Source kinds used in Header.SourceKind.
const (
	KindFolder = "folder"
	KindVolume = "volume"
	KindPhone  = "phone"
	// KindDevice is a portable device read through Windows (MTP/PTP): a phone
	// in "File transfer" mode, an iPhone, a camera. SourceID "dev-<12 hex>".
	KindDevice = "device"
)

// Index is the whole file list of one source. All slices have the same length
// (one slot per entry). Slots are positions in the slices; they are NOT the
// same thing as IDs.
type Index struct {
	Header  Header
	IDs     []uint64
	Parents []uint64
	Names   []string
	Lower   []string // lowercase names, used for matching
	Sizes   []int64
	MTimes  []int64
	Flags   []uint8

	byID    map[uint64]int32 // ID -> slot
	deleted int              // number of tombstoned slots
}

// New returns an empty index.
func New() *Index { return NewWithCapacity(0) }

// NewWithCapacity returns an empty index with room for n entries, which avoids
// repeated slice growth when the final size is known (e.g. when loading).
func NewWithCapacity(n int) *Index {
	return &Index{
		IDs:     make([]uint64, 0, n),
		Parents: make([]uint64, 0, n),
		Names:   make([]string, 0, n),
		Lower:   make([]string, 0, n),
		Sizes:   make([]int64, 0, n),
		MTimes:  make([]int64, 0, n),
		Flags:   make([]uint8, 0, n),
		byID:    make(map[uint64]int32, n),
	}
}

// Len is the number of slots, including tombstones that have not been
// compacted away yet.
func (ix *Index) Len() int { return len(ix.IDs) }

// LiveLen is the number of entries that are not deleted.
func (ix *Index) LiveLen() int { return len(ix.IDs) - ix.deleted }

// Add appends an entry.
func (ix *Index) Add(e Entry) {
	ix.byID[e.ID] = int32(len(ix.IDs))
	ix.IDs = append(ix.IDs, e.ID)
	ix.Parents = append(ix.Parents, e.ParentID)
	ix.Names = append(ix.Names, e.Name)
	ix.Lower = append(ix.Lower, strings.ToLower(e.Name))
	ix.Sizes = append(ix.Sizes, e.Size)
	ix.MTimes = append(ix.MTimes, e.ModTime)
	ix.Flags = append(ix.Flags, e.Flags)
}

// Slot returns the slot holding the entry with the given ID.
func (ix *Index) Slot(id uint64) (int, bool) {
	s, ok := ix.byID[id]
	return int(s), ok
}

// Entry returns a copy of the entry stored in a slot.
func (ix *Index) Entry(slot int) Entry {
	return Entry{
		ID:       ix.IDs[slot],
		ParentID: ix.Parents[slot],
		Name:     ix.Names[slot],
		Size:     ix.Sizes[slot],
		ModTime:  ix.MTimes[slot],
		Flags:    ix.Flags[slot],
	}
}

// Upsert inserts the entry, or replaces the existing entry with the same ID.
// It is idempotent, and "later wins" - which is exactly the rule needed to
// replay the NTFS change journal (create, rename and move are all "set this
// entry's name and parent").
func (ix *Index) Upsert(e Entry) {
	slot, ok := ix.byID[e.ID]
	if !ok {
		ix.Add(e)
		return
	}
	ix.Parents[slot] = e.ParentID
	ix.Names[slot] = e.Name
	ix.Lower[slot] = strings.ToLower(e.Name)
	ix.Sizes[slot] = e.Size
	ix.MTimes[slot] = e.ModTime
	ix.Flags[slot] = e.Flags &^ FlagDeleted
}

// Delete tombstones the entry with the given ID. It reports whether the entry
// existed. Tombstones are removed for good by Compact.
func (ix *Index) Delete(id uint64) bool {
	slot, ok := ix.byID[id]
	if !ok {
		return false
	}
	ix.Flags[slot] |= FlagDeleted
	ix.deleted++
	delete(ix.byID, id)
	return true
}

// Compact removes tombstones. Call it before saving.
func (ix *Index) Compact() {
	if ix.deleted == 0 {
		return
	}
	w := 0
	for r := range ix.IDs {
		if ix.Flags[r]&FlagDeleted != 0 {
			continue
		}
		ix.IDs[w] = ix.IDs[r]
		ix.Parents[w] = ix.Parents[r]
		ix.Names[w] = ix.Names[r]
		ix.Lower[w] = ix.Lower[r]
		ix.Sizes[w] = ix.Sizes[r]
		ix.MTimes[w] = ix.MTimes[r]
		ix.Flags[w] = ix.Flags[r]
		w++
	}
	ix.IDs = ix.IDs[:w]
	ix.Parents = ix.Parents[:w]
	ix.Names = ix.Names[:w]
	ix.Lower = ix.Lower[:w]
	ix.Sizes = ix.Sizes[:w]
	ix.MTimes = ix.MTimes[:w]
	ix.Flags = ix.Flags[:w]
	ix.deleted = 0
	ix.rebuildByID()
}

// rebuildByID recomputes the ID -> slot map from the slices.
func (ix *Index) rebuildByID() {
	ix.byID = make(map[uint64]int32, len(ix.IDs))
	for slot, id := range ix.IDs {
		if ix.Flags[slot]&FlagDeleted == 0 {
			ix.byID[id] = int32(slot)
		}
	}
}

// fillLower computes Lower from Names using all CPU cores. Used after loading
// a file, where Names was filled directly.
//
// PERF: lowercasing a million names is embarrassingly parallel, so we split
// the slots into one chunk per CPU.
func (ix *Index) fillLower() {
	n := len(ix.Names)
	ix.Lower = make([]string, n)
	workers := runtime.NumCPU()
	if workers > n {
		workers = 1
	}
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for i := start; i < end; i++ {
				ix.Lower[i] = strings.ToLower(ix.Names[i])
			}
		}(start, end)
	}
	wg.Wait()
}

// Builder is used by scanners to construct an Index. It is NOT safe for
// concurrent use: parallel scanners funnel their results into one collector
// goroutine that owns the builder.
type Builder struct {
	ix      *Index
	discard bool
	count   int64
}

// NewBuilder returns a builder that stores entries in a fresh Index whose
// header starts as h.
func NewBuilder(h Header) *Builder {
	ix := New()
	ix.Header = h
	return &Builder{ix: ix}
}

// NewDiscardBuilder returns a builder that only counts entries and keeps
// nothing. It is for "count-only" scans, which should not use memory
// proportional to the size of the drive.
func NewDiscardBuilder() *Builder { return &Builder{ix: New(), discard: true} }

// Add records one entry.
func (b *Builder) Add(e Entry) {
	b.count++
	if !b.discard {
		b.ix.Add(e)
	}
}

// Len is the number of entries added so far.
func (b *Builder) Len() int64 { return b.count }

// Index returns the index being built. For a discard builder it is empty.
func (b *Builder) Index() *Index { return b.ix }
