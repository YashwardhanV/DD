//go:build windows

package copier

import (
	"encoding/binary"
	"errors"

	"golang.org/x/sys/windows"
)

// FirstLCN returns the logical cluster number where a file's data starts.
// The file is opened only to read its attributes (FILE_READ_ATTRIBUTES), so
// no Administrator rights are needed and its contents aren't touched.
func FirstLCN(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var in [8]byte // STARTING_VCN_INPUT_BUFFER: StartingVcn = 0
	out := make([]byte, 32)
	var n uint32
	err = windows.DeviceIoControl(h, windows.FSCTL_GET_RETRIEVAL_POINTERS, &in[0], uint32(len(in)), &out[0], uint32(len(out)), &n, nil)
	switch {
	case errors.Is(err, windows.ERROR_HANDLE_EOF):
		return 0, ErrResident // no clusters: the data lives in the MFT record
	case errors.Is(err, windows.ERROR_MORE_DATA):
		// More extents follow; the first one is all we need.
	case err != nil:
		return 0, err
	}
	if n < 32 && binary.LittleEndian.Uint32(out) != 0 {
		return 0, errors.New("short answer from FSCTL_GET_RETRIEVAL_POINTERS")
	}
	return ParseRetrievalPointers(out)
}
