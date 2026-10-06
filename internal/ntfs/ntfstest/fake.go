// Package ntfstest is a tiny in-memory NTFS volume for tests: it has an MFT
// (a table of files), a change journal, NTFS's own metafiles and a "System
// Volume Information" folder, and it answers the same control codes as a real
// volume handle. It lets the MFT-enumeration and journal-refresh logic be
// tested on any machine, with no drive and no Administrator rights.
package ntfstest

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/yashwardhanv/dd/internal/ntfs"
)

// Journal reason bits used by the fake (a subset of the real ones).
const (
	reasonCreate    uint32 = 0x00000100
	reasonRenameOld uint32 = 0x00001000
	reasonRenameNew uint32 = 0x00002000
	reasonWrite     uint32 = 0x00000002
	reasonClose     uint32 = 0x80000000
)

type file struct {
	parent uint64
	name   string
	attrs  uint32
	usn    int64 // USN of the last change, as reported by enumeration
}

// Volume is the fake drive. Create it with New.
type Volume struct {
	mu sync.Mutex

	// RootFRN is the file reference number of the root folder (record 5, sequence 5).
	RootFRN uint64
	// JournalActive: when false, journal calls fail like on a drive with no journal.
	JournalActive bool
	JournalID     uint64
	// MaxRecordsPerCall, if > 0, limits how many records one call returns, so
	// tests exercise the "loop until done" logic with small batches.
	MaxRecordsPerCall int
	// Closed counts how many times the session was closed.
	Closed int
	// Calls counts DeviceIoControl calls by control code.
	Calls map[uint32]int

	files       map[uint64]*file
	nextRecord  uint64
	nextUsn     int64
	lowestValid int64
	log         []ntfs.Record // journal, in USN order
}

// FRN builds a file reference number from an MFT record number and a sequence number.
func FRN(record uint64, seq uint16) uint64 { return uint64(seq)<<48 | record }

// New returns a volume containing NTFS's metafiles, the root folder and a
// "System Volume Information" folder with a file inside it, with an active journal.
func New() *Volume {
	v := &Volume{
		RootFRN:       FRN(5, 5),
		JournalActive: true,
		JournalID:     0x01D9A0B1C2D3E4F5,
		Calls:         map[uint32]int{},
		files:         map[uint64]*file{},
		nextRecord:    64, // user files start well above the metafile range
		nextUsn:       4096,
		lowestValid:   4096,
	}
	// Metafiles: records 0-23, all "children" of the root like in real NTFS.
	for i, name := range []string{"$MFT", "$MFTMirr", "$LogFile", "$Volume", "$AttrDef", ".", "$Bitmap", "$Boot", "$BadClus", "$Secure", "$UpCase", "$Extend"} {
		frn := FRN(uint64(i), uint16(max(i, 1)))
		v.files[frn] = &file{parent: v.RootFRN, name: name, attrs: ntfs.AttrHidden | ntfs.AttrSystem}
	}
	v.files[v.RootFRN].attrs |= ntfs.AttrDirectory
	extend := FRN(11, 11)
	v.files[extend].attrs |= ntfs.AttrDirectory
	v.files[FRN(30, 1)] = &file{parent: extend, name: "$UsnJrnl", attrs: ntfs.AttrHidden | ntfs.AttrSystem} // inside $Extend
	v.files[FRN(31, 1)] = &file{parent: extend, name: "$ObjId", attrs: ntfs.AttrHidden | ntfs.AttrSystem}
	v.nextRecord = 64

	svi := v.Mkdir(0, "System Volume Information")
	v.Create(svi, "tracking.log")
	v.log, v.nextUsn, v.lowestValid = nil, 4096, 4096 // setup history is not part of the journal
	for _, f := range v.files {
		f.usn = 0
	}
	return v
}

// ---- modifying the volume (each change writes journal records) ---------------

func (v *Volume) parentFRN(parent uint64) uint64 {
	if parent == 0 {
		return v.RootFRN
	}
	return parent
}

func (v *Volume) newFRN() uint64 {
	n := v.nextRecord
	v.nextRecord++
	return FRN(n, 1)
}

