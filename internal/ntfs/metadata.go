package ntfs

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/yashwardhanv/dd/internal/index"
)

// FillMetadata gives every entry marked FlagNoMeta its real size and
// modification time by asking the file system (os.Lstat), using `workers`
// goroutines. root is the drive root such as `E:\`.
//
// MFT enumeration is fast precisely because it doesn't read sizes and dates;
// this is the slower, complete alternative (`index --with-metadata`).
//
// It returns how many entries were filled and how many couldn't be read (for
// example because the file was deleted since the scan). Those keep FlagNoMeta.
func FillMetadata(ctx context.Context, ix *index.Index, root string, workers int, progress func(done, total int64)) (filled, missing int64, err error) {
	var todo []int
	for slot := 0; slot < ix.Len(); slot++ {
		if f := ix.Flags[slot]; f&index.FlagNoMeta != 0 && f&index.FlagDeleted == 0 {
			todo = append(todo, slot)
		}
	}
	if len(todo) == 0 {
		return 0, 0, nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(todo) {
		workers = len(todo)
	}

	var okCount, badCount, done atomic.Int64
	var wg sync.WaitGroup
	chunk := (len(todo) + workers - 1) / workers
	for start := 0; start < len(todo); start += chunk {
		end := min(start+chunk, len(todo))
		wg.Add(1)
		go func(slots []int) {
			defer wg.Done()
			// Each worker writes only its own slots, so no locking is needed;
			// the per-worker cache keeps folder path building cheap.
			cache := map[uint64]string{}
			for i, slot := range slots {
				if i%256 == 0 && ctx.Err() != nil {
					return
				}
				if ix.FillMeta(slot, index.JoinRoot(root, ix.RelPathCached(slot, cache))) {
					okCount.Add(1)
				} else {
					badCount.Add(1)
				}
				if n := done.Add(1); progress != nil && n%2048 == 0 {
					progress(n, int64(len(todo)))
				}
			}
		}(todo[start:end])
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return okCount.Load(), badCount.Load(), err
	}
	if progress != nil {
		progress(int64(len(todo)), int64(len(todo)))
	}
	return okCount.Load(), badCount.Load(), nil
}
