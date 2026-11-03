package copier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"
)

// Local fast paths (BUILD-V2 stage 18). Each one has a switch; the zero value
// of FastPaths is exactly v1.
type FastPaths struct {
	// SingleOpen opens each source file once and reads its size and time
	// from the open handle (18a), instead of looking the path up twice
	// (os.Lstat, then os.Open). The modification time given to the copy is
	// the file's real one at that moment, not the plan's.
	SingleOpen bool
	// Unbuffered copies files of UnbufferedMin bytes and more straight to
	// and from the disk, skipping Windows' RAM cache (18b, what robocopy /J
	// does): no "fast at first, then stalls while the cache flushes".
	Unbuffered bool
	// MediaVerify makes --verify compute the source's checksum while copying
	// and re-read only the DESTINATION, bypassing the cache, so the checksum
	// comes from the disk, not RAM (18c).
	MediaVerify bool
	// Direct writes small files (< 1 MB) straight to their real names when
	// the destination folder is new, recording finished files in a resume
	// journal (18e, experimental).
	Direct bool
}

// UnbufferedMin is the smallest file copied unbuffered (copy_unbuffered).
const UnbufferedMin = 256 << 20

// unbufferedMin is UnbufferedMin (a variable so tests can use small files).
var unbufferedMin int64 = UnbufferedMin

// sectorAlign is the alignment unbuffered I/O needs: buffer addresses and
// every read/write size must be multiples of the disk's sector size. 4096
// covers both 512-byte and 4K-sector disks.
const sectorAlign = 4096

// unbufferedBufSize is the buffer for unbuffered copies (big requests keep a
// disk streaming).
const unbufferedBufSize = 8 << 20

// alignedBuffer returns a size-byte slice whose first byte sits on a
// sectorAlign boundary: allocate size+4096 and start at the first aligned
// address inside.
func alignedBuffer(size int) []byte {
	raw := make([]byte, size+sectorAlign)
	off := 0
	if r := int(uintptr(unsafe.Pointer(&raw[0])) % sectorAlign); r != 0 {
		off = sectorAlign - r
	}
	return raw[off : off+size : off+size]
}

// roundUp rounds n up to a multiple of sectorAlign.
func roundUp(n int) int { return (n + sectorAlign - 1) / sectorAlign * sectorAlign }

// copyAligned copies in to out with sector-sized writes, as unbuffered I/O
// requires. Every read and write uses the whole aligned buffer (never a
// slice of it starting in the middle); the last, partial block is padded with
// zeros to a full sector and the file is then cut back to its true length
// with setEOF. buf must come from alignedBuffer and be a multiple of
// sectorAlign long. srcUnbuffered says the source handle is unbuffered too
// (then each Read is one aligned request and a short read means end of file).
func copyAligned(ctx context.Context, out *os.File, in io.Reader, buf []byte, srcUnbuffered bool) (int64, error) {
	if len(buf) == 0 || len(buf)%sectorAlign != 0 {
		return 0, errors.New("copyAligned: buffer is not a multiple of the sector size")
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var n int
		var rerr error
		if srcUnbuffered {
			n, rerr = in.Read(buf)
		} else {
			n, rerr = io.ReadFull(in, buf)
			if rerr == io.ErrUnexpectedEOF {
				rerr = io.EOF
			}
		}
		if n > 0 {
			w := n
			if n%sectorAlign != 0 {
				w = roundUp(n)
				clear(buf[n:w])
			}
			if _, err := out.Write(buf[:w]); err != nil {
				return total, fmt.Errorf("writing: %w", err)
			}
			total += int64(n)
			if n%sectorAlign != 0 {
				// A partial block can only be the last one.
				if err := setEOF(out, total); err != nil {
					return total, fmt.Errorf("setting the length: %w", err)
				}
				return total, nil
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, fmt.Errorf("reading: %w", rerr)
		}
	}
}
