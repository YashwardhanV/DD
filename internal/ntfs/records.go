// Package ntfs reads an NTFS drive's own bookkeeping instead of walking its
// folders - the trick that makes tools like "Everything" instant.
//
// Two things NTFS keeps:
//
//   - The MFT (Master File Table) is a catalogue of EVERY file on the drive.
//     Reading it in big chunks is like reading a library's card catalogue
//     instead of walking along every shelf. Millions of files in seconds.
//   - The USN change journal is a diary of what changed (created, renamed,
//     deleted ...). Reading only the new diary entries lets us refresh an
//     index in milliseconds without rescanning anything.
//
// Talking to NTFS this way needs Administrator rights, and only works through
// Windows' DeviceIoControl API. To keep almost all of the logic testable on any
// machine, the code is split in two:
//
//   - Portable: parsing the binary records, deciding what they mean, building
//     and updating the index. Works against a small Device interface.
//   - Windows-only (mft_windows.go, usn_windows.go): the thin layer that
//     implements Device with real DeviceIoControl calls.
package ntfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Windows control codes (BUILD.md stage 6; defined here so no platform package is needed).
const (
	FsctlQueryUsnJournal  uint32 = 0x000900F4 // FSCTL_QUERY_USN_JOURNAL
	FsctlEnumUsnData      uint32 = 0x000900B3 // FSCTL_ENUM_USN_DATA
	FsctlReadUsnJournal   uint32 = 0x000900BB // FSCTL_READ_USN_JOURNAL
	FsctlCreateUsnJournal uint32 = 0x000900E7 // FSCTL_CREATE_USN_JOURNAL
)

// Windows error codes the journal calls can fail with.
const (
	ErrnoAccessDenied               = 5
	ErrnoHandleEOF                  = 38
	ErrnoJournalDeleteInProg        = 1178
	ErrnoJournalNotActive           = 1179
	ErrnoJournalEntryDeleted        = 1181
	UsnReasonFileDelete      uint32 = 0x00000200
)

// File attribute bits found in USN records.
const (
	AttrReadOnly  uint32 = 0x01
	AttrHidden    uint32 = 0x02
	AttrSystem    uint32 = 0x04
	AttrDirectory uint32 = 0x10
	AttrReparse   uint32 = 0x400
)

// FirstUserRecord: MFT records 0-23 are NTFS's own metafiles ($MFT, $LogFile,
// $Extend ...). They are not user files, so we skip them and everything inside them.
const FirstUserRecord = 24

// RecordNumber strips the "sequence number" from the top 16 bits of a file
// reference number, leaving the position in the MFT.
func RecordNumber(frn uint64) uint64 { return frn & 0x0000FFFFFFFFFFFF }

// Record is one USN_RECORD_V2: what the MFT / journal says about one file.
type Record struct {
	FRN       uint64 // file reference number: this file's identity
	ParentFRN uint64 // the folder it is in
	USN       int64
	Reason    uint32 // why this journal record was written (journal only)
	Attrs     uint32 // FILE_ATTRIBUTE_* bits
	Name      string // just the file name, not the path
}

// IsDir reports whether the record describes a folder.
func (r Record) IsDir() bool { return r.Attrs&AttrDirectory != 0 }

// IsDelete reports whether the journal says this file was deleted.
func (r Record) IsDelete() bool { return r.Reason&UsnReasonFileDelete != 0 }

// USN_RECORD_V2 layout (byte offsets, little-endian):
//
//	 0 RecordLength uint32          4 MajorVersion uint16 (we only read 2)
//	 6 MinorVersion uint16          8 FileReferenceNumber uint64
//	16 ParentFileReferenceNumber    24 Usn int64
//	32 TimeStamp int64              40 Reason uint32
//	44 SourceInfo uint32            48 SecurityId uint32
//	52 FileAttributes uint32        56 FileNameLength uint16 (BYTES)
//	58 FileNameOffset uint16        then the UTF-16LE name
const usnRecordV2MinSize = 60

// ErrBadRecord means a buffer from NTFS didn't look like USN records.
var ErrBadRecord = errors.New("malformed USN record data")

// ParseRecords decodes consecutive USN_RECORD_V2 structures from buf (the
// bytes AFTER the 8-byte header that enumeration and journal reads return).
// Records of other versions are skipped. Truncated or inconsistent data
// returns an error - never a panic.
func ParseRecords(buf []byte) ([]Record, error) {
	return AppendRecords(nil, buf)
}

