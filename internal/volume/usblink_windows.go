//go:build windows

package volume

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// How a drive letter is traced to the USB port it is plugged into:
//
//	E:  --IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS-->  PhysicalDrive2
//	    --match IOCTL_STORAGE_GET_DEVICE_NUMBER over every disk-->  the disk's device
//	    --CM_Get_Parent, upwards-->  the USB device, whose parent is a USB hub
//	    --the device's "address" = its port number on that hub-->  ask the hub about that port
//
// Everything here only asks questions; nothing is changed, and no
// Administrator rights are needed (USBView, Microsoft's USB viewer, asks the
// same questions as a normal user).

var (
	cfgmgr32                      = windows.NewLazySystemDLL("cfgmgr32.dll")
	procCMLocateDevNodeW          = cfgmgr32.NewProc("CM_Locate_DevNodeW")
	procCMGetParent               = cfgmgr32.NewProc("CM_Get_Parent")
	procCMGetDeviceIDW            = cfgmgr32.NewProc("CM_Get_Device_IDW")
	procCMGetDevNodeRegPropertyW  = cfgmgr32.NewProc("CM_Get_DevNode_Registry_PropertyW")
	procCMGetDeviceInterfacePropW = cfgmgr32.NewProc("CM_Get_Device_Interface_PropertyW")
)

const (
	// USB hub IOCTLs: CTL_CODE(FILE_DEVICE_USB, function, METHOD_BUFFERED, FILE_ANY_ACCESS).
	ioctlUSBGetNodeInformation         = 0x220408 // 258
	ioctlUSBGetNodeConnectionInfoEx    = 0x220448 // 274
	ioctlUSBGetHubInformationEx        = 0x220454 // 277
	ioctlUSBGetPortConnectorProperties = 0x220458 // 278
	ioctlUSBGetNodeConnectionInfoExV2  = 0x22045C // 279

	ioctlStorageGetDeviceNumber = 0x002D1080

	cmCrSuccess    = 0
	cmLocateNormal = 0
	cmDrpAddress   = 0x1D // CM_DRP_ADDRESS: for a USB device, its port number on the parent hub
	maxInstanceID  = 200
	maxHubLevels   = 12 // a USB tree is at most 7 tiers deep; disk/adapter nodes add a few
)

var (
	guidDiskInterface   = windows.GUID{Data1: 0x53f56307, Data2: 0xb6bf, Data3: 0x11d0, Data4: [8]byte{0x94, 0xf2, 0x00, 0xa0, 0xc9, 0x1e, 0xfb, 0x8b}}
	guidUSBHubInterface = windows.GUID{Data1: 0xf18a0e88, Data2: 0xc30c, Data3: 0x11d0, Data4: [8]byte{0x88, 0x15, 0x00, 0xa0, 0xc9, 0x06, 0xbe, 0xd8}}
	devpkeyInstanceID   = windows.DEVPROPKEY{
		FmtID: windows.DEVPROPGUID{Data1: 0x78c34fc8, Data2: 0x104a, Data3: 0x4aca, Data4: [8]byte{0x9e, 0xa4, 0x52, 0x4d, 0x52, 0x99, 0x6e, 0x57}},
		PID:   256,
	}
)

// ErrNotUSB means the drive isn't connected through a USB hub (an internal
// SATA/NVMe disk, an SD card in a PCIe reader...).
var ErrNotUSB = errors.New("the drive is not connected by USB")

// USBLinkFor finds the USB port the drive holding root ("E:\") is plugged
// into and reports how fast it is running and could run.
func USBLinkFor(root string) (USBLink, error) {
	vol := filepath.VolumeName(root)
	if len(vol) != 2 || vol[1] != ':' {
		return USBLink{}, fmt.Errorf("%s is not a drive letter", root)
	}
	diskNo, err := volumeDisk(vol)
	if err != nil {
		return USBLink{}, fmt.Errorf("finding the disk under %s: %w", vol, err)
	}
	inst, err := diskInstanceID(diskNo)
	if err != nil {
		return USBLink{}, err
	}
	node, ok := cmLocate(inst)
	if !ok {
		return USBLink{}, fmt.Errorf("Windows has no device %s", inst)
	}
	for level := 0; level < maxHubLevels; level++ {
		parent, ok := cmParent(node)
		if !ok {
			break
		}
		parentID, ok := cmDeviceID(parent)
		if !ok {
			break
		}
		if hub := hubInterface(parentID); hub != "" {
			port, ok := cmDWORD(node, cmDrpAddress)
			if !ok || port == 0 {
				return USBLink{}, fmt.Errorf("couldn't find which port of %s the drive is in", parentID)
			}
			link, err := linkOnPort(hub, int(port))
			link.ViaHub = !IsRootHub(parentID)
			return link, err
		}
		node = parent
	}
	return USBLink{}, ErrNotUSB
}

