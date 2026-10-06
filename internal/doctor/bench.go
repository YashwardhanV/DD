package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// Bench settings (BUILD.md stage 11).
const (
	BenchFileName  = ".drivedex-bench.tmp"
	BenchSize      = 256 << 20 // bytes written
	BenchChunk     = 4 << 20   // written per call
	BenchMinFree   = 600 << 20 // free space needed before starting
	slowLimitMBps  = 25.0
	usb2LimitMBps  = 60.0
	megabyte       = 1 << 20
	benchSeedValue = 1
)

// BenchOptions are the knobs of Bench; the zero value means "the standard test".
// (Tests use small sizes so they stay fast.)
type BenchOptions struct {
	Size    int64
	Chunk   int
	MinFree uint64
	// FreeBytes reports the free space of the drive holding a path. nil skips the check.
	FreeBytes func(path string) (uint64, error)
	// Progress is called after every chunk with the bytes written so far.
	Progress func(done, total int64)
}

func (o *BenchOptions) fill() {
	if o.Size <= 0 {
		o.Size = BenchSize
	}
	if o.Chunk <= 0 {
		o.Chunk = BenchChunk
	}
	if o.MinFree == 0 {
		o.MinFree = BenchMinFree
	}
}

// BenchResult is the measured write speed.
type BenchResult struct {
	Bytes   int64
	Elapsed time.Duration
	MBps    float64
}

// ErrNotEnoughSpace means the drive has less free space than the test needs.
var ErrNotEnoughSpace = errors.New("not enough free space for the speed test")

// ErrBenchFileExists means a file with the test's name is already there.
var ErrBenchFileExists = errors.New("a file with the speed test's name already exists")

// Bench measures how fast root (a drive such as `E:\`) accepts writes.
//
// It is the ONLY place doctor writes to a drive. It creates one temporary file,
// writes Size bytes in chunks, flushes them to the device (f.Sync) so the
// number reflects the drive and not Windows' memory cache, and ALWAYS deletes
// the file again - on success, on error and on Ctrl+C. It will not touch a
// file that already has the test's name: it only deletes a file it created.
func Bench(ctx context.Context, root string, opt BenchOptions) (res BenchResult, err error) {
	opt.fill()
	if opt.FreeBytes != nil {
		free, ferr := opt.FreeBytes(root)
		if ferr != nil {
			return res, fmt.Errorf("checking free space on %s: %w", root, ferr)
		}
		if free < opt.MinFree {
			return res, fmt.Errorf("%w: %s has %d MB free, the test needs %d MB", ErrNotEnoughSpace, root, free/megabyte, opt.MinFree/megabyte)
		}
	}

	path := filepath.Join(root, BenchFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // O_EXCL: never reuse or overwrite an existing file
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return res, fmt.Errorf("%w: %s (left over from an earlier run? delete it yourself and try again)", ErrBenchFileExists, path)
		}
		return res, fmt.Errorf("creating the test file %s: %w", path, err)
	}
	// From here on we own the file, so whatever happens it gets removed.
	defer func() {
		closeErr := f.Close() // Windows can't delete an open file
		rmErr := os.Remove(path)
		if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("couldn't delete the test file - please delete %s yourself: %w", path, rmErr))
		} else if closeErr != nil && err == nil && !errors.Is(closeErr, os.ErrClosed) {
			err = fmt.Errorf("closing the test file: %w", closeErr)
		}
	}()

	// Random bytes, so a drive that compresses data can't cheat. One chunk,
	// reused: generating 256 MB of random data would itself take time.
	buf := make([]byte, opt.Chunk)
	rand.New(rand.NewSource(benchSeedValue)).Read(buf)

	start := time.Now()
	var written int64
	for written < opt.Size {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		n := int64(len(buf))
		if rest := opt.Size - written; rest < n {
			n = rest
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return res, fmt.Errorf("writing to %s: %w", path, err)
		}
		written += n
		if opt.Progress != nil {
			opt.Progress(written, opt.Size)
		}
	}
	if err := f.Sync(); err != nil {
		return res, fmt.Errorf("flushing to the drive: %w", err)
	}
	res.Elapsed = time.Since(start)
	res.Bytes = written
	if res.Elapsed <= 0 {
		res.Elapsed = time.Nanosecond
	}
	res.MBps = float64(written) / megabyte / res.Elapsed.Seconds()
	return res, nil
}

// Interpret turns a write speed into the advice from BUILD.md.
func Interpret(mbps float64) Line {
	text := fmt.Sprintf("write speed %.1f MB/s", mbps)
	switch {
	case mbps < slowLimitMBps:
		return Line{Fail, text + " - very slow: likely a USB 2.0 port/cable or a low-end drive",
			"try a different port and cable (a blue or \"SS\" USB 3 port); if it stays slow, the drive itself is slow"}
	case mbps <= usb2LimitMBps:
		return Line{Warn, text + " - USB 2.0-class speed",
			"try a blue/\"SS\" USB 3 port and a different cable"}
	default:
		return Line{OK, text + " - good", ""}
	}
}

// WriteBenchReport prints the result of a speed test.
func WriteBenchReport(w io.Writer, root string, res BenchResult) {
	l := Interpret(res.MBps)
	fmt.Fprintf(w, "  %s %s (%d MB in %.1f s)\n", l.Level.glyph(), l.Text, res.Bytes/megabyte, res.Elapsed.Seconds())
	if l.Tip != "" {
		fmt.Fprintf(w, "       → %s\n", l.Tip)
	}
}
