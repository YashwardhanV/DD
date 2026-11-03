package copier

import (
	"context"
	"fmt"
	"hash"
	"io"
	"os"
	"time"

	"github.com/yashwardhanv/dd/internal/copier/trace"
)

// ioMode says how a file's bytes are moved.
type ioMode uint8

const (
	buffered   ioMode = iota // through the Windows file cache (v1)
	unbuffered               // straight to the disk, FILE_FLAG_NO_BUFFERING (stage 18, big files only)
)

// writeAtomic streams r into dst through dst+".ddxpart", sets the
// modification time through the open handle, closes, and renames into place.
//
// EVERY lane uses it - the local workers, the big-file lane, the phone tar
// lane, and the pull lane's placing step - so the v1 guarantee holds
// everywhere: a file's real name only ever appears when the whole file is on
// disk. If anything fails (or ctx is cancelled) the .ddxpart is removed and
// nothing appears under the real name.
//
// size is the expected size (used to reserve space; -1 = unknown). There is
// deliberately no fsync: forcing every small file to the platter destroys USB
// throughput. (Use "Safely Remove" before unplugging a drive.)
func writeAtomic(ctx context.Context, dst string, r io.Reader, size, mtime int64, buf []byte, mode ioMode, tr *trace.Recorder) (written int64, err error) {
	part := dst + PartSuffix

	m := tr.Start()
	out, unbufOut, err := createPart(part, mode)
	tr.End(trace.DstCreate, m)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", part, err)
	}
	cleanup := func() { out.Close(); os.Remove(part) }

	// Reserve the whole file's space up front: one allocation instead of
	// growing the file a megabyte at a time. With several files written at
	// once this keeps each file in one piece on the destination instead of
	// interleaving them. Best effort: if it can't, the writes still work.
	if size >= preallocMin {
		m = tr.Start()
		preallocate(out, size)
		tr.End(trace.Prealloc, m)
	}

	m = tr.Start()
	n, err := copyData(ctx, out, r, buf, unbufOut)
	tr.End(trace.Data, m)
	if err != nil {
		cleanup()
		return 0, err
	}

	// Set the time through the handle we already have, before closing. Doing
	// it by name afterwards (os.Chtimes) opens the file a second time and
	// rewrites its folder entry a second time - on a pen drive each of those
	// is a slow little write, paid once per file.
	t := time.Unix(mtime, 0)
	m = tr.Start()
	timeSet, err := setModTime(out, t)
	tr.End(trace.SetTime, m)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("setting the time of %s: %w", part, err)
	}
	m = tr.Start()
	err = out.Close()
	tr.End(trace.Close, m)
	if err != nil {
		os.Remove(part)
		return 0, fmt.Errorf("writing %s: %w", part, err)
	}
	if !timeSet {
		m = tr.Start()
		err := os.Chtimes(part, t, t)
		tr.End(trace.SetTime, m)
		if err != nil {
			os.Remove(part)
			return 0, fmt.Errorf("setting the time of %s: %w", part, err)
		}
	}
	m = tr.Start()
	err = os.Rename(part, dst)
	tr.End(trace.Rename, m)
	if err != nil {
		os.Remove(part)
		return 0, fmt.Errorf("moving %s into place: %w", part, err)
	}
	return n, nil
}

// createPart creates (or truncates) the .ddxpart file. In unbuffered mode
// it tries FILE_FLAG_NO_BUFFERING and reports whether it got it.
func createPart(part string, mode ioMode) (*os.File, bool, error) {
	if mode == unbuffered {
		if f, err := createUnbuffered(part); err == nil {
			return f, true, nil
		}
		// Not possible here (not Windows, or refused): the normal way.
	}
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	return f, false, err
}

// copyData moves the bytes from r to out with buf. When either side is
// unbuffered, the bytes move in sector-aligned blocks from an aligned buffer.
func copyData(ctx context.Context, out *os.File, r io.Reader, buf []byte, unbufOut bool) (int64, error) {
	// An unbuffered handle on EITHER side needs whole, aligned blocks.
	if _, srcUnbuf := r.(*unbufferedSource); unbufOut || srcUnbuf {
		return copyAligned(ctx, out, &ctxReader{ctx: ctx, r: r}, alignedBuffer(unbufferedBufSize), srcUnbuf)
	}
	// struct{ io.Writer } hides *os.File's ReadFrom method; otherwise
	// io.CopyBuffer would ignore our big buffer and use a small default one.
	n, err := io.CopyBuffer(struct{ io.Writer }{out}, &ctxReader{ctx: ctx, r: r}, buf)
	if err != nil {
		return 0, fmt.Errorf("copying: %w", err)
	}
	return n, nil
}

// unbufferedSource marks a source opened with FILE_FLAG_NO_BUFFERING, so
// copyData reads it in whole aligned blocks. h, if set, sees every byte read
// (the --verify checksum, 18c).
type unbufferedSource struct {
	f *os.File
	h hash.Hash32
}

func (u *unbufferedSource) Read(p []byte) (int, error) {
	n, err := u.f.Read(p)
	if u.h != nil && n > 0 {
		u.h.Write(p[:n])
	}
	return n, err
}