func (v *Volume) journal(frn uint64, reason uint32) {
	f := v.files[frn]
	// NextUsn is the USN the NEXT record will get (as on real NTFS), so a record
	// takes the current value and then the counter moves on.
	rec := ntfs.Record{FRN: frn, ParentFRN: f.parent, USN: v.nextUsn, Reason: reason, Attrs: f.attrs, Name: f.name}
	v.nextUsn += 64
	v.log = append(v.log, rec)
	f.usn = rec.USN
}

// Mkdir creates a folder (parent 0 = the root) and returns its reference number.
func (v *Volume) Mkdir(parent uint64, name string) uint64 {
	return v.add(parent, name, ntfs.AttrDirectory)
}

// Create creates a file (parent 0 = the root) and returns its reference number.
func (v *Volume) Create(parent uint64, name string) uint64 { return v.add(parent, name, 0) }

func (v *Volume) add(parent uint64, name string, attrs uint32) uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	frn := v.newFRN()
	v.files[frn] = &file{parent: v.parentFRN(parent), name: name, attrs: attrs}
	v.journal(frn, reasonCreate)
	v.journal(frn, reasonCreate|reasonClose)
	return frn
}

// Rename changes a file's name.
func (v *Volume) Rename(frn uint64, newName string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	f := v.files[frn]
	v.journal(frn, reasonRenameOld) // the journal first records the OLD name...
	f.name = newName
	v.journal(frn, reasonRenameNew) // ...then the new one
	v.journal(frn, reasonRenameNew|reasonClose)
}

// Move puts a file or folder into another folder (0 = the root).
func (v *Volume) Move(frn, newParent uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	f := v.files[frn]
	v.journal(frn, reasonRenameOld)
	f.parent = v.parentFRN(newParent)
	v.journal(frn, reasonRenameNew)
	v.journal(frn, reasonRenameNew|reasonClose)
}

// Touch simulates a write to a file (extra journal noise: the name doesn't change).
func (v *Volume) Touch(frn uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.journal(frn, reasonWrite)
	v.journal(frn, reasonWrite|reasonClose)
}

// Delete removes a file or (empty) folder.
func (v *Volume) Delete(frn uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.journal(frn, ntfs.UsnReasonFileDelete|reasonClose)
	delete(v.files, frn)
}

// TrimJournal throws away all history so far, like a full journal overwriting
// old entries: bookmarks older than now can no longer be read.
func (v *Volume) TrimJournal() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.log = nil
	v.lowestValid = v.nextUsn
}

// ResetJournal deletes and recreates the journal: it gets a new ID.
func (v *Volume) ResetJournal() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.JournalID++
	v.log = nil
	v.lowestValid = v.nextUsn
}

// ---- ground truth for assertions ----------------------------------------------