// AppendRecords is ParseRecords appending to dst, so a caller reading many
// buffers can build one big slice without copying.
func AppendRecords(dst []Record, buf []byte) ([]Record, error) {
	for off := 0; off < len(buf); {
		if len(buf)-off < 8 {
			return dst, fmt.Errorf("%w: %d stray bytes at offset %d", ErrBadRecord, len(buf)-off, off)
		}
		length := int(binary.LittleEndian.Uint32(buf[off:]))
		if length < 8 || length > len(buf)-off {
			return dst, fmt.Errorf("%w: record at offset %d claims length %d but only %d bytes remain", ErrBadRecord, off, length, len(buf)-off)
		}
		major := binary.LittleEndian.Uint16(buf[off+4:])
		if major != 2 {
			off += length // V3/V4 records: not ours
			continue
		}
		if length < usnRecordV2MinSize {
			return dst, fmt.Errorf("%w: V2 record at offset %d is only %d bytes", ErrBadRecord, off, length)
		}
		rec := buf[off : off+length]
		nameLen := int(binary.LittleEndian.Uint16(rec[56:]))
		nameOff := int(binary.LittleEndian.Uint16(rec[58:]))
		if nameLen%2 != 0 || nameOff < usnRecordV2MinSize-2 || nameOff+nameLen > length {
			return dst, fmt.Errorf("%w: record at offset %d has an impossible file name (offset %d, %d bytes)", ErrBadRecord, off, nameOff, nameLen)
		}
		dst = append(dst, Record{
			FRN:       binary.LittleEndian.Uint64(rec[8:]),
			ParentFRN: binary.LittleEndian.Uint64(rec[16:]),
			USN:       int64(binary.LittleEndian.Uint64(rec[24:])),
			Reason:    binary.LittleEndian.Uint32(rec[40:]),
			Attrs:     binary.LittleEndian.Uint32(rec[52:]),
			Name:      decodeUTF16LE(rec[nameOff : nameOff+nameLen]),
		})
		off += length
	}
	return dst, nil
}

// decodeUTF16LE converts a UTF-16 little-endian byte slice to a Go string.
//
// PERF: nearly every file name is plain ASCII, so there is a fast path that
// builds the string with one allocation; anything else goes through the
// standard library's (slower, fully correct) decoder.
func decodeUTF16LE(b []byte) string {
	n := len(b) / 2
	ascii := true
	for i := 0; i < n; i++ {
		if b[2*i+1] != 0 || b[2*i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		var sb strings.Builder
		sb.Grow(n)
		for i := 0; i < n; i++ {
			sb.WriteByte(b[2*i])
		}
		return sb.String()
	}
	units := make([]uint16, n)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units))
}

// ---------------------------------------------------------------------------
// Structures exchanged with FSCTL calls
// ---------------------------------------------------------------------------

// JournalData is USN_JOURNAL_DATA_V0: the current state of the change journal.
type JournalData struct {
	ID              uint64 // changes if the journal is deleted and recreated
	FirstUsn        int64
	NextUsn         int64 // where the NEXT change will be written
	LowestValidUsn  int64 // older bookmarks than this can no longer be read
	MaxUsn          int64
	MaximumSize     uint64
	AllocationDelta uint64
}

// journalDataSize is the part of USN_JOURNAL_DATA_V0 we read (7 x 8 bytes).
const journalDataSize = 56

// ParseJournalData decodes the answer to FSCTL_QUERY_USN_JOURNAL.
func ParseJournalData(b []byte) (JournalData, error) {
	if len(b) < journalDataSize {
		return JournalData{}, fmt.Errorf("%w: journal data is %d bytes, expected at least %d", ErrBadRecord, len(b), journalDataSize)
	}
	le := binary.LittleEndian
	return JournalData{
		ID:              le.Uint64(b[0:]),
		FirstUsn:        int64(le.Uint64(b[8:])),
		NextUsn:         int64(le.Uint64(b[16:])),
		LowestValidUsn:  int64(le.Uint64(b[24:])),
		MaxUsn:          int64(le.Uint64(b[32:])),
		MaximumSize:     le.Uint64(b[40:]),
		AllocationDelta: le.Uint64(b[48:]),
	}, nil
}

