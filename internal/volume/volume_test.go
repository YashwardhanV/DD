package volume

import (
	"encoding/binary"
	"testing"
)

func TestSourceID(t *testing.T) {
	if got := SourceID(Volume{Serial: 0x1A2B3C4D}); got != "vol-1A2B3C4D" {
		t.Errorf("got %q", got)
	}
	if got := SourceID(Volume{Serial: 0xAB}); got != "vol-000000AB" {
		t.Errorf("serials are zero-padded to 8 digits, got %q", got)
	}
}

func TestIsDriveRoot(t *testing.T) {
	yes := []string{"E:", "e:", `E:\`, "E:/", "  d:  "}
	no := []string{`E:\Photos`, "E", `\\server\share`, "", ".", "EE:", `C:\Users\`}
	for _, s := range yes {
		if !IsDriveRoot(s) {
			t.Errorf("IsDriveRoot(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if IsDriveRoot(s) {
			t.Errorf("IsDriveRoot(%q) = true, want false", s)
		}
	}
}

func TestNormalizeLetter(t *testing.T) {
	for in, want := range map[string]string{"e": "E:", "E:": "E:", `e:\`: "E:", " f ": "F:"} {
		if got, ok := NormalizeLetter(in); !ok || got != want {
			t.Errorf("NormalizeLetter(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "ab", `E:\x`, "1"} {
		if _, ok := NormalizeLetter(in); ok {
			t.Errorf("NormalizeLetter(%q) should fail", in)
		}
	}
}

func TestVolumeHelpers(t *testing.T) {
	v := Volume{Root: `E:\`, DriveType: DriveRemovable}
	if v.Letter() != "E:" || v.TypeName() != "Removable" {
		t.Errorf("Letter=%q TypeName=%q", v.Letter(), v.TypeName())
	}
	if (Volume{DriveType: DriveFixed}).TypeName() != "Fixed" || (Volume{DriveType: DriveRemote}).TypeName() != "Network" {
		t.Error("type names wrong")
	}
}

// descriptor builds a STORAGE_DEVICE_DESCRIPTOR like Windows returns it.
func descriptor(bus uint32, vendor, product string) []byte {
	b := make([]byte, 1024)
	le := binary.LittleEndian
	le.PutUint32(b[0:], 1) // Version
	le.PutUint32(b[28:], bus)
	off := uint32(40)
	if vendor != "" {
		le.PutUint32(b[12:], off)
		off += uint32(copy(b[off:], vendor)) + 1
	}
	if product != "" {
		le.PutUint32(b[16:], off)
		off += uint32(copy(b[off:], product)) + 1
	}
	le.PutUint32(b[4:], off)
	return b[:off]
}

func TestParseDeviceDescriptor(t *testing.T) {
	info, err := ParseDeviceDescriptor(descriptor(BusTypeUsb, "SanDisk ", "Ultra USB 3.0   "))
	if err != nil || !info.IsUSB() || info.Vendor != "SanDisk" || info.Product != "Ultra USB 3.0" || info.Name() != "SanDisk Ultra USB 3.0" {
		t.Errorf("got %+v, %v", info, err)
	}
	info, _ = ParseDeviceDescriptor(descriptor(BusTypeNvme, "", ""))
	if info.IsUSB() || info.Bus != BusTypeNvme || info.Name() != "" {
		t.Errorf("NVMe without strings: %+v", info)
	}
	info, _ = ParseDeviceDescriptor(descriptor(BusTypeSata, "", "Samsung SSD 860"))
	if info.Vendor != "" || info.Name() != "Samsung SSD 860" {
		t.Errorf("product only: %+v", info)
	}
}

func TestParseDeviceDescriptorNeverPanics(t *testing.T) {
	if _, err := ParseDeviceDescriptor(make([]byte, 31)); err == nil {
		t.Error("a short buffer must be an error")
	}
	// Offsets pointing outside the buffer, or at an unterminated string, are tolerated.
	b := make([]byte, 40)
	binary.LittleEndian.PutUint32(b[12:], 9999)
	binary.LittleEndian.PutUint32(b[16:], 39)
	b[39] = 'X'
	info, err := ParseDeviceDescriptor(b)
	if err != nil || info.Vendor != "" || info.Product != "X" {
		t.Errorf("got %+v, %v", info, err)
	}
	for n := 0; n <= len(b); n++ {
		ParseDeviceDescriptor(b[:n]) // must not panic at any length
	}
}

func TestBusName(t *testing.T) {
	for b, want := range map[uint32]string{BusTypeUsb: "USB", BusTypeSata: "SATA", BusTypeNvme: "NVMe", BusTypeSd: "SD", BusTypeUnknown: "unknown", 99: "bus 99"} {
		if got := BusName(b); got != want {
			t.Errorf("BusName(%d) = %q, want %q", b, got, want)
		}
	}
}

func TestExternalDrivesAreToldApartFromInternalOnes(t *testing.T) {
	fixed := func(letter string) Volume {
		return Volume{Root: letter + `\`, DriveType: DriveFixed, Serial: 1, Ready: true}
	}
	pen := Volume{Root: `F:\`, DriveType: DriveRemovable, Serial: 2, Ready: true}
	cases := []struct {
		name string
		v    Volume
		b    BusInfo
		want bool
		conn string
	}{
		{"USB hard disk", fixed("E:"), BusInfo{Bus: BusTypeUsb}, true, "USB · external"},
		{"FireWire disk", fixed("E:"), BusInfo{Bus: BusType1394}, true, "FireWire · external"},
		{"SD card in a built-in reader", fixed("E:"), BusInfo{Bus: BusTypeSd}, true, "SD · external"},
		{"MMC card", fixed("E:"), BusInfo{Bus: BusTypeMmc}, true, "MMC · external"},
		{"Thunderbolt NVMe enclosure", fixed("E:"), BusInfo{Bus: BusTypeNvme, Hotplug: true}, true, "NVMe · external (hot-plug)"},
		{"eSATA disk", fixed("E:"), BusInfo{Bus: BusTypeSata, Hotplug: true}, true, "SATA · external (hot-plug)"},
		{"internal SATA disk", fixed("D:"), BusInfo{Bus: BusTypeSata}, false, "SATA · internal"},
		{"internal NVMe disk", fixed("D:"), BusInfo{Bus: BusTypeNvme}, false, "NVMe · internal"},
		{"internal disk behind RAID/RST", fixed("D:"), BusInfo{Bus: BusTypeRaid}, false, "RAID · internal"},
		{"pen drive whose bus is unknown", pen, BusInfo{}, true, "unknown · external"},
		{"mounted .vhdx", fixed("V:"), BusInfo{Bus: BusTypeFileBackedVirtual, Hotplug: true}, false, "Virtual · internal"},
		{"Storage Spaces", fixed("S:"), BusInfo{Bus: BusTypeSpaces}, false, "Storage Spaces · internal"},
		{"the Windows drive, even if hot-pluggable", fixed("C:"), BusInfo{Bus: BusTypeUsb, Hotplug: true}, false, "USB · internal"},
	}
	for _, c := range cases {
		if got := IsExternal(c.v, c.b, "C:"); got != c.want {
			t.Errorf("%s: IsExternal = %v, want %v", c.name, got, c.want)
		}
		if got := Connection(c.v, c.b, "c:"); got != c.conn {
			t.Errorf("%s: Connection = %q, want %q", c.name, got, c.conn)
		}
	}
	// No system drive known (not Windows): nothing is excluded for being the system drive.
	if !IsExternal(fixed("C:"), BusInfo{Bus: BusTypeUsb}, "") {
		t.Error("an empty system drive must not exclude C:")
	}
}

func TestParseHotplugInfo(t *testing.T) {
	if hp, err := ParseHotplugInfo([]byte{8, 0, 0, 0, 0, 0, 1, 0}); err != nil || !hp {
		t.Errorf("DeviceHotplug=1: %v, %v", hp, err)
	}
	if hp, err := ParseHotplugInfo([]byte{8, 0, 0, 0, 1, 1, 0, 0}); err != nil || hp {
		t.Errorf("removable media is not a hot-pluggable device: %v, %v", hp, err)
	}
	for n := 0; n < 7; n++ {
		if _, err := ParseHotplugInfo(make([]byte, n)); err == nil {
			t.Errorf("%d bytes must be an error", n)
		}
	}
}