// Paths returns the path (using `\`) of every file and folder a correct index
// should contain: user files only, none of NTFS's metafiles and none inside
// "System Volume Information" or "$RECYCLE.BIN".
func (v *Volume) Paths() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	for frn := range v.files {
		if ntfs.RecordNumber(frn) < ntfs.FirstUserRecord {
			continue
		}
		p, ok := v.pathOf(frn)
		if !ok {
			continue
		}
		low := strings.ToLower(p)
		if strings.HasPrefix(low, `system volume information`) || strings.HasPrefix(low, `$recycle.bin`) {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (v *Volume) pathOf(frn uint64) (string, bool) {
	var parts []string
	for i := 0; i < 1000; i++ {
		f, ok := v.files[frn]
		if !ok || ntfs.RecordNumber(frn) < ntfs.FirstUserRecord {
			return "", false // missing, or inside an NTFS metafile such as $Extend
		}
		parts = append([]string{f.name}, parts...)
		if f.parent == v.RootFRN {
			return strings.Join(parts, `\`), true
		}
		frn = f.parent
	}
	return "", false
}

// Session returns an ntfs.Session reading this volume.
func (v *Volume) Session() *ntfs.Session {
	return ntfs.NewSession(v, v.RootFRN, func() { v.mu.Lock(); v.Closed++; v.mu.Unlock() })
}

// ---- the Device implementation ---------------------------------------------------

// IOControl implements ntfs.Device.
func (v *Volume) IOControl(code uint32, in, out []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Calls[code]++

	switch code {
	case ntfs.FsctlQueryUsnJournal:
		if !v.JournalActive {
			return 0, fmt.Errorf("%w: (fake)", ntfs.ErrJournalNotActive)
		}
		jd := ntfs.JournalData{ID: v.JournalID, FirstUsn: v.lowestValid, NextUsn: v.nextUsn, LowestValidUsn: v.lowestValid,
			MaxUsn: math.MaxInt64, MaximumSize: 32 << 20, AllocationDelta: 4 << 20}
		return copy(out, ntfs.EncodeJournalData(jd)), nil

	case ntfs.FsctlCreateUsnJournal:
		if len(in) != 16 {
			return 0, fmt.Errorf("fake: bad create-journal input (%d bytes)", len(in))
		}
		if !v.JournalActive {
			v.JournalActive = true
			v.JournalID++
			v.lowestValid = v.nextUsn
		}
		return 0, nil

	case ntfs.FsctlEnumUsnData:
		req, err := ntfs.DecodeEnumRequest(in)
		if err != nil {
			return 0, err
		}
		if !v.JournalActive {
			return 0, fmt.Errorf("%w: (fake)", ntfs.ErrJournalNotActive)
		}
		return v.enumerate(req, out)

	case ntfs.FsctlReadUsnJournal:
		req, err := ntfs.DecodeReadRequest(in)
		if err != nil {
			return 0, err
		}
		if !v.JournalActive || req.JournalID != v.JournalID {
			return 0, fmt.Errorf("%w: (fake)", ntfs.ErrJournalNotActive)
		}
		if req.StartUsn < v.lowestValid {
			return 0, fmt.Errorf("%w: (fake)", ntfs.ErrJournalEntryDeleted)
		}
		return v.readJournal(req, out)
	}
	return 0, fmt.Errorf("fake: unsupported control code %#x", code)
}

// enumerate answers FSCTL_ENUM_USN_DATA: files in reference-number order,
// starting after req.StartFRN. First 8 bytes of the answer = where to resume.
func (v *Volume) enumerate(req ntfs.EnumRequest, out []byte) (int, error) {
	frns := make([]uint64, 0, len(v.files))
	for frn, f := range v.files {
		if frn > req.StartFRN && f.usn < req.HighUsn {
			frns = append(frns, frn)
		}
	}
	if len(frns) == 0 {
		return 0, ntfs.ErrEOF
	}
	sort.Slice(frns, func(i, j int) bool { return frns[i] < frns[j] })

	n, last := 8, uint64(0)
	count := 0
	for _, frn := range frns {
		f := v.files[frn]
		rec := ntfs.EncodeRecord(ntfs.Record{FRN: frn, ParentFRN: f.parent, USN: f.usn, Attrs: f.attrs, Name: f.name})
		if n+len(rec) > len(out) || (v.MaxRecordsPerCall > 0 && count >= v.MaxRecordsPerCall) {
			break
		}
		n += copy(out[n:], rec)
		last = frn
		count++
	}
	binary.LittleEndian.PutUint64(out[:8], last)
	return n, nil
}

// readJournal answers FSCTL_READ_USN_JOURNAL: records with USN >= StartUsn.
// First 8 bytes of the answer = the USN to continue from.
func (v *Volume) readJournal(req ntfs.ReadRequest, out []byte) (int, error) {
	n, next, count := 8, v.nextUsn, 0
	for _, rec := range v.log {
		if rec.USN < req.StartUsn {
			continue
		}
		enc := ntfs.EncodeRecord(rec)
		if n+len(enc) > len(out) || (v.MaxRecordsPerCall > 0 && count >= v.MaxRecordsPerCall) {
			next = rec.USN // resume exactly here
			break
		}
		n += copy(out[n:], enc)
		count++
	}
	binary.LittleEndian.PutUint64(out[:8], uint64(next))
	return n, nil
}