// linkOnPort asks a hub about the device on one of its ports.
func linkOnPort(hubPath string, port int) (USBLink, error) {
	h, err := openHub(hubPath)
	if err != nil {
		return USBLink{}, err
	}
	defer windows.CloseHandle(h)

	link := USBLink{Port: port}
	ex, err := connectionInfoEx(h, port)
	if err != nil {
		return link, fmt.Errorf("asking the hub about port %d: %w", port, err)
	}
	var v2p *ConnectionInfoV2
	if v2, err := connectionInfoV2(h, port); err == nil {
		v2p = &v2
		link.PortKnown = true
		link.PortUSB3 = v2.PortUSB3
	}
	link.Speed, link.Capable = Speeds(ex, v2p)

	// On the USB 2.0 half of a socket? Then ask whether its other half is USB 3.
	if link.PortKnown && !link.PortUSB3 {
		if pc, err := portConnector(h, port); err == nil && pc.CompanionPort != 0 {
			link.PortTwinUSB3 = companionIsUSB3(h, hubPath, pc)
		}
	}
	return link, nil
}

func companionIsUSB3(h windows.Handle, hubPath string, pc PortConnector) bool {
	other := HubDevicePath(pc.CompanionHubLink)
	if other == "" || strings.EqualFold(other, hubPath) {
		v2, err := connectionInfoV2(h, pc.CompanionPort)
		return err == nil && v2.PortUSB3
	}
	oh, err := openHub(other)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(oh)
	v2, err := connectionInfoV2(oh, pc.CompanionPort)
	return err == nil && v2.PortUSB3
}

// USBPorts lists every port of every USB hub Windows knows about, with what
// each port supports.
func USBPorts() ([]PortInfo, error) {
	hubs, err := windows.CM_Get_Device_Interface_List("", &guidUSBHubInterface, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if errors.Is(err, windows.ERROR_NO_SUCH_DEVICE_INTERFACE) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing USB hubs: %w", err)
	}
	var ports []PortInfo
	for _, hub := range hubs {
		if hub == "" {
			continue
		}
		id, _ := interfaceInstanceID(hub)
		ports = append(ports, hubPorts(hub, IsRootHub(id))...)
	}
	return ports, nil
}

func hubPorts(hubPath string, root bool) []PortInfo {
	h, err := openHub(hubPath)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(h)
	n := hubPortCount(h)
	key := strings.ToLower(hubPath)
	var out []PortInfo
	for port := 1; port <= n; port++ {
		p := PortInfo{Hub: key, Number: port, Root: root, USB2: true}
		var v2p *ConnectionInfoV2
		if v2, err := connectionInfoV2(h, port); err == nil {
			v2p = &v2
			p.USB2, p.USB3 = v2.PortUSB2 || v2.PortUSB1, v2.PortUSB3
		}
		if pc, err := portConnector(h, port); err == nil {
			p.UserConnectable, p.TypeC = pc.UserConnectable, pc.TypeC
			p.CompanionPort = pc.CompanionPort
			if other := HubDevicePath(pc.CompanionHubLink); other != "" && !strings.EqualFold(other, hubPath) {
				p.CompanionHub = strings.ToLower(other)
			}
		} else {
			p.UserConnectable = true // no answer: assume it's a real socket
		}
		if ex, err := connectionInfoEx(h, port); err == nil && ex.Connected {
			p.Connected = true
			p.Speed, _ = Speeds(ex, v2p)
		}
		out = append(out, p)
	}
	return out
}

// ---- hub questions ------------------------------------------------------------------------

func openHub(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	// USBView opens hubs for writing; fall back to "no access" if that is refused.
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE, windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err == nil {
		return h, nil
	}
	return openForQuery(path)
}

// hubIoctl sends a USB hub IOCTL whose input is also its output buffer.
func hubIoctl(h windows.Handle, code uint32, buf []byte) ([]byte, error) {
	var returned uint32
	if err := windows.DeviceIoControl(h, code, &buf[0], uint32(len(buf)), &buf[0], uint32(len(buf)), &returned, nil); err != nil {
		return nil, err
	}
	return buf[:returned], nil
}

func connectionInfoEx(h windows.Handle, port int) (ConnectionInfoEx, error) {
	buf := make([]byte, 512) // fixed part + room for the open-pipe list
	binary.LittleEndian.PutUint32(buf, uint32(port))
	out, err := hubIoctl(h, ioctlUSBGetNodeConnectionInfoEx, buf)
	if err != nil {
		return ConnectionInfoEx{}, err
	}
	return ParseConnectionInfoEx(out)
}

