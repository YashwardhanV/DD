package ntfs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/yashwardhanv/dd/internal/index"
)

// Sentinel errors. The Windows layer translates raw Windows error numbers into
// these, so everything above it is portable and testable.
var (
	// ErrNeedAdmin: opening the drive for raw reading was refused.
	ErrNeedAdmin = errors.New("reading the NTFS master file table needs an Administrator terminal")
	// ErrNoJournal: the drive has no (usable) change journal.
	ErrNoJournal = errors.New("this drive has no NTFS change journal")
	// ErrEOF: the enumeration has no more records.
	ErrEOF = errors.New("end of data")
	// ErrJournalNotActive: the change journal exists but is switched off.
	ErrJournalNotActive = errors.New("the change journal is not active")
	// ErrJournalDeleteInProgress: the journal is being deleted right now.
	ErrJournalDeleteInProgress = errors.New("the change journal is being deleted")
	// ErrJournalEntryDeleted: the bookmark is older than the journal's oldest entry.
	ErrJournalEntryDeleted = errors.New("the change journal no longer holds the entries we need")
	// ErrNeedFullRebuild: an incremental refresh isn't possible; re-scan everything.
	ErrNeedFullRebuild = errors.New("a full re-index is needed")
)

// Device is "something we can send NTFS control codes to": a volume handle on
// Windows, an in-memory fake in tests.
type Device interface {
	// IOControl sends a control code with an input buffer and fills out;
	// it returns the number of bytes written to out. Windows errors are
	// translated to the sentinel errors above.
	IOControl(code uint32, in, out []byte) (int, error)
}

// QueryJournal returns the state of the change journal. ErrJournalNotActive
// means the drive has none switched on.
func QueryJournal(d Device) (JournalData, error) {
	out := make([]byte, 80) // the structure is 56 bytes; a bit of slack is harmless
	n, err := d.IOControl(FsctlQueryUsnJournal, nil, out)
	if err != nil {
		return JournalData{}, err
	}
	return ParseJournalData(out[:n])
}

// CreateJournal switches the change journal on (32 MB with a 4 MB step by
// default, as BUILD.md specifies).
func CreateJournal(d Device, maxSize, allocationDelta uint64) error {
	_, err := d.IOControl(FsctlCreateUsnJournal, EncodeCreateJournal(maxSize, allocationDelta), make([]byte, 16))
	return err
}

// enumBufferSize is how much of the MFT is requested per call. Big chunks are
// the whole point: few calls, lots of files each.
const enumBufferSize = 1 << 20

// EnumerateMFT reads every file record on the drive using FSCTL_ENUM_USN_DATA.
// high is the highest USN to include (the journal's NextUsn, or MaxInt64 if the
// drive has no journal). report, if non-nil, is called with the running count.
//
// If NTFS answers with a journal error, ErrNoJournal is returned so the caller
// can fall back to the normal folder walker.
func EnumerateMFT(ctx context.Context, d Device, high int64, report func(records int64)) ([]Record, error) {
	out := make([]byte, enumBufferSize)
	var recs []Record
	var start uint64
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req := EnumRequest{StartFRN: start, LowUsn: 0, HighUsn: high}
		n, err := d.IOControl(FsctlEnumUsnData, req.Encode(), out)
		switch {
		case errors.Is(err, ErrEOF):
			return recs, nil // finished
		case errors.Is(err, ErrJournalNotActive), errors.Is(err, ErrJournalDeleteInProgress):
			return nil, fmt.Errorf("%w: %v", ErrNoJournal, err)
		case err != nil:
			return nil, err
		}
		if n < 8 {
			return nil, fmt.Errorf("%w: enumeration returned only %d bytes", ErrBadRecord, n)
		}
		// The first 8 bytes say where the next call should start; records follow.
		next := binary.LittleEndian.Uint64(out[:8])
		before := len(recs)
		recs, err = AppendRecords(recs, out[8:n])
		if err != nil {
			return nil, err
		}
		if len(recs) == before && next == start {
			return recs, nil // no progress: stop rather than loop forever
		}
		start = next
		if report != nil {
			report(int64(len(recs)))
		}
	}
}

// readBufferSize is the size of one journal read.
const readBufferSize = 1 << 20

