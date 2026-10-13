package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// These two are variables (not direct calls) so tests can pretend a pen drive
// is plugged in. In the real program they are the Windows implementations.
var (
	listVolumes   = volume.List
	volumeForPath = volume.ForPath
	systemDrive   = func() string { return os.Getenv("SystemDrive") }
)

// phoneCheckTimeout bounds the "which phones are connected?" check that runs
// during search/list, so a stuck adb can never make searching hang.
const phoneCheckTimeout = 5 * time.Second

// currentEnv works out what is connected right now, so results can show the
// drive's CURRENT letter (found by serial number) or an "offline" marker.
func currentEnv(wantPhones bool) index.Env {
	env := index.Env{Mounts: map[string]string{}, Phones: map[string]bool{}, Devices: map[string]bool{}}
	vols, _ := listVolumes() // on error we simply know of no drives
	for _, v := range vols {
		if v.Ready && v.Serial != 0 {
			env.Mounts[volume.SourceID(v)] = v.Root
		}
	}
	if wantPhones {
		phoneEnv(&env) // asks adb once; the answer is used for the whole command
	}
	deviceEnv(&env)
	return env
}

// volumeHeader is the starting index header for a whole-drive source.
func volumeHeader(v volume.Volume) index.Header {
	return index.Header{
		SourceKind: index.KindVolume,
		SourceID:   volume.SourceID(v),
		Label:      v.Label,
		FileSystem: v.FileSystem,
		RootPath:   v.Root,
		LastMount:  v.Root,
		TotalBytes: int64(v.TotalBytes),
		Scanner:    "walk",
	}
}

// headerFor decides what kind of source the user's path is: a whole drive
// ("E:" - remembered by serial number) or just a folder.
func headerFor(arg, root string) (index.Header, error) {
	if !volume.IsDriveRoot(arg) {
		return index.FolderHeader(root), nil
	}
	v, err := volumeForPath(root)
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		return index.FolderHeader(root), nil // not Windows: treat the path as a plain folder
	case err != nil:
		return index.Header{}, userErr(fmt.Sprintf("can't identify drive %s", arg), "run 'drivedex drives' to see what is connected", err)
	case !v.Ready:
		return index.Header{}, userErr(fmt.Sprintf("drive %s isn't ready", v.Letter()), "is a disk inserted? (empty card readers show up as drives)", nil)
	case v.Serial == 0:
		return index.FolderHeader(root), nil // no usable serial number: fall back to a folder source
	}
	return volumeHeader(v), nil
}

// indexedSummary describes a drive's index for the `drives` table.
func indexedSummary(h index.Header, now time.Time) string {
	return fmt.Sprintf("yes (%s, %s entries)", ui.Ago(h.UpdatedAt, now), ui.Compact(int64(h.EntryCount)))
}

// letterOf extracts "E:" from a last-mount string such as `E:\`.
func letterOf(lastMount string) string {
	l, ok := volume.NormalizeLetter(lastMount)
	if !ok {
		return ""
	}
	return l
}

// describeIndex is a short human name for an index: its label, or its ID.
func describeIndex(h index.Header) string {
	if strings.TrimSpace(h.Label) != "" {
		return h.Label
	}
	return h.SourceID
}
