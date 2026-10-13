//go:build windows

package portable

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	cfgmgr32                     = windows.NewLazySystemDLL("cfgmgr32.dll")
	procCMLocateDevNodeW         = cfgmgr32.NewProc("CM_Locate_DevNodeW")
	procCMGetParent              = cfgmgr32.NewProc("CM_Get_Parent")
	procCMGetDeviceIDW           = cfgmgr32.NewProc("CM_Get_Device_IDW")
	procCMGetDevNodeRegPropertyW = cfgmgr32.NewProc("CM_Get_DevNode_Registry_PropertyW")
)

const (
	crSuccess          = 0
	cmLocateNormal     = 0
	cmDrpDeviceDesc    = 0x00000001
	cmDrpFriendlyName  = 0x0000000D
	maxDeviceIDLen     = 200
	maxPropertyBufSize = 1024
)

var wpdGUID = windows.GUID{Data1: 0x6AC27878, Data2: 0xA6FA, Data3: 0x4155, Data4: [8]byte{0xBA, 0x85, 0xF9, 0x8F, 0x49, 0x1D, 0x4F, 0x33}}

// List returns the portable devices connected over USB right now. It only
// asks Windows' device list (no device is opened), so it is cheap enough to
// call every few seconds.
func List() ([]Device, error) {
	paths, err := windows.CM_Get_Device_Interface_List("", &wpdGUID, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if errors.Is(err, windows.ERROR_NO_SUCH_DEVICE_INTERFACE) {
		return nil, nil // x/sys reports an empty list as this error: nothing is connected
	}
	if err != nil {
		return nil, fmt.Errorf("listing portable devices: %w", err)
	}
	var devs []Device
	for _, p := range paths {
		if !IsUSBInterface(p) {
			continue
		}
		inst, ok := InstanceIDFromInterface(p)
		if !ok {
			continue
		}
		d := Device{InterfacePath: p, InstanceID: inst}
		if node, ok := locate(inst); ok {
			d.Name = nodeString(node, cmDrpFriendlyName)
			if d.Name == "" {
				d.Name = nodeString(node, cmDrpDeviceDesc)
			}
			d.Serial = SerialFromInstanceID(inst)
			if d.Serial == "" {
				// A phone is usually a "composite" USB device: the MTP part is a
				// child whose made-up ID has no serial, but its parent's does.
				if parent, ok := parentOf(node); ok {
					if id, ok := deviceID(parent); ok {
						d.Serial = SerialFromInstanceID(id)
					}
				}
			}
		}
		devs = append(devs, d)
	}
	return devs, nil
}

func locate(instanceID string) (uint32, bool) {
	p, err := windows.UTF16PtrFromString(instanceID)
	if err != nil {
		return 0, false
	}
	var node uint32
	r, _, _ := procCMLocateDevNodeW.Call(uintptr(unsafe.Pointer(&node)), uintptr(unsafe.Pointer(p)), cmLocateNormal)
	return node, r == crSuccess
}

func parentOf(node uint32) (uint32, bool) {
	var parent uint32
	r, _, _ := procCMGetParent.Call(uintptr(unsafe.Pointer(&parent)), uintptr(node), 0)
	return parent, r == crSuccess
}

func deviceID(node uint32) (string, bool) {
	buf := make([]uint16, maxDeviceIDLen+1)
	r, _, _ := procCMGetDeviceIDW.Call(uintptr(node), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	if r != crSuccess {
		return "", false
	}
	return windows.UTF16ToString(buf), true
}

// nodeString reads a text property (friendly name, description) of a device.
func nodeString(node uint32, prop uint32) string {
	buf := make([]uint16, maxPropertyBufSize/2)
	size := uint32(len(buf) * 2)
	var regType uint32
	r, _, _ := procCMGetDevNodeRegPropertyW.Call(uintptr(node), uintptr(prop), uintptr(unsafe.Pointer(&regType)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if r != crSuccess {
		return ""
	}
	return windows.UTF16ToString(buf)
}
