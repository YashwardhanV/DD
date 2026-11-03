package cli

import (
	"bufio"
	"fmt"
	"time"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/copier/trace"
	"github.com/yashwardhanv/dd/internal/ui"
)

// reportTrace prints (--trace) and saves (--trace-file) the trace report of a
// finished copy. The latest report is also kept in DriveDex's own folder, so
// 'drivedex doctor' can explain its hints later.
func reportTrace(out *bufio.Writer, plan *copier.Plan, o planRun, workers int) {
	if o.trace == nil {
		return
	}
	elapsed := time.Since(o.began)
	rep := o.trace.Report(elapsed, traceContext(plan, o, workers))
	if o.showTrace {
		fmt.Fprintln(out)
		rep.WriteText(out)
	}
	if o.traceFile != "" {
		if err := rep.WriteJSONFile(o.traceFile); err != nil {
			fmt.Fprintln(out, ui.Yellow("note:")+" couldn't save the trace: "+err.Error())
		} else {
			fmt.Fprintln(out, ui.Dim("Trace saved to "+o.traceFile))
		}
	}
	if store, err := openStore(); err == nil {
		rep.WriteJSONFile(trace.LastReportPath(store.Dir)) // best effort
	}
}

// traceContext describes the copy for the hint rules.
func traceContext(plan *copier.Plan, o planRun, workers int) trace.Context {
	c := trace.Context{
		Workers: workers, PlanSource: plan.Source, Destination: o.dstArg,
		SrcKind: "unknown", DstKind: driveKind(plan.DstRoot),
	}
	switch {
	case plan.Phone != nil:
		c.SrcKind = "phone"
		c.PhoneMode = "pull"
	case o.srcRoot != "":
		c.SrcKind = driveKind(o.srcRoot)
	}
	c.LinkMBps = linkMBps(o.srcRoot, plan.DstRoot)
	return c
}

// driveKind is "usb-hdd", "usb-flash", "hdd", "ssd" or "unknown".
func driveKind(path string) string {
	if driveOf(path) == "" {
		return "unknown"
	}
	penalty, known := seekPenaltyFor(path)
	usb := false
	if bus, err := busTypeFor(driveOf(path) + `\`); err == nil {
		usb = bus.IsUSB()
	}
	switch {
	case !known:
		if usb {
			return "usb-unknown"
		}
		return "unknown"
	case usb && penalty:
		return "usb-hdd"
	case usb:
		return "usb-flash"
	case penalty:
		return "hdd"
	}
	return "ssd"
}

// linkMBps is the slowest USB link among the given paths (0 = none known).
func linkMBps(paths ...string) float64 {
	best := 0.0
	for _, p := range paths {
		if p == "" {
			continue
		}
		if l, ok := usbLinkOf(p); ok {
			if mb := l.Speed.MaxMBps(); mb > 0 && (best == 0 || mb < best) {
				best = mb
			}
		}
	}
	return best
}