// allReasons asks the journal for every kind of change.
const allReasons = 0xFFFFFFFF

// ReadJournal reads all journal records from startUsn up to the present and
// passes each to apply, in order. It returns the USN to continue from next time.
//
// ErrJournalEntryDeleted means startUsn is older than the journal's oldest
// entry (the diary was trimmed): the caller must rebuild from scratch.
func ReadJournal(ctx context.Context, d Device, journalID uint64, startUsn int64, apply func(Record)) (next int64, err error) {
	out := make([]byte, readBufferSize)
	usn := startUsn
	for {
		if err := ctx.Err(); err != nil {
			return usn, err
		}
		req := ReadRequest{StartUsn: usn, ReasonMask: allReasons, JournalID: journalID}
		n, err := d.IOControl(FsctlReadUsnJournal, req.Encode(), out)
		if err != nil {
			return usn, err
		}
		if n < 8 {
			return usn, fmt.Errorf("%w: journal read returned only %d bytes", ErrBadRecord, n)
		}
		// First 8 bytes: the USN to continue from. Records (if any) follow.
		nextUsn := int64(binary.LittleEndian.Uint64(out[:8]))
		if n > 8 {
			recs, err := ParseRecords(out[8:n])
			if err != nil {
				return usn, err
			}
			for _, r := range recs {
				apply(r)
			}
		}
		progressed := nextUsn > usn
		if nextUsn > usn {
			usn = nextUsn
		}
		if n <= 8 || !progressed { // nothing (more) to read
			return usn, nil
		}
	}
}

// Session is an opened NTFS volume, ready for the calls above.
type Session struct {
	Dev     Device
	RootFRN uint64 // the file reference number of the drive's root folder
	// SkipDirs are folder names left out when replaying the change journal
	// (nil = scan.DefaultSkipDirs). Set it to the same list the full scan used.
	SkipDirs []string
	closer   func()
}

// NewSession wraps a Device (used by the Windows opener and by tests).
func NewSession(dev Device, rootFRN uint64, closer func()) *Session {
	return &Session{Dev: dev, RootFRN: rootFRN, closer: closer}
}

// Close releases the volume handle.
func (s *Session) Close() {
	if s != nil && s.closer != nil {
		s.closer()
	}
}

// noJournalHigh is the HighUsn used when the drive has no journal.
const noJournalHigh = math.MaxInt64

// RefreshResult describes an incremental update.
type RefreshResult struct {
	Changes int // distinct files/folders that changed in the visible tree
	Records int // journal records read
}

// Refresh brings ix up to date by replaying only the journal records written
// since the index was built. It returns an error wrapping ErrNeedFullRebuild
// whenever that isn't safe: no journal, a different journal (it was deleted and
// recreated), or a bookmark older than the journal's oldest entry.
func Refresh(ctx context.Context, s *Session, ix *index.Index) (RefreshResult, error) {
	var res RefreshResult
	jd, err := QueryJournal(s.Dev)
	if err != nil {
		if errors.Is(err, ErrJournalNotActive) || errors.Is(err, ErrJournalDeleteInProgress) {
			return res, fmt.Errorf("%w: the drive's change journal is not active", ErrNeedFullRebuild)
		}
		return res, err
	}
	h := &ix.Header
	switch {
	case h.UsnJournalID == 0:
		return res, fmt.Errorf("%w: the saved index has no journal bookmark", ErrNeedFullRebuild)
	case h.UsnJournalID != jd.ID:
		return res, fmt.Errorf("%w: the change journal was reset since the last index", ErrNeedFullRebuild)
	case h.NextUsn < jd.LowestValidUsn:
		return res, fmt.Errorf("%w: the last index is older than the change journal's history", ErrNeedFullRebuild)
	}

	ap := NewApplier(ix, s.RootFRN, s.SkipDirs)
	next, err := ReadJournal(ctx, s.Dev, jd.ID, h.NextUsn, ap.Apply)
	if errors.Is(err, ErrJournalEntryDeleted) {
		return res, fmt.Errorf("%w: the change journal no longer holds the entries since the last index", ErrNeedFullRebuild)
	}
	if err != nil {
		return res, err
	}
	h.NextUsn = next
	return RefreshResult{Changes: ap.Changes(), Records: ap.Records}, nil
}
