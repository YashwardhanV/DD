package volume

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Why this file exists
//
// A "USB 3" pen drive in a USB 2.0 port runs at USB 2.0 speed: about 40 MB/s at
// best, instead of hundreds. No software can change that - a USB 2.0 port
// simply doesn't have the extra wires USB 3 uses. What software CAN do is
// notice it and say so, which Explorer never does: "this drive could run
// about 10x faster; this port is USB 2.0 only; this PC has two USB 3 ports".
//
// Windows knows all of it. The USB hub driver can say, for each port, which
// USB versions the port supports and at what speed the device in it is
// running, and whether that device could go faster (it is how Windows decides
// to show "This device can perform faster if connected to a USB 3.0 port").
// The code below reads those answers (usblink_windows.go asks; this file
// decodes them, so it can be tested anywhere).

// USBSpeed is a USB link speed.
type USBSpeed int

const (
	SpeedUnknown   USBSpeed = iota
	SpeedLow                // USB 1.0, 1.5 Mbit/s (keyboards, mice)
	SpeedFull               // USB 1.1, 12 Mbit/s
	SpeedHigh               // USB 2.0, 480 Mbit/s
	SpeedSuper              // USB 3.2 Gen 1 (also sold as "USB 3.0" / "USB 3.1 Gen 1"), 5 Gbit/s
	SpeedSuperPlus          // USB 3.2 Gen 2 or faster, 10 Gbit/s and up
)

// String is a short name for tables: "USB 2.0 · 480 Mbps".
func (s USBSpeed) String() string {
	switch s {
	case SpeedLow:
		return "USB 1.0 · 1.5 Mbps"
	case SpeedFull:
		return "USB 1.1 · 12 Mbps"
	case SpeedHigh:
		return "USB 2.0 · 480 Mbps"
	case SpeedSuper:
		return "USB 3 · 5 Gbps"
	case SpeedSuperPlus:
		return "USB 3 · 10+ Gbps"
	}
	return "unknown"
}

// MaxMBps is roughly the most a USB storage device can move over a link of
// this speed in practice (after protocol overhead), in MB per second. 0 = unknown.
func (s USBSpeed) MaxMBps() float64 {
	switch s {
	case SpeedLow:
		return 0.1
	case SpeedFull:
		return 1
	case SpeedHigh:
		return 40
	case SpeedSuper:
		return 450
	case SpeedSuperPlus:
		return 1000
	}
	return 0
}

// USBLink is how one USB drive is connected right now.
type USBLink struct {
	// Speed is the speed the drive is running at.
	Speed USBSpeed
	// Capable is the fastest the drive itself supports. SpeedUnknown when
	// Windows can't tell (no USB 3 controller in the PC, or Windows 7).
	Capable USBSpeed
	// PortUSB3 says whether the port the drive is in can do USB 3.
	// PortKnown is false when Windows couldn't tell.
	PortUSB3, PortKnown bool
	// PortTwinUSB3 is true when the drive sits on the USB 2.0 half of a port
	// whose USB 3 half exists: the port could do USB 3, but the connection
	// fell back to USB 2.0 (a USB 2.0 cable, extension or adapter, or a plug
	// that isn't pushed fully in).
	PortTwinUSB3 bool
	// ViaHub is true when the drive is plugged into an external hub, dock or
	// adapter rather than straight into the PC.
	ViaHub bool
	// Port is the port number on its hub (for --debug output).
	Port int
}

// Verdict is the plain-English judgement of a USBLink.
type Verdict int

const (
	VerdictUnknown   Verdict = iota // couldn't tell
	VerdictFull                     // running as fast as the drive can
	VerdictSlowPort                 // the PC port is USB 2.0 only
	VerdictSlowHub                  // a USB 2.0 hub/dock/adapter in between is the limit
	VerdictSlowCable                // the port can do USB 3 but the connection fell back
	VerdictSlowMaybe                // running at USB 2.0; Windows can't say whether the drive could go faster
)

