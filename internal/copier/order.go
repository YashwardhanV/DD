package copier

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yashwardhanv/dd/internal/copier/trace"
)

// Disk-order reads (BUILD-V2 stage 20)
//
// On a spinning disk the read head has to move to wherever each file lives.
// v1 reads small files largest-first: for the head that is a random order -
// a long jump for nearly every file. Sorting the small files by WHERE they sit
// on the platter (their first "logical cluster number", LCN) turns most of
// those jumps into short forward moves.
//
// Windows tells any program, without Administrator rights, where a file's
// data starts: FSCTL_GET_RETRIEVAL_POINTERS on a handle opened only to read
// attributes. Tiny files stored inside the NTFS master file table itself
// have no clusters of their own; they get key 0 and are read first, together.
//
// Asking costs one request per file, so the pass is timed: if it would take
// more than 10% of the copy's estimated time, it is abandoned and the v1
// (size) order is used.

// Copy orders.
const (
	OrderSize = "size" // largest first (v1)
	OrderName = "name" // by path
	OrderDisk = "disk" // by first cluster on the disk (hard disks, NTFS)
)

// ErrResident means the file's data lives inside the MFT record (no clusters).
var ErrResident = errors.New("the file's data is stored in its MFT record")

// Estimates for "how long will this copy take" on a USB hard disk, used only
// to budget the disk-order pass.
const (
	hddEstimateBytesPerSec = 80e6
	hddEstimatePerFile     = 8 * time.Millisecond
	orderBudgetShare       = 0.10
	orderWorkers           = 2
)

// DiskOrderBudget is 10% of a rough estimate of the copy time.
func DiskOrderBudget(files, bytes int64) time.Duration {
	est := time.Duration(float64(bytes)/hddEstimateBytesPerSec*float64(time.Second)) + time.Duration(files)*hddEstimatePerFile
	return time.Duration(float64(est) * orderBudgetShare)
}

// OrderSmallFiles reorders the plan's small files (smaller than BigFileSize;
// big files keep their place at the front, largest first) and reports the
// order used, with a reason when it isn't the one asked for. lcnOf is
// FirstLCN (a variable for tests).
func (p *Plan) OrderSmallFiles(ctx context.Context, order string, lcnOf func(string) (int64, error), budget time.Duration) (used, why string) {
	if p.Phone != nil {
		return OrderSize, ""
	}
	var small []int
	for i := range p.Items {
		if p.Items[i].Size < bigFileSize && !p.Items[i].Decision.Skipped() {
			small = append(small, i)
		}
	}
	switch order {
	case OrderName:
		p.reorder(small, nil)
		return OrderName, ""
	case OrderDisk:
		if len(small) < 2 {
			return OrderSize, ""
		}
		tr := trace.From(ctx)
		m := tr.Start()
		keys, err := lcnKeys(ctx, p, small, lcnOf, budget)
		tr.End(trace.PlanList, m)
		if err != nil {
			return OrderSize, err.Error()
		}
		p.reorder(small, keys)
		return OrderDisk, ""
	}
	return OrderSize, ""
}

// reorder sorts the items at the given positions among themselves: by
// keys[k] (the key of the item at pos[k]), then by path; by path alone when
// keys is nil.
func (p *Plan) reorder(pos []int, keys []int64) {
	type keyed struct {
		it  Item
		key int64
	}
	items := make([]keyed, len(pos))
	for k, i := range pos {
		items[k].it = p.Items[i]
		if keys != nil {
			items[k].key = keys[k]
		}
	}
	sort.SliceStable(items, func(a, b int) bool {
		if items[a].key != items[b].key {
			return items[a].key < items[b].key
		}
		return items[a].it.Rel < items[b].it.Rel
	})
	for k, i := range pos {
		p.Items[i] = items[k].it
	}
}

// lcnKeys looks up every small file's first cluster with two workers, giving
// up when the budget runs out or the file system doesn't answer.
func lcnKeys(ctx context.Context, p *Plan, small []int, lcnOf func(string) (int64, error), budget time.Duration) ([]int64, error) {
	keys := make([]int64, len(small))
	if budget <= 0 {
		budget = time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var failures, answered atomic.Int64
	var wg sync.WaitGroup
	next := atomic.Int64{}
	for w := 0; w < orderWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				k := int(next.Add(1) - 1)
				if k >= len(small) || dctx.Err() != nil {
					return
				}
				lcn, err := lcnOf(p.Items[small[k]].Src)
				switch {
				case err == nil:
					keys[k] = max(lcn, 0)
					answered.Add(1)
				case errors.Is(err, ErrResident):
					keys[k] = 0 // inside the MFT: read first, together
					answered.Add(1)
				default:
					keys[k] = 1<<62 + int64(k) // unknown: last, in plan order
					if failures.Add(1) >= 16 && answered.Load() == 0 {
						cancel() // the file system doesn't answer (not NTFS?)
					}
				}
			}
		}()
	}
	wg.Wait()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case answered.Load() == 0:
		return nil, errors.New("the file system didn't say where the files are (not NTFS?)")
	case dctx.Err() != nil:
		return nil, fmt.Errorf("finding where the files sit took longer than 10%% of the copy (%s); using size order", budget.Round(time.Millisecond))
	}
	return keys, nil
}

// ParseRetrievalPointers reads the first extent's LCN from the answer to
// FSCTL_GET_RETRIEVAL_POINTERS (RETRIEVAL_POINTERS_BUFFER):
//
//	0 ExtentCount u32   (4 padding)   8 StartingVcn i64   16 Extents[0].NextVcn i64   24 Extents[0].Lcn i64
func ParseRetrievalPointers(buf []byte) (int64, error) {
	if len(buf) < 32 {
		return 0, fmt.Errorf("retrieval pointers: %d bytes, need 32", len(buf))
	}
	if binary.LittleEndian.Uint32(buf) == 0 {
		return 0, ErrResident
	}
	return int64(binary.LittleEndian.Uint64(buf[24:])), nil
}
