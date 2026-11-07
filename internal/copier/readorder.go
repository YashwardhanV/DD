package copier

import (
	"sort"
)

// Read orders for small files (--read-order).
const (
	ReadOrderAuto = "auto" // currently the same as size (see below)
	ReadOrderSize = "size" // biggest first (the default)
	ReadOrderDisk = "disk" // in the order the files lie on the disk
)

// ValidReadOrder reports whether s is a known --read-order value.
func ValidReadOrder(s string) bool {
	switch s {
	case ReadOrderAuto, ReadOrderSize, ReadOrderDisk, "":
		return true
	}
	return false
}

// Why read in disk order (experimental)
//
// On a spinning hard disk, every jump of the read head costs several
// milliseconds. Copying 20,000 small photos biggest-first makes the head jump
// all over the platter; reading them in the order they lie on the disk lets it
// sweep across once. Two ways to know that order:
//
//   - cheap: the NTFS file reference number. Files created together get
//     nearby numbers and usually nearby places on the disk. Costs nothing.
//   - exact: ask NTFS where the file's first cluster is
//     (FSCTL_GET_RETRIEVAL_POINTERS; no Administrator rights needed). Files
//     small enough to live inside their MFT record have no cluster; they are
//     placed by reference number.
//
// "auto" stays "size" until a benchmark on a real USB hard disk shows a gain
// (PROGRESS.md / docs/BENCHMARKS.md).

// firstLCN returns the disk position of a file's first cluster (Windows).
// A variable so tests can provide positions.
var firstLCN = platformFirstLCN

// diskOrder returns the small (non-big) items to copy, sorted by where they
// are on the disk.
func diskOrder(items []Item, isBig func(*Item) bool) []*Item {
	type keyed struct {
		it  *Item
		lcn int64
		has bool
	}
	var ks []keyed
	for i := range items {
		it := &items[i]
		if it.Decision.Skipped() || isBig(it) {
			continue
		}
		lcn, ok := firstLCN(it.Src)
		ks = append(ks, keyed{it, lcn, ok})
	}
	sort.SliceStable(ks, func(a, b int) bool {
		ka, kb := ks[a], ks[b]
		if ka.has != kb.has {
			return ka.has // files with a known position first, in disk order
		}
		if ka.has && ka.lcn != kb.lcn {
			return ka.lcn < kb.lcn
		}
		return ka.it.SrcFileID < kb.it.SrcFileID // then by reference number (creation order)
	})
	out := make([]*Item, len(ks))
	for i, k := range ks {
		out[i] = k.it
	}
	return out
}