func connectionInfoV2(h windows.Handle, port int) (ConnectionInfoV2, error) {
	out, err := hubIoctl(h, ioctlUSBGetNodeConnectionInfoExV2, ConnectionInfoV2Request(port))
	if err != nil {
		return ConnectionInfoV2{}, err
	}
	return ParseConnectionInfoV2(out)
}

func portConnector(h windows.Handle, port int) (PortConnector, error) {
	out, err := hubIoctl(h, ioctlUSBGetPortConnectorProperties, PortConnectorRequest(port))
	if err != nil {
		return PortConnector{}, err
	}
	return ParsePortConnector(out)
}

func hubPortCount(h windows.Handle) int {
	if out, err := hubIoctl(h, ioctlUSBGetHubInformationEx, make([]byte, 256)); err == nil {
		if n, err := HubPortCount(out); err == nil && n > 0 {
			return n
		}
	}
	if out, err := hubIoctl(h, ioctlUSBGetNodeInformation, make([]byte, 256)); err == nil {
		if n, err := HubPortCountLegacy(out); err == nil {
			return n
		}
	}
	return 0
}

// ---- device tree ---------------------------------------------------------------------------

// diskInstanceID finds the device instance ID of \\.\PhysicalDrive<n>.
func diskInstanceID(n uint32) (string, error) {
	disks, err := windows.CM_Get_Device_Interface_List("", &guidDiskInterface, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil {
		return "", fmt.Errorf("listing disks: %w", err)
	}
	for _, path := range disks {
		if path == "" {
			continue
		}
		out, err := deviceIoctl(path, ioctlStorageGetDeviceNumber, nil, 12)
		// STORAGE_DEVICE_NUMBER: DeviceType u32, DeviceNumber u32, PartitionNumber u32.
		if err != nil || len(out) < 8 || binary.LittleEndian.Uint32(out[4:]) != n {
			continue
		}
		if id, ok := interfaceInstanceID(path); ok {
			return id, nil
		}
		return "", fmt.Errorf("couldn't read the device ID of disk %d", n)
	}
	return "", fmt.Errorf("disk %d not found among the connected disks", n)
}

// hubInterface returns the device path of instanceID if it is a USB hub, else "".
func hubInterface(instanceID string) string {
	paths, err := windows.CM_Get_Device_Interface_List(instanceID, &guidUSBHubInterface, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil {
		return ""
	}
	for _, p := range paths {
		if p != "" {
			return p
		}
	}
	return ""
}

// interfaceInstanceID asks which device an interface path belongs to.
func interfaceInstanceID(path string) (string, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	buf := make([]uint16, maxInstanceID+1)
	size := uint32(len(buf) * 2)
	var propType uint32
	r, _, _ := procCMGetDeviceInterfacePropW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&devpkeyInstanceID)),
		uintptr(unsafe.Pointer(&propType)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if r == cmCrSuccess {
		return windows.UTF16ToString(buf), true
	}
	return instanceIDFromPath(path)
}

// instanceIDFromPath is the fallback: \\?\USB#VID_1&PID_2#SERIAL#{guid} -> USB\VID_1&PID_2\SERIAL.
func instanceIDFromPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, `\\?\`)
	if !ok {
		return "", false
	}
	i := strings.LastIndexByte(rest, '#')
	if i <= 0 {
		return "", false
	}
	return strings.ToUpper(strings.ReplaceAll(rest[:i], "#", `\`)), true
}

func cmLocate(instanceID string) (uint32, bool) {
	p, err := windows.UTF16PtrFromString(instanceID)
	if err != nil {
		return 0, false
	}
	var node uint32
	r, _, _ := procCMLocateDevNodeW.Call(uintptr(unsafe.Pointer(&node)), uintptr(unsafe.Pointer(p)), cmLocateNormal)
	return node, r == cmCrSuccess
}

func cmParent(node uint32) (uint32, bool) {
	var parent uint32
	r, _, _ := procCMGetParent.Call(uintptr(unsafe.Pointer(&parent)), uintptr(node), 0)
	return parent, r == cmCrSuccess
}

func cmDeviceID(node uint32) (string, bool) {
	buf := make([]uint16, maxInstanceID+1)
	r, _, _ := procCMGetDeviceIDW.Call(uintptr(node), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	if r != cmCrSuccess {
		return "", false
	}
	return windows.UTF16ToString(buf), true
}

func cmDWORD(node uint32, prop uint32) (uint32, bool) {
	var v, regType uint32
	size := uint32(4)
	r, _, _ := procCMGetDevNodeRegPropertyW.Call(uintptr(node), uintptr(prop), uintptr(unsafe.Pointer(&regType)),
		uintptr(unsafe.Pointer(&v)), uintptr(unsafe.Pointer(&size)), 0)
	return v, r == cmCrSuccess && size == 4
}
