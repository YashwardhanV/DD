package preflight

import (
	"fmt"
	"math"
	"time"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// Class is the kind of storage at one end of a copy.
type Class int

const (
	ClassUnknown Class = iota
	ClassHDD           // spinning hard disk
	ClassSSD           // internal or external SSD
	ClassFlash         // pen drive or memory card
	ClassPhone         // Android phone over adb
)

func (c Class) String() string {
	switch c {
	case ClassHDD:
		return "hard disk"
	case ClassSSD:
		return "SSD"
	case ClassFlash:
		return "pen drive / card"
	case ClassPhone:
		return "phone"
	}
	return "unknown"
}

// Device describes one end of a copy for the time estimate.
type Device struct {
	Label string // "E:" or "Pixel 8", for messages
	ID    string // the source ID ("vol-1A2B3C4D", "adb-<serial>"): learned rates are kept per device
	Class Class
	// USB is how the device is connected, when it is a USB drive.
	USB    volume.USBLink
	HasUSB bool
}

// Rates supplies learned speeds (P6's run history): bytes per second for
// reading from or writing to a device, and its per-file overhead.
type Rates interface {
	Learned(id string, write bool) (bytesPerSec float64, perFile time.Duration, ok bool)
}

// Default drive speeds and per-file overheads. Starting values from typical
// hardware, to be calibrated on the owner's drives (PROGRESS.md "needs real
// hardware"); learned values from the history replace them.
const (
	mb = 1e6

	hddRate        = 100 * mb
	ssdRate        = 300 * mb
	flashReadRate  = 80 * mb
	flashWriteRate = 15 * mb
	phoneRate      = 30 * mb
	unknownRate    = 100 * mb

	perFileSSD     = 300 * time.Microsecond
	perFileHDD     = 4 * time.Millisecond
	perFileFlash   = 8 * time.Millisecond
	perFilePhone   = 15 * time.Millisecond
	perFileUnknown = 1 * time.Millisecond

	// USB protocol efficiency: the share of the raw link rate a storage
	// transfer achieves.
	usb2Efficiency = 0.70
	usb3Efficiency = 0.80
)

// Estimate is how long a copy is expected to take.
type Estimate struct {
	Seconds float64 `json:"seconds"`
	// Low and High bound the estimate when no learned rate was available
	// (0.75 to 1.5 times Seconds); equal to Seconds otherwise.
	Low     float64 `json:"low_seconds"`
	High    float64 `json:"high_seconds"`
	Learned bool    `json:"learned"`
	// FastSeconds is the estimate on a USB 3 port, when a drive runs below
	// its speed (0 = not applicable).
	FastSeconds float64 `json:"fast_seconds,omitempty"`
	// VerifySeconds is the share of Seconds spent reading copies back.
	VerifySeconds float64 `json:"verify_seconds,omitempty"`
	VerifyBytes   int64   `json:"verify_bytes,omitempty"`
}

// rates is the read rate, write rate and per-file cost of one device.
type rates struct {
	read, write float64
	perFile     time.Duration
	learned     bool
}

func deviceRates(d Device, rs Rates) rates {
	var r rates
	switch d.Class {
	case ClassHDD:
		r = rates{hddRate, hddRate, perFileHDD, false}
	case ClassSSD:
		r = rates{ssdRate, ssdRate, perFileSSD, false}
	case ClassFlash:
		r = rates{flashReadRate, flashWriteRate, perFileFlash, false}
	case ClassPhone:
		r = rates{phoneRate, phoneRate, perFilePhone, false}
	default:
		r = rates{unknownRate, unknownRate, perFileUnknown, false}
	}
	if rs != nil && d.ID != "" {
		if bps, pf, ok := rs.Learned(d.ID, false); ok && bps > 0 {
			r.read, r.learned = bps, true
			if pf > 0 {
				r.perFile = pf
			}
		}
		if bps, pf, ok := rs.Learned(d.ID, true); ok && bps > 0 {
			r.write, r.learned = bps, true
			if pf > 0 {
				r.perFile = pf
			}
		}
	}
	return r
}

// linkRate is the most the device's USB link moves per second (0 = no limit).
func linkRate(d Device, speed volume.USBSpeed) float64 {
	if !d.HasUSB {
		return 0
	}
	switch speed {
	case volume.SpeedLow:
		return 1.5e6 / 8 * usb2Efficiency
	case volume.SpeedFull:
		return 12e6 / 8 * usb2Efficiency
	case volume.SpeedHigh:
		return 480e6 / 8 * usb2Efficiency
	case volume.SpeedSuper:
		return 5e9 / 8 * usb3Efficiency
	case volume.SpeedSuperPlus:
		return 10e9 / 8 * usb3Efficiency
	}
	return 0
}

func limit(rate, link float64) float64 {
	if link > 0 && link < rate {
		return link
	}
	return rate
}

// EstimateCopy predicts the time of copying files (bytes in total) from src
// to dst:
//
//	seconds = bytes / readRate + bytes / writeRate + files * (perFile(src) + perFile(dst))
//
// where each rate is the smaller of the drive's rate and its USB link's.
// With media verification the copies are read back once more from dst.
func EstimateCopy(src, dst Device, bytes, files int64, verify copier.VerifyMode, rs Rates) Estimate {
	if bytes <= 0 && files <= 0 {
		return Estimate{}
	}
	s, d := deviceRates(src, rs), deviceRates(dst, rs)
	calc := func(srcLink, dstLink float64) (total, verifySec float64) {
		read := limit(s.read, srcLink)
		write := limit(d.write, dstLink)
		total = float64(bytes)/read + float64(bytes)/write + float64(files)*(s.perFile.Seconds()+d.perFile.Seconds())
		if verify != copier.VerifyOff {
			verifySec = float64(bytes) / limit(d.read, dstLink)
			if verify == copier.VerifyCache {
				verifySec /= 4 // mostly served from the PC's memory
			}
			total += verifySec
		}
		return total, verifySec
	}
	total, vs := calc(linkRate(src, src.USB.Speed), linkRate(dst, dst.USB.Speed))
	e := Estimate{Seconds: total, Low: total, High: total, Learned: s.learned || d.learned, VerifySeconds: vs}
	if verify != copier.VerifyOff {
		e.VerifyBytes = bytes
	}
	if !e.Learned {
		e.Low, e.High = total*0.75, total*1.5
	}
	// Would a USB 3 port help?
	if slow(src) || slow(dst) {
		fast, _ := calc(linkRate(src, best(src)), linkRate(dst, best(dst)))
		if fast < total*0.8 {
			e.FastSeconds = fast
		}
	}
	return e
}

// slow reports whether the device's USB link is below what it could reach.
func slow(d Device) bool {
	return d.HasUSB && d.USB.Speed != volume.SpeedUnknown && d.USB.Speed < best(d)
}

// best is the speed the device could reach on a good port.
func best(d Device) volume.USBSpeed {
	if d.USB.Capable != volume.SpeedUnknown {
		return d.USB.Capable
	}
	return volume.SpeedSuper
}

// Text is the estimate in plain English: "About 13 min." or "About 8 to 20
// min." (a range when no learned rate exists), plus the USB 3 comparison.
func (e Estimate) Text(slowDrive string) string {
	if e.Seconds <= 0 {
		return ""
	}
	var s string
	if e.Learned || e.High-e.Low < 2 {
		s = "About " + roughDuration(e.Seconds) + "."
	} else {
		s = "About " + roughRange(e.Low, e.High) + "."
	}
	if e.FastSeconds > 0 && slowDrive != "" {
		s += fmt.Sprintf(" %s is on a USB 2.0 link; on a USB 3 port it would take about %s.", slowDrive, roughDuration(e.FastSeconds))
	}
	return s
}

// VerifyText is the extra cost of verification: "verify on: adds a read of
// 18.2 GB from E: (about 3 min)".
func (e Estimate) VerifyText(dst string) string {
	if e.VerifyBytes <= 0 {
		return ""
	}
	return fmt.Sprintf("verify on: adds a read of %s from %s (about %s)", ui.HumanSize(e.VerifyBytes), dst, roughDuration(e.VerifySeconds))
}

// roughDuration rounds for humans: "40 s", "3 min", "1 h 20 min".
func roughDuration(sec float64) string {
	switch {
	case sec < 1:
		return "a second"
	case sec < 60:
		return fmt.Sprintf("%d s", int(math.Round(sec)))
	case sec < 3600:
		return fmt.Sprintf("%d min", int(math.Round(sec/60)))
	}
	h := int(sec / 3600)
	m := int(math.Round((sec - float64(h)*3600) / 60))
	if m == 60 {
		h, m = h+1, 0
	}
	if m == 0 {
		return fmt.Sprintf("%d h", h)
	}
	return fmt.Sprintf("%d h %d min", h, m)
}

// roughRange: "8 to 20 min", "40 s to 2 min".
func roughRange(lo, hi float64) string {
	a, b := roughDuration(lo), roughDuration(hi)
	if a == b {
		return a
	}
	// Share the unit when both are in minutes or seconds.
	if lo >= 60 && hi < 3600 {
		return fmt.Sprintf("%d to %d min", int(math.Round(lo/60)), int(math.Round(hi/60)))
	}
	if hi < 60 {
		return fmt.Sprintf("%d to %d s", int(math.Round(lo)), int(math.Round(hi)))
	}
	return a + " to " + b
}
