package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/ui"
	"github.com/yashwardhanv/dd/internal/volume"
)

// Variables so tests can plug in a pretend USB drive.
var (
	usbLinkFor  = volume.USBLinkFor
	usbPortsFor = volume.USBPorts
)

// portSummary asks Windows about the PC's USB sockets.
func portSummary() volume.PortSummary {
	ports, err := usbPortsFor()
	if err != nil {
		return volume.PortSummary{}
	}
	return volume.Summarize(ports)
}

// usbLinkOf returns how the drive holding path is connected over USB.
// ok is false for anything that isn't a USB drive, or when Windows won't say.
func usbLinkOf(path string) (volume.USBLink, bool) {
	vol := driveOf(path)
	if vol == "" {
		return volume.USBLink{}, false
	}
	bus, err := busTypeFor(vol + `\`)
	if err != nil || !bus.IsUSB() {
		return volume.USBLink{}, false
	}
	l, err := usbLinkFor(vol + `\`)
	if err != nil || l.Speed == volume.SpeedUnknown {
		return volume.USBLink{}, false
	}
	return l, true
}

// driveOf returns the drive letter a Windows path is on ("E:"), or "".
// (Not filepath.VolumeName: that only understands drive letters on Windows,
// and the tests run everywhere.)
func driveOf(path string) string {
	if len(path) < 2 || path[1] != ':' {
		return ""
	}
	if l, ok := volume.NormalizeLetter(path[:2]); ok {
		return l
	}
	return ""
}

// warnSlowUSB prints one warning per drive in roots that runs slower than it
// could, with how long the link alone makes the copy take. It returns whether
// any of the roots is a USB drive (copies then use BigLanes).
func warnSlowUSB(out io.Writer, roots []string, writeBytes int64) (anyUSB bool) {
	seen := map[string]bool{}
	var sum *volume.PortSummary // asked only if a drive is slow
	for _, r := range roots {
		vol := driveOf(r)
		if vol == "" || seen[vol] {
			continue
		}
		seen[vol] = true
		l, ok := usbLinkOf(r)
		if !ok {
			continue
		}
		anyUSB = true
		if !l.Judge().Slow() {
			continue
		}
		if sum == nil {
			s := portSummary()
			sum = &s
		}
		text, tip := volume.Advice(l, *sum)
		fmt.Fprintln(out, ui.Yellow("⚠ "+vol+" "+text))
		if mb := l.Speed.MaxMBps(); mb > 0 && writeBytes > 0 {
			floor := time.Duration(float64(writeBytes) / (mb * 1e6) * float64(time.Second))
			if floor >= 10*time.Second {
				fmt.Fprintln(out, ui.Dim(fmt.Sprintf("  At that speed this copy takes at least %s.", ui.FormatDuration(floor))))
			}
		}
		if tip != "" {
			fmt.Fprintln(out, ui.Dim("  → "+tip))
		}
	}
	return anyUSB
}

// ---- drivedex ports ---------------------------------------------------------------------

func newPortsCmd() *cobra.Command {
	var watch, all bool
	cmd := &cobra.Command{
		Use:   "ports",
		Short: "Find the fast USB ports on this PC and check each USB drive's speed",
		Long: `A USB 3 pen drive in a USB 2.0 port runs at USB 2.0 speed: about 40 MB/s at
most, instead of hundreds. No software can make a USB 2.0 port faster (it
doesn't have USB 3's extra wires), but DriveDex can tell you which is which.

This command shows how many of this PC's ports can do USB 3 (and which are
USB-C), and for every USB drive plugged in: the speed it runs at, the speed it
could run at, and what is in the way (the port, a hub, or the cable/adapter).

With --watch it keeps running: move your drive from port to port and it tells
you the speed of each one as you plug in. Press Ctrl+C to stop.`,
		Example: `  drivedex ports
  drivedex ports --watch
  drivedex ports --all`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPorts(cmd.Context(), cmd.OutOrStdout(), watch, all)
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "keep watching: report each drive's speed as you plug it into different ports")
	cmd.Flags().BoolVar(&all, "all", false, "also list every port of the PC's own USB controllers")
	return cmd
}

func runPorts(ctx context.Context, out io.Writer, watch, all bool) error {
	ports, err := usbPortsFor()
	if errors.Is(err, errors.ErrUnsupported) {
		return userErr("checking USB ports only works on Windows", "run DriveDex on your Windows PC", err)
	}
	if err != nil {
		return userErr("couldn't ask Windows about the USB ports", "try again; if it keeps failing run with --debug", err)
	}
	sum := volume.Summarize(ports)
	printPortSummary(out, sum)
	if all {
		printPortTable(out, ports)
	}

	fmt.Fprintln(out)
	vols, err := listVolumes()
	if err != nil {
		return userErr("couldn't list the drives", "try again; if it keeps failing run with --debug", err)
	}
	shown := 0
	for _, v := range vols {
		if !v.Ready {
			continue
		}
		if l, ok := usbLinkOf(v.Root); ok {
			printDriveLink(out, v, l, sum)
			shown++
		}
	}
	if shown == 0 && !watch {
		fmt.Fprintln(out, "No USB drive is plugged in. Plug one in and run this again (or use --watch).")
	}
	if !watch {
		return nil
	}
	return watchPorts(ctx, out, vols, sum)
}

func printPortSummary(out io.Writer, s volume.PortSummary) {
	if !s.Known {
		fmt.Fprintln(out, "Windows didn't report any USB ports.")
		return
	}
	switch {
	case s.USB3 == 0:
		fmt.Fprintln(out, ui.Bold("This PC has no USB 3 ports")+" that Windows knows of: every port tops out at USB 2.0 (about 40 MB/s).")
	default:
		line := fmt.Sprintf("This PC has %d USB 3 %s", s.USB3, plural(s.USB3, "port", "ports"))
		if s.USB3TypeC > 0 {
			line += fmt.Sprintf(" (%d USB-C)", s.USB3TypeC)
		}
		fmt.Fprintln(out, ui.Bold(line+"."))
	}
	if s.USB2OnlyKnown && s.USB2Only > 0 {
		fmt.Fprintf(out, "It also has %d USB 2.0-only %s: fine for a mouse, slow for a drive.\n", s.USB2Only, plural(s.USB2Only, "port", "ports"))
	}
	fmt.Fprintln(out, ui.Dim(`Windows usually can't say WHERE each port is on the case: use --watch and move your drive around to find the fast ones.`))
}

