package manifest

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
)

// manifest.journal is a sequence of frames:
//
//	uvarint  payload length
//	payload  op byte, then for opPut a record (full path, source ID, fields),
//	         for opDelete a path
//	uint32 LE CRC-32C of the payload
//
// Replay stops at the first frame that is short or fails its CRC: a torn tail
// is expected after an unplug, and everything before it is good.

const (
	opPut    byte = 1
	opDelete byte = 2
)

const maxFrame = 1 << 20

func putPayload(r Record) []byte {
	var b bytes.Buffer
	e := &enc{w: &b}
	b.WriteByte(opPut)
	e.str(Key(r.Rel))
	e.str(r.SrcID)
	e.fields(r)
	return b.Bytes()
}

func deletePayload(rel string) []byte {
	var b bytes.Buffer
	e := &enc{w: &b}
	b.WriteByte(opDelete)
	e.str(Key(rel))
	return b.Bytes()
}

// frame wraps a payload for the journal.
func frame(payload []byte) []byte {
	var b bytes.Buffer
	(&enc{w: &b}).uvarint(uint64(len(payload)))
	b.Write(payload)
	binary.Write(&b, binary.LittleEndian, crc32.Checksum(payload, castagnoli))
	return b.Bytes()
}

// ReplayJournal applies the good frames of data to m. It returns how many
// frames were applied and whether the data ended in a damaged (torn) frame.
func ReplayJournal(data []byte, m *Manifest) (applied int, torn bool) {
	for len(data) > 0 {
		n, k := binary.Uvarint(data)
		if k <= 0 || n == 0 || n > maxFrame || uint64(len(data)-k) < n+4 {
			return applied, true
		}
		payload := data[k : k+int(n)]
		crc := binary.LittleEndian.Uint32(data[k+int(n):])
		if crc != crc32.Checksum(payload, castagnoli) {
			return applied, true
		}
		if !applyPayload(payload, m) {
			return applied, true
		}
		applied++
		data = data[k+int(n)+4:]
	}
	return applied, false
}

func applyPayload(p []byte, m *Manifest) bool {
	d := &dec{b: p[1:]}
	switch p[0] {
	case opPut:
		var r Record
		r.Rel = d.str()
		r.SrcID = d.str()
		d.fields(&r)
		if d.err != nil || len(d.b) != 0 || r.Rel == "" {
			return false
		}
		m.Put(r)
	case opDelete:
		rel := d.str()
		if d.err != nil || len(d.b) != 0 {
			return false
		}
		m.Delete(rel)
	default:
		return false
	}
	return true
}
