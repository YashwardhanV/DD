package copier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/zeebo/xxh3"
)

// errNotRegular marks a link or device that is skipped rather than copied.
var errNotRegular = errors.New("not a regular file")

// errSourceChanged means the source file was different after it was read than
// before (another program wrote to it). The copy would be a mix of old and new
// bytes under the old time, so it is thrown away and retried.
var errSourceChanged = errors.New("the file changed while it was being copied")

// errInUse means another program has the source open for writing right now
// (Windows refused to let DriveDex block writes while reading it).
var errInUse = errors.New("the file is in use by another program")

// errVerify marks a checksum mismatch.
var errVerify = errors.New("verification failed")

// copyWithRetries copies one item, retrying failures after a short pause.
//
// Every attempt but the last opens the source so that nobody can write to it
// meanwhile; a file that another program holds open for writing then fails
// with errInUse ("in use, will retry") instead of being copied half-written.
// The last attempt opens it normally, still guarded by the before/after check.
func copyWithRetries(ctx context.Context, dstRoot string, it *Item, buf []byte, abuf []byte, opt Options, st *state) {
	dst := joinDst(dstRoot, it.Rel)
	var err error
	for attempt := 0; attempt <= opt.retries(); attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(opt.retryDelay()):
			case <-ctx.Done():
				return
			}
		}
		locked := attempt < opt.retries()
		var n int64
		n, err = copyFile(ctx, it, dst, buf, abuf, opt, st, locked)
		if err == nil {
			if opt.Record != nil {
				if fi, serr := os.Lstat(dst); serr == nil {
					it.Dst = dstStateOf(fi) // what the copy really got (FAT rounds times)
				}
				opt.Record(it)
			}
			st.written.Add(1)
			st.bytesDone.Add(n)
			st.filesDone.Add(1)
			return
		}
		if ctx.Err() != nil {
			return // cancelled: not a failure
		}
		if errors.Is(err, errNotRegular) {
			st.notRegular.Add(1)
			st.skipped.Add(1)
			st.filesDone.Add(1)
			return
		}
		// Any other failure (including a verify mismatch or a source that
		// changed, whose bad copy has already been deleted) is retried.
	}
	if errors.Is(err, errSourceChanged) {
		err = errors.New("the file kept changing while it was copied")
	}
	st.fail(it.Rel, err)
}