func printPortTable(out io.Writer, ports []volume.PortInfo) {
	sort.SliceStable(ports, func(i, j int) bool {
		if ports[i].Hub != ports[j].Hub {
			return ports[i].Hub < ports[j].Hub
		}
		return ports[i].Number < ports[j].Number
	})
	fmt.Fprintln(out)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HUB\tPORT\tSPEAKS\tSOCKET\tIN USE")
	hubs := map[string]int{}
	for _, p := range ports {
		if !p.Root {
			continue
		}
		if _, ok := hubs[p.Hub]; !ok {
			hubs[p.Hub] = len(hubs) + 1
		}
		speaks := "USB 2.0"
		if p.USB3 {
			speaks = "USB 3"
		}
		socket := "internal"
		if p.UserConnectable {
			socket = "USB-A"
			if p.TypeC {
				socket = "USB-C"
			}
		}
		if p.CompanionPort != 0 && p.CompanionHub == "" {
			socket += fmt.Sprintf(" (with port %d)", p.CompanionPort)
		}
		inUse := "-"
		if p.Connected {
			inUse = p.Speed.String()
		}
		fmt.Fprintf(tw, "#%d\t%d\t%s\t%s\t%s\n", hubs[p.Hub], p.Number, speaks, socket, inUse)
	}
	tw.Flush()
	fmt.Fprintln(out, ui.Dim("A USB 3 socket shows up as two ports: its USB 3 half and its USB 2.0 half (\"with port N\")."))
}

func printDriveLink(out io.Writer, v volume.Volume, l volume.USBLink, sum volume.PortSummary) {
	name := v.Letter()
	if v.Label != "" {
		name += " " + v.Label
	}
	text, tip := volume.Advice(l, sum)
	verdict := l.Judge()
	switch {
	case verdict.Slow():
		fmt.Fprintln(out, ui.Yellow("⚠ "+name+": "+text))
	case verdict == volume.VerdictFull:
		fmt.Fprintln(out, ui.Green("✓ ")+name+": "+text)
	default:
		fmt.Fprintln(out, "• "+name+": "+text)
	}
	if tip != "" {
		fmt.Fprintln(out, ui.Dim("    → "+tip))
	}
}

// watchPorts reports every USB drive that is plugged in from now on, until
// ctx ends (Ctrl+C).
func watchPorts(ctx context.Context, out io.Writer, initial []volume.Volume, sum volume.PortSummary) error {
	fmt.Fprintln(out, "\nWatching. Plug your drive into a port to see its speed there; Ctrl+C to stop.")
	known := map[string]bool{}
	for _, v := range initial {
		if v.Ready {
			known[v.Root] = true
		}
	}
	tick := time.NewTicker(watchInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "Stopped.")
			return nil
		case <-tick.C:
		}
		vols, err := listVolumes()
		if err != nil {
			continue
		}
		now := map[string]bool{}
		for _, v := range vols {
			if !v.Ready {
				continue
			}
			now[v.Root] = true
			if known[v.Root] {
				continue
			}
			fmt.Fprint(out, ui.Dim(time.Now().Format("15:04:05")+" "))
			if l, ok := usbLinkOf(v.Root); ok {
				printDriveLink(out, v, l, sum)
			} else {
				fmt.Fprintf(out, "%s %s: not a USB drive, or Windows didn't say its speed\n", v.Letter(), v.Label)
			}
		}
		for root := range known {
			if !now[root] {
				fmt.Fprintf(out, "%s%s unplugged\n", ui.Dim(time.Now().Format("15:04:05")+" "), strings.TrimRight(root, `\`))
			}
		}
		known = now
	}
}

// watchInterval is how often --watch looks for drives (a variable for tests).
var watchInterval = time.Second
