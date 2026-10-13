package volume

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Storage bus types (the values of Windows' STORAGE_BUS_TYPE).
const (
	BusTypeUnknown uint32 = 0
	BusTypeScsi    uint32 = 1
	BusTypeAtapi   uint32 = 2
	BusTypeAta     uint32 = 3
	BusType1394    uint32 = 4 // FireWire
	BusTypeUsb     uint32 = 7
	BusTypeRaid    uint32 = 8
	BusTypeSata    uint32 = 11
	BusTypeSd      uint32 = 12
	BusTypeMmc     uint32 = 13
	// Virtual disks: a mounted .vhd(x) file, Storage Spaces. Never "external".
	BusTypeVirtual           uint32 = 14
	BusTypeFileBackedVirtual uint32 = 15
	BusTypeSpaces            uint32 = 16
	BusTypeNvme              uint32 = 17
)

// BusName is a friendly name for a bus type ("USB", "SATA", "NVMe", ...).
func BusName(b uint32) string {
	switch b {
	case BusTypeScsi:
		return "SCSI"
	case BusTypeAtapi:
		return "ATAPI"
	case BusTypeAta:
		return "ATA"
	case BusType1394:
		return "FireWire"
	case BusTypeUsb:
		return "USB"
	case BusTypeRaid:
		return "RAID"
	case BusTypeSata:
		return "SATA"
	case BusTypeSd:
		return "SD"
	case BusTypeMmc:
		return "MMC"
	case BusTypeVirtual, BusTypeFileBackedVirtual:
		return "Virtual"
	case BusTypeSpaces:
		return "Storage Spaces"
	case BusTypeNvme:
		return "NVMe"
	case BusTypeUnknown:
		return "unknown"
	}
	return fmt.Sprintf("bus %d", b)
}

// BusInfo says how a drive is connected and what it calls itself.
type BusInfo struct {
	Bus     uint32
	Vendor  string // e.g. "SanDisk"
	Product string // e.g. "Ultra USB 3.0"
	// Hotplug is Windows' "this disk can be unplugged while the PC runs"
	// (STORAGE_HOTPLUG_INFO.DeviceHotplug). It is how an eSATA disk, a
	// Thunderbolt/NVMe enclosure or a hot-swap bay is told apart from an
	// internal SATA/NVMe disk, which all report the same bus.
	Hotplug bool
}

// IsUSB reports whether the drive is attached over USB.
func (b BusInfo) IsUSB() bool { return b.Bus == BusTypeUsb }

// External reports whether the disk is one you plug in and take away: USB,
// FireWire, an SD/MMC card, or any other disk Windows says is hot-pluggable.
// Virtual disks (mounted .vhdx files, Storage Spaces) never are.
func (b BusInfo) External() bool {
	switch b.Bus {
	case BusTypeUsb, BusType1394, BusTypeSd, BusTypeMmc:
		return true
	case BusTypeVirtual, BusTypeFileBackedVirtual, BusTypeSpaces:
		return false
	}
	return b.Hotplug
}

// IsExternal decides whether a drive is an external one - the kind the
// background agent indexes by itself as soon as it is plugged in. Drives that
// Windows calls "removable" (pen drives, memory cards) always are, whatever
// their bus reports; the Windows system drive never is.
func IsExternal(v Volume, b BusInfo, systemDrive string) bool {
	if sys, ok := NormalizeLetter(systemDrive); ok && strings.EqualFold(sys, v.Letter()) {
		return false
	}
	return v.DriveType == DriveRemovable || b.External()
}

// Connection describes how a drive is connected, for people:
// "USB · external", "SATA · internal", "NVMe · external (hot-plug)".
func Connection(v Volume, b BusInfo, systemDrive string) string {
	name := BusName(b.Bus)
	switch {
	case !IsExternal(v, b, systemDrive):
		return name + " · internal"
	case b.Hotplug && !b.IsUSB() && b.Bus != BusTypeSd && b.Bus != BusTypeMmc && b.Bus != BusType1394:
		return name + " · external (hot-plug)"
	}
	return name + " · external"
}

// Name combines vendor and product into a label like "SanDisk Ultra USB 3.0".
func (b BusInfo) Name() string {
	return strings.TrimSpace(strings.TrimSpace(b.Vendor) + " " + strings.TrimSpace(b.Product))
}

// STORAGE_DEVICE_DESCRIPTOR layout (byte offsets, little-endian):
//
//	 0 Version u32      4 Size u32          8 DeviceType u8      9 DeviceTypeModifier u8
//	10 RemovableMedia   11 CommandQueueing  12 VendorIdOffset u32
//	16 ProductIdOffset u32                  20 ProductRevisionOffset u32
//	24 SerialNumberOffset u32               28 BusType u32
//
// The vendor/product/... values are NUL-terminated ASCII strings stored later
// in the same buffer, at the given offsets (0 = absent).
const descriptorMinSize = 32

// ParseDeviceDescriptor reads a STORAGE_DEVICE_DESCRIPTOR returned by
// IOCTL_STORAGE_QUERY_PROPERTY(StorageDeviceProperty). Malformed buffers give
// an error, never a panic.
func ParseDeviceDescriptor(buf []byte) (BusInfo, error) {
	if len(buf) < descriptorMinSize {
		return BusInfo{}, fmt.Errorf("device descriptor is only %d bytes, need %d", len(buf), descriptorMinSize)
	}
	le := binary.LittleEndian
	info := BusInfo{Bus: le.Uint32(buf[28:])}
	info.Vendor = cString(buf, le.Uint32(buf[12:]))
	info.Product = cString(buf, le.Uint32(buf[16:]))
	return info, nil
}

// ParseHotplugInfo reads a STORAGE_HOTPLUG_INFO returned by
// IOCTL_STORAGE_GET_HOTPLUG_INFO and reports DeviceHotplug:
//
//	0 Size u32   4 MediaRemovable u8   5 MediaHotplug u8   6 DeviceHotplug u8   7 WriteCacheEnableOverride u8
func ParseHotplugInfo(buf []byte) (deviceHotplug bool, err error) {
	if len(buf) < 7 {
		return false, fmt.Errorf("hotplug info is only %d bytes, need 7", len(buf))
	}
	return buf[6] != 0, nil
}

// cString reads a NUL-terminated ASCII string at off inside buf ("" if off is
// 0 or outside the buffer), trimmed of padding spaces.
func cString(buf []byte, off uint32) string {
	if off == 0 || int(off) >= len(buf) {
		return ""
	}
	end := int(off)
	for end < len(buf) && buf[end] != 0 {
		end++
	}
	return strings.TrimSpace(string(buf[off:end]))
}
