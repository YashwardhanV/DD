package index

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// FolderSourceID is the stable ID of a folder source: "dir-" plus the first 12
// hex characters of the SHA-1 of the lowercase absolute path. The same folder
// always maps to the same index file; different folders never share one.
func FolderSourceID(absPath string) string {
	sum := sha1.Sum([]byte(strings.ToLower(absPath)))
	return "dir-" + hex.EncodeToString(sum[:])[:12]
}

// FolderHeader returns the starting header for indexing a folder.
func FolderHeader(absPath string) Header {
	label := filepath.Base(absPath)
	if label == "." || label == string(filepath.Separator) || label == `\` {
		label = absPath
	}
	return Header{
		SourceKind: KindFolder,
		SourceID:   FolderSourceID(absPath),
		Label:      label,
		RootPath:   absPath,
		Scanner:    "walk",
	}
}