// copyFile copies one file atomically: write "<dst>.ddxpart", set its times,
// then rename it to the real name. On success the item records what the
// source looked like and (when hashing) the content hash.
//
// The source must look the same before and after it is read (size and exact
// modification time, and exactly Size bytes read); otherwise the partial copy
// is deleted and errSourceChanged returned.
//
// There is no fsync unless the copy is verified against the media: forcing
// every small file to the platter destroys USB throughput. (Use "Safely
// Remove" before unplugging a drive.)
func copyFile(ctx context.Context, it *Item, dst string, buf, abuf []byte, opt Options, st *state, locked bool) (written int64, err error) {
	src := it.Src
	before, err := os.Lstat(src)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", src, err)
	}
	if !before.Mode().IsRegular() {
		return 0, errNotRegular // never follow links: that could copy files from elsewhere
	}
	in, err := openSource(src, locked)
	if err != nil {
		if errors.Is(err, errInUse) {
			return 0, fmt.Errorf("%s: %w, will retry", src, errInUse)
		}
		return 0, fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()
	if it.SrcFileID == 0 {
		it.SrcFileID = platformFileID(in)
	}

	part := dst + PartSuffix
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", part, err)
	}
	cleanup := func() { out.Close(); os.Remove(part) }

	// Reserve the whole file's space up front: one allocation instead of
	// growing the file a megabyte at a time. With several files written at
	// once this keeps each file in one piece on the destination instead of
	// interleaving them. Best effort: if it can't, the writes still work.
	if before.Size() >= preallocMin {
		preallocate(out, before.Size())
	}

	// Hash while copying, so the source is read only once.
	var r io.Reader = &ctxReader{ctx: ctx, r: in}
	var h *xxh3.Hasher
	if opt.hashing() {
		h = xxh3.New()
		r = io.TeeReader(r, h)
	}
	// struct{ io.Writer } hides *os.File's ReadFrom method; otherwise
	// io.CopyBuffer would ignore our big buffer and use a small default one.
	n, err := io.CopyBuffer(struct{ io.Writer }{out}, r, buf)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("copying %s: %w", src, err)
	}
	if duringCopy != nil {
		duringCopy(src)
	}

	// Did the source change while we read it? Same size, same exact time and
	// every byte read, or the copy is not accepted.
	after, err := os.Lstat(src)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || n != before.Size() {
		cleanup()
		return 0, errSourceChanged
	}

	// The times come from the source as it was when it was copied (not from
	// the plan, which may be minutes old), at full precision. Set them
	// through the handle we already have: doing it by name afterwards opens
	// the file a second time and rewrites its folder entry a second time, a
	// slow little write on a pen drive paid once per file.
	mtime := before.ModTime()
	ctime := creationTime(before)
	timeSet, err := setFileTimes(out, mtime, ctime)
	if err != nil {
		cleanup()
		return 0, fmt.Errorf("setting the time of %s: %w", part, err)
	}
	if opt.Verify == VerifyMedia {
		// Push the data out of Windows' cache AND the drive's own write cache
		// (FlushFileBuffers sends the device a cache-flush command), so the
		// read-back below comes from the media.
		if err := out.Sync(); err != nil {
			cleanup()
			return 0, fmt.Errorf("flushing %s to the drive: %w", part, err)
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(part)
		return 0, fmt.Errorf("writing %s: %w", part, err)
	}
	if !timeSet {
		if err := os.Chtimes(part, mtime, mtime); err != nil {
			os.Remove(part)
			return 0, fmt.Errorf("setting the time of %s: %w", part, err)
		}
	}
	if opt.Trash != nil && it.Decision == Overwrite {
		// Keep the old version: into the trash first (a rename on the same
		// drive), then the new one into its place. If the trash can't take
		// it, the old file stays and this copy fails (INV-3).
		if _, serr := os.Lstat(dst); serr == nil {
			if terr := opt.Trash.Move(it.Rel, TrashReplaced); terr != nil {
				os.Remove(part)
				return 0, fmt.Errorf("keeping the old version of %s: %w", it.Rel, terr)
			}
		}
	}
	if err := os.Rename(part, dst); err != nil {
		os.Remove(part)
		return 0, fmt.Errorf("moving %s into place: %w", part, err)
	}
	if afterCopy != nil {
		afterCopy(dst)
	}

	it.SrcSize, it.SrcMTimeNs, it.DstMTimeNs = before.Size(), mtime.UnixNano(), mtime.UnixNano()
	if h != nil {
		it.Hash, it.Hashed = h.Sum128().Bytes(), true
	}

	if opt.Verify != VerifyOff {
		var got [16]byte
		var verr error
		if opt.Verify == VerifyMedia {
			got, verr = hashUnbuffered(ctx, dst, abuf)
		} else {
			got, verr = hashCached(ctx, dst, buf)
		}
		switch {
		case ctx.Err() != nil:
			return n, ctx.Err()
		case verr != nil:
			os.Remove(dst)
			return n, fmt.Errorf("%w: couldn't read the copy back (%v)", errVerify, verr)
		case got != it.Hash:
			// Remove the bad copy: it has the right size and time, so a re-run
			// would otherwise consider it "already there" and keep it.
			os.Remove(dst)
			st.mismatches.Add(1)
			it.Hashed = false
			return n, fmt.Errorf("%w: the copy read back from the drive differs from the original", errVerify)
		}
		it.MediaChecked = opt.Verify == VerifyMedia
	}
	return n, nil
}

// hashCached computes the XXH3-128 of a file read normally (through the cache).
func hashCached(ctx context.Context, path string, buf []byte) ([16]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [16]byte{}, err
	}
	defer f.Close()
	h := xxh3.New()
	if _, err := io.CopyBuffer(h, &ctxReader{ctx: ctx, r: f}, buf); err != nil {
		return [16]byte{}, err
	}
	return h.Sum128().Bytes(), nil
}

// hashUnbuffered computes the XXH3-128 of a file read WITHOUT the Windows
// cache. buf must be aligned (alignedBuffer) and a whole number of sectors
// long; every read asks for len(buf) bytes, and only the last one may return
// fewer (the end of the file).
func hashUnbuffered(ctx context.Context, path string, buf []byte) ([16]byte, error) {
	f, err := openUnbuffered(path)
	if err != nil {
		return [16]byte{}, err
	}
	defer f.Close()
	h := xxh3.New()
	for {
		if err := ctx.Err(); err != nil {
			return [16]byte{}, err
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF || (err == nil && n == 0) {
			break
		}
		if err != nil {
			return [16]byte{}, err
		}
	}
	return h.Sum128().Bytes(), nil
}

// ctxReader makes a long read stop promptly when the context is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
