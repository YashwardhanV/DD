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
	BusTypeUsb     uint32 = 7
	BusTypeRaid    uint32 = 8
	BusTypeSata    uint32 = 11
	BusTypeSd      uint32 = 12
	BusTypeMmc     uint32 = 13
	BusTypeNvme    uint32 = 17
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
	case 14, 15:
		return "Virtual"
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
}

// IsUSB reports whether the drive is attached over USB.
func (b BusInfo) IsUSB() bool { return b.Bus == BusTypeUsb }

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
