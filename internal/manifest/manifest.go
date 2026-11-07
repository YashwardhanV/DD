// Package manifest is the copy manifest: a record, kept on the destination
// drive, of every file DriveDex placed there — where it came from, what the
// source looked like at that moment, and a content hash.
//
// It lives in <dstRoot>\.drivedex\ (a hidden, system folder) and travels with
// the drive, so any PC running DriveDex can use it. It is not encrypted: it
// must be readable anywhere.
//
//	manifest.ddm      the compacted manifest, written atomically
//	manifest.journal  records added during a run (append-only, CRC-framed)
//	lock              held exclusively while a copy or sync writes here
//	trash\<run>\      recoverable trash (package trash)
//
// A record is appended to the journal only after its file is in place (and
// verified, when verify is on). A file copied but not yet recorded simply has
// no record, so the next run compares it the ordinary way: safe (INV-1).
package manifest

import (
	"strings"
)

// DirName is the manifest folder inside a destination.
const DirName = ".drivedex"

// File names inside DirName.
const (
	ManifestFile = "manifest.ddm"
	JournalFile  = "manifest.journal"
	LockFile     = "lock"
	TrashDir     = "trash"
)

// FormatVersion is the manifest.ddm format written by this version.
const FormatVersion = 1

// Hash kinds.
const (
	HashNone uint8 = 0
	HashXXH3 uint8 = 1 // XXH3-128
)

// Record flags.
const (
	FlagVerifiedMedia uint8 = 1 << iota // read back from the drive without the cache
	FlagSanitised                       // the name was changed so Windows can store it
	FlagCaseRenamed                     // renamed because of a letter-case clash
	FlagAdopted                         // found already in place (same size and time), not copied by DriveDex
)

// Record is one file DriveDex placed (or adopted) in the destination.
type Record struct {
	Rel        string // destination path relative to dstRoot, `\`-separated
	SrcID      string // index source ID: vol-1A2B3C4D, adb-<serial>, dir-...
	SrcRel     string // source-relative path when it differs from Rel's (renamed names)
	SrcFileID  uint64 // NTFS file reference number, 0 if unknown
	SrcSize    int64
	SrcMTimeNs int64
	DstSize    int64 // the destination file right after the copy
	DstMTimeNs int64
	Hash       [16]byte
	HashKind   uint8
	SrcUSN     int64  // journal position of the source at copy time; 0 if not NTFS
	JournalID  uint64 // the source volume's journal ID; 0 if not NTFS
	CopiedAt   int64  // Unix seconds
	Flags      uint8
}

// Header is the JSON header of manifest.ddm.
type Header struct {
	FormatVersion int    `json:"format_version"`
	WrittenBy     string `json:"written_by,omitempty"`
	DstFS         string `json:"dst_fs,omitempty"`
	RecordCount   int    `json:"record_count"`
	DirCount      int    `json:"dir_count"`
}

// Manifest is the set of records of one destination, keyed by Rel.
type Manifest struct {
	Header Header
	recs   map[string]Record
}

// New returns an empty manifest.
func New() *Manifest {
	return &Manifest{Header: Header{FormatVersion: FormatVersion}, recs: map[string]Record{}}
}

// Key normalises a relative path the way records are stored: `\` separators,
// no leading or trailing separator.
func Key(rel string) string {
	return strings.Trim(strings.ReplaceAll(rel, "/", `\`), `\`)
}

// Len is the number of records.
func (m *Manifest) Len() int { return len(m.recs) }

// Get returns the record for a destination path.
func (m *Manifest) Get(rel string) (Record, bool) {
	r, ok := m.recs[Key(rel)]
	return r, ok
}

// Put adds or replaces a record.
func (m *Manifest) Put(r Record) {
	r.Rel = Key(r.Rel)
	m.recs[r.Rel] = r
}

// Delete removes a record.
func (m *Manifest) Delete(rel string) { delete(m.recs, Key(rel)) }

// Range calls fn for every record (in no particular order) until it returns false.
func (m *Manifest) Range(fn func(Record) bool) {
	for _, r := range m.recs {
		if !fn(r) {
			return
		}
	}
}

// Under returns the records at or below the folder prefix ("" = all).
func (m *Manifest) Under(prefix string) []Record {
	p := Key(prefix)
	var out []Record
	for k, r := range m.recs {
		if p == "" || k == p || (len(k) > len(p) && k[len(p)] == '\\' && strings.EqualFold(k[:len(p)], p)) {
			out = append(out, r)
		}
	}
	return out
}

// CountUnder is len(Under(prefix)) without building the slice.
func (m *Manifest) CountUnder(prefix string) int {
	p := Key(prefix)
	n := 0
	for k := range m.recs {
		if p == "" || k == p || (len(k) > len(p) && k[len(p)] == '\\' && strings.EqualFold(k[:len(p)], p)) {
			n++
		}
	}
	return n
}
