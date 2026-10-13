//go:build windows

package portable

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// Reading a device goes through the Windows Shell (Shell.Application), the
// same layer Explorer uses, so anything Explorer can browse - Android MTP,
// iPhone and camera PTP, media players - can be indexed. The Shell is a COM
// "apartment-threaded" object: every call must come from the thread that
// created it, so a session locks its goroutine to one OS thread until the
// device's top-level folder is closed.

const (
	ssfDrives   = 17         // Shell special folder "This PC"
	sFalse      = 1          // S_FALSE: COM was already initialized on this thread
	hrWrongMode = 0x80010106 // RPC_E_CHANGED_MODE
)

// session is one thread's Shell.Application.
type session struct {
	shell  *ole.IDispatch
	uninit bool
}

func openSession() (*session, error) {
	runtime.LockOSThread()
	s := &session{}
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		var oe *ole.OleError
		if !errors.As(err, &oe) || (oe.Code() != sFalse && oe.Code() != hrWrongMode) {
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("starting COM: %w", err)
		}
		s.uninit = oe.Code() == sFalse // S_FALSE still has to be balanced
	} else {
		s.uninit = true
	}
	unk, err := oleutil.CreateObject("Shell.Application")
	if err != nil {
		s.close()
		return nil, fmt.Errorf("starting the Windows Shell: %w", err)
	}
	s.shell, err = unk.QueryInterface(ole.IID_IDispatch)
	unk.Release()
	if err != nil {
		s.close()
		return nil, fmt.Errorf("starting the Windows Shell: %w", err)
	}
	return s, nil
}

func (s *session) close() {
	if s.shell != nil {
		s.shell.Release()
		s.shell = nil
	}
	if s.uninit {
		ole.CoUninitialize()
	}
	runtime.UnlockOSThread()
}

// namespace returns Shell.NameSpace(arg) as a folder (nil if the Shell has no such folder).
func (s *session) namespace(arg any) (*ole.IDispatch, error) {
	v, err := oleutil.CallMethod(s.shell, "NameSpace", arg)
	if err != nil {
		return nil, err
	}
	return v.ToIDispatch(), nil // nil when NameSpace returned "nothing"
}

// shellFolder is a Folder backed by a Shell Folder object. The top-level one
// owns the session and ends it on Close.
type shellFolder struct {
	disp *ole.IDispatch
	sess *session // set on the top-level folder only
}

// OpenDevice opens a connected device's top level (its storages) for
// scanning. The returned Folder, and everything listed from it, must be used
// and closed on the calling goroutine.
func OpenDevice(d Device) (Folder, error) {
	s, err := openSession()
	if err != nil {
		return nil, err
	}
	disp, err := findDevice(s, d)
	if err != nil {
		s.close()
		return nil, err
	}
	return &shellFolder{disp: disp, sess: s}, nil
}

// openShellPath opens any folder the Shell understands (tests use a local folder).
func openShellPath(path string) (Folder, error) {
	s, err := openSession()
	if err != nil {
		return nil, err
	}
	disp, err := s.namespace(path)
	if err != nil || disp == nil {
		s.close()
		return nil, fmt.Errorf("the Shell can't open %s (%v)", path, err)
	}
	return &shellFolder{disp: disp, sess: s}, nil
}

// findDevice finds the device under This PC. The Shell's path for a device
// contains its WPD interface path, which is how it is matched; the name is the
// fallback when exactly one device item has it.
func findDevice(s *session, d Device) (*ole.IDispatch, error) {
	thisPC, err := s.namespace(ssfDrives)
	if err != nil || thisPC == nil {
		return nil, fmt.Errorf("opening This PC: %v", err)
	}
	defer thisPC.Release()
	items, count, err := folderItems(thisPC)
	if err != nil {
		return nil, err
	}
	defer items.Release()

	want := strings.ToLower(d.InterfacePath)
	var byName *ole.IDispatch
	named := 0
	for i := 0; i < count; i++ {
		it, err := itemAt(items, i)
		if err != nil || it == nil {
			continue
		}
		path := strings.ToLower(propString(it, "Path"))
		if want != "" && strings.Contains(path, want) {
			if byName != nil {
				byName.Release()
			}
			f, err := subFolder(it)
			it.Release()
			if err != nil || f == nil {
				return nil, fmt.Errorf("opening %s: %v", d.Label(), err)
			}
			return f, nil
		}
		if !looksLikeDrive(path) && strings.EqualFold(propString(it, "Name"), d.Label()) {
			named++
			if byName == nil {
				byName = it
				continue
			}
		}
		it.Release()
	}
	if byName != nil {
		defer byName.Release()
		if named == 1 {
			if f, err := subFolder(byName); err == nil && f != nil {
				return f, nil
			}
		}
	}
	return nil, fmt.Errorf("%s isn't visible in This PC - unlock it and choose \"File transfer\" on its USB notification", d.Label())
}

// looksLikeDrive reports whether a This PC item is a drive ("e:\").
func looksLikeDrive(path string) bool { return len(path) >= 2 && path[1] == ':' }