// EncodeJournalData is the inverse of ParseJournalData (used by test doubles).
func EncodeJournalData(j JournalData) []byte {
	b := make([]byte, journalDataSize)
	le := binary.LittleEndian
	le.PutUint64(b[0:], j.ID)
	le.PutUint64(b[8:], uint64(j.FirstUsn))
	le.PutUint64(b[16:], uint64(j.NextUsn))
	le.PutUint64(b[24:], uint64(j.LowestValidUsn))
	le.PutUint64(b[32:], uint64(j.MaxUsn))
	le.PutUint64(b[40:], j.MaximumSize)
	le.PutUint64(b[48:], j.AllocationDelta)
	return b
}

// EnumRequest is MFT_ENUM_DATA_V0 (24 bytes): "give me the next batch of files".
type EnumRequest struct {
	StartFRN uint64
	LowUsn   int64
	HighUsn  int64
}

// Encode serialises the request for DeviceIoControl.
func (r EnumRequest) Encode() []byte {
	b := make([]byte, 24)
	binary.LittleEndian.PutUint64(b[0:], r.StartFRN)
	binary.LittleEndian.PutUint64(b[8:], uint64(r.LowUsn))
	binary.LittleEndian.PutUint64(b[16:], uint64(r.HighUsn))
	return b
}

// DecodeEnumRequest parses an EnumRequest (used by test doubles).
func DecodeEnumRequest(b []byte) (EnumRequest, error) {
	if len(b) < 24 {
		return EnumRequest{}, fmt.Errorf("%w: enum request is %d bytes", ErrBadRecord, len(b))
	}
	return EnumRequest{
		StartFRN: binary.LittleEndian.Uint64(b[0:]),
		LowUsn:   int64(binary.LittleEndian.Uint64(b[8:])),
		HighUsn:  int64(binary.LittleEndian.Uint64(b[16:])),
	}, nil
}

// ReadRequest is READ_USN_JOURNAL_DATA_V0 (40 bytes): "give me the changes since StartUsn".
type ReadRequest struct {
	StartUsn          int64
	ReasonMask        uint32
	ReturnOnlyOnClose uint32
	Timeout           uint64
	BytesToWaitFor    uint64
	JournalID         uint64
}

// Encode serialises the request for DeviceIoControl.
func (r ReadRequest) Encode() []byte {
	b := make([]byte, 40)
	le := binary.LittleEndian
	le.PutUint64(b[0:], uint64(r.StartUsn))
	le.PutUint32(b[8:], r.ReasonMask)
	le.PutUint32(b[12:], r.ReturnOnlyOnClose)
	le.PutUint64(b[16:], r.Timeout)
	le.PutUint64(b[24:], r.BytesToWaitFor)
	le.PutUint64(b[32:], r.JournalID)
	return b
}

// DecodeReadRequest parses a ReadRequest (used by test doubles).
func DecodeReadRequest(b []byte) (ReadRequest, error) {
	if len(b) < 40 {
		return ReadRequest{}, fmt.Errorf("%w: read request is %d bytes", ErrBadRecord, len(b))
	}
	le := binary.LittleEndian
	return ReadRequest{
		StartUsn:          int64(le.Uint64(b[0:])),
		ReasonMask:        le.Uint32(b[8:]),
		ReturnOnlyOnClose: le.Uint32(b[12:]),
		Timeout:           le.Uint64(b[16:]),
		BytesToWaitFor:    le.Uint64(b[24:]),
		JournalID:         le.Uint64(b[32:]),
	}, nil
}

// EncodeCreateJournal is CREATE_USN_JOURNAL_DATA (16 bytes).
func EncodeCreateJournal(maxSize, allocationDelta uint64) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], maxSize)
	binary.LittleEndian.PutUint64(b[8:], allocationDelta)
	return b
}

// EncodeRecord builds one USN_RECORD_V2 (used by test doubles and tests).
func EncodeRecord(r Record) []byte {
	name := utf16.Encode([]rune(r.Name))
	nameBytes := len(name) * 2
	length := (usnRecordV2MinSize + nameBytes + 7) &^ 7 // records are padded to 8 bytes
	b := make([]byte, length)
	le := binary.LittleEndian
	le.PutUint32(b[0:], uint32(length))
	le.PutUint16(b[4:], 2) // major version
	le.PutUint64(b[8:], r.FRN)
	le.PutUint64(b[16:], r.ParentFRN)
	le.PutUint64(b[24:], uint64(r.USN))
	le.PutUint32(b[40:], r.Reason)
	le.PutUint32(b[52:], r.Attrs)
	le.PutUint16(b[56:], uint16(nameBytes))
	le.PutUint16(b[58:], usnRecordV2MinSize)
	for i, u := range name {
		le.PutUint16(b[usnRecordV2MinSize+2*i:], u)
	}
	return b
}