// Judge decides what, if anything, is holding the drive back.
func (l USBLink) Judge() Verdict {
	switch {
	case l.Speed == SpeedUnknown:
		return VerdictUnknown
	case l.Capable != SpeedUnknown && l.Speed >= l.Capable:
		return VerdictFull
	case l.Capable == SpeedUnknown:
		if l.Speed >= SpeedSuper {
			return VerdictFull // already on USB 3: nothing to gain from another port
		}
		return VerdictSlowMaybe
	}
	// The drive could go faster than it does. Why?
	switch {
	case l.PortKnown && (l.PortUSB3 || l.PortTwinUSB3):
		// The port speaks USB 3 but the drive isn't using it: something
		// between the two is USB 2.0 only, or the contacts aren't all touching.
		if l.ViaHub {
			return VerdictSlowHub
		}
		return VerdictSlowCable
	case l.ViaHub:
		return VerdictSlowHub
	case l.PortKnown:
		return VerdictSlowPort
	}
	return VerdictSlowMaybe
}

// Slow reports whether the drive is running slower than it could.
func (v Verdict) Slow() bool {
	return v == VerdictSlowPort || v == VerdictSlowHub || v == VerdictSlowCable
}

// Advice explains a link in one line and gives a one-line fix ("" when there
// is nothing to do). ports, when Known, lets the fix say whether this PC has
// a faster port to move to.
func Advice(l USBLink, ports PortSummary) (text, tip string) {
	v := l.Judge()
	now := l.Speed.String()
	switch v {
	case VerdictUnknown:
		return "USB speed unknown (Windows didn't say)", ""
	case VerdictFull:
		return "running at " + now + " - as fast as this drive can", ""
	case VerdictSlowMaybe:
		return "running at " + now + " (about 40 MB/s at most); Windows can't tell whether this drive could go faster",
			"if it is a USB 3 drive, " + movePortTip(ports)
	}
	could := fmt.Sprintf("can run at %s but is running at %s", l.Capable, now)
	gain := ""
	if a, b := l.Speed.MaxMBps(), l.Capable.MaxMBps(); a > 0 && b > 0 {
		gain = fmt.Sprintf(" - the link alone caps it at about %.0f MB/s instead of about %.0f", a, b)
	}
	switch v {
	case VerdictSlowPort:
		return could + gain + ". This port is USB 2.0 only.", movePortTip(ports)
	case VerdictSlowHub:
		return could + gain + ". It is plugged into a hub, dock or adapter that runs at USB 2.0.",
			"plug the drive straight into the PC, or use a hub/adapter marked USB 3 (5 Gbps); " + movePortTip(ports)
	default: // VerdictSlowCable
		return could + gain + ". The port supports USB 3, so the connection itself fell back to USB 2.0.",
			"push the plug fully in, and drop any extension cable or adapter that isn't USB 3; if it stays slow, try another port"
	}
}

