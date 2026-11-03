package trace

import (
	"fmt"
	"strings"
)

// MaxHints is how many hints a report shows (the most important first).
const MaxHints = 3

// Hint rule thresholds (section "Spec - tracer" of BUILD-V2.pdf).
const (
	linkDataShare     = 0.70      // Data phase share for "the link is the limit"
	linkUseShare      = 0.80      // ... while moving at least this share of the link's speed
	overheadShare     = 0.40      // create+close+rename+set-time share for "creating files is slow"
	overheadAvgFile   = 256 << 10 // ... when the average file is smaller than this
	listingShare      = 0.25      // listing share of the elapsed time
	phoneSmallFile    = 1 << 20   // phone files smaller than this pay mostly round trips
	phoneMinFiles     = 100       // too few files to say anything
	phoneLinkShare    = 0.50      // a phone copy moving less than this share of its link is waiting
	phoneSlowNoLink   = 10        // MB/s: with an unknown link, slower than this counts as waiting
	seekShare         = 0.20      // HDD source below this share of its sequential speed is seeking
	defaultHDDSeqMBps = 100       // a USB hard disk's typical sequential speed, when it wasn't measured
)

// Hints turns a report into at most MaxHints plain-English tips, most
// important first. Each rule is a pure function of the report so it can be
// unit-tested without copying anything.
func Hints(rep Report) []string {
	var out []string
	add := func(s string) {
		if s != "" && len(out) < MaxHints {
			out = append(out, s)
		}
	}
	add(hintLinkBound(rep))
	add(hintPerFileOverhead(rep))
	add(hintListing(rep))
	add(hintPhoneRoundTrips(rep))
	add(hintSeekBound(rep))
	if out == nil {
		out = []string{}
	}
	return out
}

// hintLinkBound: the bytes are flowing as fast as the USB link allows.
func hintLinkBound(rep Report) string {
	link := rep.Context.LinkMBps
	if link <= 0 || rep.Share(Data) <= linkDataShare || rep.MBps < linkUseShare*link {
		return ""
	}
	port := ""
	if link <= 60 {
		port = " (USB 2.0 port?)"
	}
	return fmt.Sprintf("The USB link is the limit%s: %.0f MB/s of about %.0f MB/s possible - software can't go faster; try a USB 3 port.", port, rep.MBps, link)
}

// hintPerFileOverhead: making each new file costs more than its data.
func hintPerFileOverhead(rep Report) string {
	over := rep.Share(DstCreate) + rep.Share(Close) + rep.Share(Rename) + rep.Share(SetTime)
	if rep.Files == 0 || over <= overheadShare || rep.AvgFileBytes() >= overheadAvgFile {
		return ""
	}
	where := "the destination folder"
	if d := strings.TrimSpace(rep.Context.Destination); d != "" {
		where = d
	}
	return fmt.Sprintf("Creating files is the slow part (avg file %s). Antivirus scanning each new file is a common cause; you may add %s to Windows Security exclusions yourself (run 'drivedex doctor' for the steps). DriveDex never changes security settings.",
		sizeShort(rep.AvgFileBytes()), where)
}

// hintListing: reading the source's file list took a big part of the copy.
func hintListing(rep Report) string {
	list := rep.Time(PlanList) + rep.Time(PhoneList)
	if rep.Elapsed <= 0 || float64(list) <= listingShare*float64(rep.Elapsed) {
		return ""
	}
	if strings.HasPrefix(rep.Context.PlanSource, "phone") {
		return fmt.Sprintf("Listing the phone took %s; with a fresh phone index DriveDex v2 lists only what changed (index the phone first, or keep the agent running).", fmtDur(list))
	}
	return fmt.Sprintf("Listing took %s; with a fresh index DriveDex v2 can skip this (index the drive first, or keep the agent running).", fmtDur(list))
}

// hintPhoneRoundTrips: a phone copy of many small files, pulled one by one.
func hintPhoneRoundTrips(rep Report) string {
	if rep.Context.PhoneMode != "pull" || rep.Files < phoneMinFiles || rep.AvgFileBytes() >= phoneSmallFile {
		return ""
	}
	link := rep.Context.LinkMBps
	waiting := (link > 0 && rep.MBps < phoneLinkShare*link) || (link <= 0 && rep.MBps < phoneSlowNoLink)
	if !waiting {
		return ""
	}
	return "The phone copy is waiting between files (adb pull asks for each file separately) - tar mode helps: --phone-mode tar."
}

// hintSeekBound: a hard disk source moving far below its sequential speed.
func hintSeekBound(rep Report) string {
	if !strings.HasSuffix(rep.Context.SrcKind, "hdd") || rep.Files == 0 {
		return ""
	}
	seq := rep.Context.DriveBenchMBps
	if seq <= 0 {
		seq = defaultHDDSeqMBps
		if l := rep.Context.LinkMBps; l > 0 && l < seq {
			seq = l
		}
	}
	if rep.MBps >= seekShare*seq {
		return ""
	}
	return fmt.Sprintf("A hard disk is seeking (%.1f MB/s of about %.0f MB/s it can read in one go): fewer workers or disk-order reads help (--workers 1, --order disk).", rep.MBps, seq)
}

func sizeShort(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%d KB", b>>10)
	}
	return fmt.Sprintf("%d B", b)
}
