package index

import (
	"os"
	"path/filepath"
	"strings"
)

// JoinRoot joins a source root such as `E:\` (or a Linux folder, in tests)
// with an index path that uses `\` separators, giving a path the OS can open.
func JoinRoot(root, rel string) string {
	return filepath.Join(root, strings.ReplaceAll(rel, `\`, string(filepath.Separator)))
}

// FillMeta asks the file system for the real size and modification time of the
// entry in slot, found at fullPath, and stores them. It returns false if the
// file can't be examined (for instance it was deleted since indexing); the
// entry then keeps FlagNoMeta.
//
// MFT-built indexes know names but not sizes or dates (NTFS keeps those
// elsewhere). Looking them up only for the handful of results being displayed
// is cheap, and is why turbo mode costs nothing in day-to-day use. Never use it
// for phone sources: their paths are not files on this PC.
func (ix *Index) FillMeta(slot int, fullPath string) bool {
	st, err := os.Lstat(fullPath)
	if err != nil {
		return false
	}
	if st.IsDir() {
		ix.Sizes[slot] = 0
	} else {
		ix.Sizes[slot] = st.Size()
	}
	ix.MTimes[slot] = st.ModTime().Unix()
	ix.Flags[slot] &^= FlagNoMeta
	return true
}