// List implements Folder.
func (f *shellFolder) List(ctx context.Context) ([]Entry, error) {
	items, count, err := folderItems(f.disp)
	if err != nil {
		return nil, err
	}
	defer items.Release()
	out := make([]Entry, 0, count)
	for i := 0; i < count; i++ {
		if err := ctx.Err(); err != nil {
			closeAll(out)
			return nil, err
		}
		it, err := itemAt(items, i)
		if err != nil || it == nil {
			continue
		}
		e := readItem(it)
		it.Release()
		out = append(out, e)
	}
	return out, nil
}

// Close implements Folder.
func (f *shellFolder) Close() {
	if f.disp != nil {
		f.disp.Release()
		f.disp = nil
	}
	if f.sess != nil {
		f.sess.close()
		f.sess = nil
	}
}

// archiveExts are files the Shell can open like folders. They are indexed as
// files - their contents aren't on the device as separate files.
var archiveExts = []string{".zip", ".cab"}

// readItem reads one Shell FolderItem.
func readItem(it *ole.IDispatch) Entry {
	e := Entry{Item: Item{Size: -1}}
	// The plain Name hides extensions when Explorer is set to; the file name property doesn't.
	e.Name = extString(it, "System.FileName")
	if e.Name == "" {
		e.Name = extString(it, "System.ItemNameDisplay")
	}
	if e.Name == "" {
		e.Name = propString(it, "Name")
	}
	isFolder := propBool(it, "IsFolder")
	lower := strings.ToLower(e.Name)
	for _, ext := range archiveExts {
		if strings.HasSuffix(lower, ext) {
			isFolder = false
		}
	}
	if isFolder {
		e.IsDir = true
		if sub, err := subFolder(it); err == nil && sub != nil {
			e.Sub = &shellFolder{disp: sub}
		}
		return e
	}
	e.Size = itemSize(it)
	e.ModTime = itemTime(it)
	return e
}

func folderItems(folder *ole.IDispatch) (*ole.IDispatch, int, error) {
	v, err := oleutil.CallMethod(folder, "Items")
	if err != nil {
		return nil, 0, fmt.Errorf("listing: %w", err)
	}
	items := v.ToIDispatch()
	if items == nil {
		return nil, 0, errors.New("listing: the Shell returned nothing")
	}
	c, err := oleutil.GetProperty(items, "Count")
	if err != nil {
		items.Release()
		return nil, 0, fmt.Errorf("counting: %w", err)
	}
	n, _ := toInt64(c.Value())
	return items, int(n), nil
}

func itemAt(items *ole.IDispatch, i int) (*ole.IDispatch, error) {
	v, err := oleutil.CallMethod(items, "Item", i)
	if err != nil {
		return nil, err
	}
	return v.ToIDispatch(), nil
}

func subFolder(it *ole.IDispatch) (*ole.IDispatch, error) {
	v, err := oleutil.GetProperty(it, "GetFolder")
	if err != nil {
		return nil, err
	}
	return v.ToIDispatch(), nil
}

func propString(it *ole.IDispatch, name string) string {
	v, err := oleutil.GetProperty(it, name)
	if err != nil {
		return ""
	}
	defer v.Clear()
	return v.ToString()
}

func propBool(it *ole.IDispatch, name string) bool {
	v, err := oleutil.GetProperty(it, name)
	if err != nil {
		return false
	}
	b, _ := v.Value().(bool)
	return b
}

func extString(it *ole.IDispatch, prop string) string {
	v, err := oleutil.CallMethod(it, "ExtendedProperty", prop)
	if err != nil {
		return ""
	}
	defer v.Clear()
	return strings.TrimSpace(v.ToString())
}

// itemSize prefers the 64-bit System.Size (FolderItem.Size is 32-bit and
// wrong for files over 2 GB). -1 means unknown.
func itemSize(it *ole.IDispatch) int64 {
	if v, err := oleutil.CallMethod(it, "ExtendedProperty", "System.Size"); err == nil {
		n, ok := toInt64(v.Value())
		v.Clear()
		if ok && n >= 0 {
			return n
		}
	}
	if v, err := oleutil.GetProperty(it, "Size"); err == nil {
		if n, ok := toInt64(v.Value()); ok && n >= 0 {
			return n
		}
	}
	return -1
}

// itemTime returns the modification time in unix seconds (0 = unknown).
// The Shell gives local wall-clock time; go-ole labels it UTC, so it is
// re-read as local time here.
func itemTime(it *ole.IDispatch) int64 {
	v, err := oleutil.GetProperty(it, "ModifyDate")
	if err != nil {
		return 0
	}
	t, ok := v.Value().(time.Time)
	if !ok || t.Year() < 1980 {
		return 0
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local).Unix()
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int8:
		return int64(n), true
	case uint8:
		return int64(n), true
	case int16:
		return int64(n), true
	case uint16:
		return int64(n), true
	case int32:
		return int64(n), true
	case uint32:
		return int64(n), true
	case int64:
		return n, true
	case uint64:
		return int64(n), true
	case int:
		return int64(n), true
	case uint:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}