// movePortTip says where a faster port is, as far as Windows knows.
func movePortTip(p PortSummary) string {
	if !p.Known {
		return `move it to a USB 3 port (often blue inside, or marked "SS", or a USB-C port with an adapter)`
	}
	if p.USB3 == 0 {
		return "this PC reports no USB 3 ports, so ~40 MB/s is the ceiling here; no software can raise it (a USB 3 PCIe card or a newer PC can)"
	}
	s := fmt.Sprintf("this PC has %d USB 3 %s", p.USB3, plural(p.USB3, "port", "ports"))
	if p.USB3TypeC > 0 {
		if p.USB3TypeC == p.USB3 {
			s += " - all USB-C, so use a USB-C to USB-A adapter (OTG) marked USB 3"
		} else {
			s += fmt.Sprintf(" (%d of them USB-C: a USB-C to USB-A adapter marked USB 3 works)", p.USB3TypeC)
		}
	}
	return s + `; run "drivedex ports --watch" and move the drive between ports to find them`
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---- the PC's ports ---------------------------------------------------------------------

// PortInfo is one port of a USB hub, as Windows reports it.
type PortInfo struct {
	Hub             string // the hub's device path
	Number          int    // port number on that hub, from 1
	Root            bool   // a port of the PC itself (a root hub), not of an external hub
	UserConnectable bool   // a socket you can plug into (not wired to the webcam inside the lid)
	TypeC           bool
	USB2, USB3      bool   // USB versions the port speaks
	CompanionPort   int    // the other half of the same socket (USB 2.0 <-> USB 3), 0 = none
	CompanionHub    string // the hub that half is on; "" = this one
	Connected       bool
	Speed           USBSpeed // of the device in it, when Connected
}

// PortSummary counts the sockets of this PC by what they can do.
//
// A USB 3 socket shows up in Windows as TWO ports - a USB 3 one and its USB 2.0
// "companion" (the old four wires are still there for old devices) - so
// sockets are counted by their USB 3 half.
type PortSummary struct {
	Known     bool
	USB3      int // sockets that can do USB 3
	USB3TypeC int // ... of which USB-C
	// USB2Only counts sockets with no USB 3 half. Only filled in when Windows
	// reported companion ports (otherwise every USB 3 socket's USB 2.0 half
	// would wrongly look like an extra USB 2.0-only socket).
	USB2Only      int
	USB2OnlyKnown bool
}

// Summarize counts the PC's own user-connectable sockets.
func Summarize(ports []PortInfo) PortSummary {
	s := PortSummary{Known: len(ports) > 0}
	type key struct {
		hub  string
		port int
	}
	usb3 := map[key]bool{}
	sawCompanion := false
	for _, p := range ports {
		if p.CompanionPort != 0 {
			sawCompanion = true
		}
		if !p.Root || !p.UserConnectable || !p.USB3 {
			continue
		}
		usb3[key{p.Hub, p.Number}] = true
		s.USB3++
		if p.TypeC {
			s.USB3TypeC++
		}
	}
	if !sawCompanion {
		return s
	}
	s.USB2OnlyKnown = true
	for _, p := range ports {
		if !p.Root || !p.UserConnectable || p.USB3 || !p.USB2 {
			continue
		}
		hub := p.CompanionHub
		if hub == "" {
			hub = p.Hub
		}
		if p.CompanionPort != 0 && usb3[key{hub, p.CompanionPort}] {
			continue // the USB 2.0 half of a USB 3 socket
		}
		s.USB2Only++
	}
	return s
}

// ---- decoding the hub driver's answers -----------------------------------------------------
//
// These structures come from Windows' usbioctl.h, which packs them to 1 byte
// (no padding). All values are little-endian.

// USB_CONNECTION_STATUS DeviceConnected.
const usbDeviceConnected = 1

// ConnectionInfoEx is the part of USB_NODE_CONNECTION_INFORMATION_EX DriveDex uses.
type ConnectionInfoEx struct {
	Speed     USBSpeed
	Connected bool
	IsHub     bool
	VendorID  uint16
	ProductID uint16
	BcdUSB    uint16 // the USB version the device's descriptor claims, e.g. 0x0320
}

// ParseConnectionInfoEx decodes IOCTL_USB_GET_NODE_CONNECTION_INFORMATION_EX:
//
//	 0 ConnectionIndex u32
//	 4 USB_DEVICE_DESCRIPTOR (18 bytes): 6 bcdUSB u16, 12 idVendor u16, 14 idProduct u16
//	22 CurrentConfigurationValue u8   23 Speed u8   24 DeviceIsHub u8
//	25 DeviceAddress u16   27 NumberOfOpenPipes u32   31 ConnectionStatus u32
func ParseConnectionInfoEx(buf []byte) (ConnectionInfoEx, error) {
	if len(buf) < 35 {
		return ConnectionInfoEx{}, fmt.Errorf("connection info is only %d bytes, need 35", len(buf))
	}
	le := binary.LittleEndian
	c := ConnectionInfoEx{
		BcdUSB:    le.Uint16(buf[6:]),
		VendorID:  le.Uint16(buf[12:]),
		ProductID: le.Uint16(buf[14:]),
		IsHub:     buf[24] != 0,
		Connected: le.Uint32(buf[31:]) == usbDeviceConnected,
	}
	// USB_DEVICE_SPEED: UsbLowSpeed 0, UsbFullSpeed 1, UsbHighSpeed 2, UsbSuperSpeed 3.
	switch buf[23] {
	case 0:
		c.Speed = SpeedLow
	case 1:
		c.Speed = SpeedFull
	case 2:
		c.Speed = SpeedHigh
	case 3:
		c.Speed = SpeedSuper
	}
	return c, nil
}

// ConnectionInfoV2 is USB_NODE_CONNECTION_INFORMATION_EX_V2 (Windows 8 and later).
type ConnectionInfoV2 struct {
	PortUSB1, PortUSB2, PortUSB3 bool // what the PORT supports
	OperatingSuper               bool // the device runs at 5 Gbps or more
	CapableSuper                 bool // the device could
	OperatingSuperPlus           bool // the device runs at 10 Gbps or more
	CapableSuperPlus             bool // the device could
}

// V2 request sizes and the protocols DriveDex says it understands.
const (
	connectionInfoV2Size = 16
	protocolsAll         = 0x7 // Usb110 | Usb200 | Usb300
)

// ConnectionInfoV2Request builds the input of
// IOCTL_USB_GET_NODE_CONNECTION_INFORMATION_EX_V2 for a port:
//
//	0 ConnectionIndex u32   4 Length u32   8 SupportedUsbProtocols u32   12 Flags u32
func ConnectionInfoV2Request(port int) []byte {
	b := make([]byte, connectionInfoV2Size)
	le := binary.LittleEndian
	le.PutUint32(b[0:], uint32(port))
	le.PutUint32(b[4:], connectionInfoV2Size)
	le.PutUint32(b[8:], protocolsAll)
	return b
}

// ParseConnectionInfoV2 decodes the answer. On output SupportedUsbProtocols
// holds the protocols the port supports (bit 0 USB 1.1, bit 1 USB 2.0, bit 2
// USB 3), and Flags bit 0 = operating at SuperSpeed or higher, bit 1 =
// SuperSpeed-capable or higher, bit 2 = operating at SuperSpeedPlus or
// higher, bit 3 = SuperSpeedPlus-capable or higher.
func ParseConnectionInfoV2(buf []byte) (ConnectionInfoV2, error) {
	if len(buf) < connectionInfoV2Size {
		return ConnectionInfoV2{}, fmt.Errorf("connection info v2 is only %d bytes, need %d", len(buf), connectionInfoV2Size)
	}
	le := binary.LittleEndian
	p, f := le.Uint32(buf[8:]), le.Uint32(buf[12:])
	return ConnectionInfoV2{
		PortUSB1: p&1 != 0, PortUSB2: p&2 != 0, PortUSB3: p&4 != 0,
		OperatingSuper: f&1 != 0, CapableSuper: f&2 != 0,
		OperatingSuperPlus: f&4 != 0, CapableSuperPlus: f&8 != 0,
	}, nil
}

// Speeds works out the running and the best possible speed of the device on
// a port from the two answers. v2 is nil when Windows doesn't support it.
//
// The V2 flags win where they exist: the older answer reports a 10 Gbps
// device as plain "SuperSpeed", and only V2 knows what the device could do.
func Speeds(ex ConnectionInfoEx, v2 *ConnectionInfoV2) (now, capable USBSpeed) {
	now = ex.Speed
	if v2 == nil {
		return now, SpeedUnknown
	}
	switch {
	case v2.OperatingSuperPlus:
		now = SpeedSuperPlus
	case v2.OperatingSuper && now < SpeedSuper:
		// The older answer can report a SuperSpeed device as "high speed"
		// (for old programs that don't know SuperSpeed exists).
		now = SpeedSuper
	}
	switch {
	case v2.CapableSuperPlus:
		capable = SpeedSuperPlus
	case v2.CapableSuper:
		capable = SpeedSuper
	case now != SpeedUnknown:
		// Not SuperSpeed-capable: a USB 2.0 device. What it runs at is its
		// best, unless that is below USB 2.0 and its descriptor claims 2.0.
		capable = now
		if now < SpeedHigh && ex.BcdUSB >= 0x0200 {
			capable = SpeedHigh
		}
	}
	return now, capable
}

// PortConnector is USB_PORT_CONNECTOR_PROPERTIES.
type PortConnector struct {
	UserConnectable  bool
	TypeC            bool
	CompanionIndex   int
	CompanionPort    int
	CompanionHubLink string // "" = the companion is on the same hub (or there is none)
}

// PortConnectorRequestSize is the buffer used for
// IOCTL_USB_GET_PORT_CONNECTOR_PROPERTIES (room for the companion hub's name).
const PortConnectorRequestSize = 1024

// PortConnectorRequest builds its input:
//
//	0 ConnectionIndex u32   4 ActualLength u32   8 UsbPortProperties u32
//	12 CompanionIndex u16   14 CompanionPortNumber u16   16 CompanionHubSymbolicLinkName UTF-16, NUL-terminated
func PortConnectorRequest(port int) []byte {
	b := make([]byte, PortConnectorRequestSize)
	binary.LittleEndian.PutUint32(b[0:], uint32(port))
	return b
}

// ParsePortConnector decodes the answer. UsbPortProperties: bit 0
// PortIsUserConnectable, bit 1 PortIsDebugCapable, bit 2
// PortHasMultipleCompanions, bit 3 PortConnectorIsTypeC.
func ParsePortConnector(buf []byte) (PortConnector, error) {
	if len(buf) < 16 {
		return PortConnector{}, fmt.Errorf("port properties are only %d bytes, need 16", len(buf))
	}
	le := binary.LittleEndian
	props := le.Uint32(buf[8:])
	pc := PortConnector{
		UserConnectable: props&1 != 0,
		TypeC:           props&8 != 0,
		CompanionIndex:  int(le.Uint16(buf[12:])),
		CompanionPort:   int(le.Uint16(buf[14:])),
	}
	end := len(buf)
	if n := int(le.Uint32(buf[4:])); n >= 16 && n < end {
		end = n
	}
	pc.CompanionHubLink = utf16z(buf[16:end])
	return pc, nil
}

// HubPortCount decodes IOCTL_USB_GET_HUB_INFORMATION_EX (offset 4:
// HighestPortNumber u16) - the right answer for USB 3 hubs, whose ports are
// numbered across both halves.
func HubPortCount(buf []byte) (int, error) {
	if len(buf) < 6 {
		return 0, fmt.Errorf("hub information is only %d bytes, need 6", len(buf))
	}
	return int(binary.LittleEndian.Uint16(buf[4:])), nil
}

// HubPortCountLegacy decodes IOCTL_USB_GET_NODE_INFORMATION (offset 6:
// bNumberOfPorts of the hub descriptor), for systems without the newer call.
func HubPortCountLegacy(buf []byte) (int, error) {
	if len(buf) < 7 {
		return 0, fmt.Errorf("node information is only %d bytes, need 7", len(buf))
	}
	return int(buf[6]), nil
}

// HubDevicePath turns the symbolic link name the hub driver reports
// ("USB#ROOT_HUB30#...#{...}" or "\??\USB#...") into a path CreateFile accepts.
func HubDevicePath(link string) string {
	link = strings.TrimSpace(link)
	switch {
	case link == "":
		return ""
	case strings.HasPrefix(link, `\??\`):
		return `\\?\` + link[4:]
	case strings.HasPrefix(link, `\\`):
		return link
	}
	return `\\?\` + link
}

// IsRootHub reports whether a device instance ID belongs to a PC's own hub
// ("USB\ROOT_HUB30\4&..."), as opposed to an external hub or dock.
func IsRootHub(instanceID string) bool {
	return strings.HasPrefix(strings.ToUpper(instanceID), `USB\ROOT_HUB`)
}

func utf16z(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}
