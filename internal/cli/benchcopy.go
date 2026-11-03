package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/benchcopy"
	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/scan"
	"github.com/yashwardhanv/dd/internal/ui"
)

// Variables so tests can run the benchmark without real programs.
var (
	benchRunner benchcopy.Runner = benchcopy.ExecRunner{}
	benchExe                     = os.Executable
	benchNow                     = time.Now
)

// newBenchCmd is `drivedex bench`, the parent of `bench copy`.
func newBenchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Measure copy speed fairly: DriveDex against Explorer, robocopy and itself",
		Long: `Benchmarks that produce honest, repeatable numbers on YOUR drives.

'bench copy' times several copy programs on the same folder: Explorer's own
copy engine, robocopy (single-threaded and /MT:16), DriveDex with every 2.0
feature off (drivedex-v1) and DriveDex as it is. The order of the programs
rotates every round and the median is reported, so a warm cache doesn't
favour anyone.`,
		Example: `  drivedex bench copy E:\Photos D:\BenchTmp --rounds 3 --cold --markdown
  drivedex bench copy phone:/DCIM/Camera D:\BenchTmp`,
	}
	cmd.AddCommand(newBenchCopyCmd())
	return cmd
}

type benchFlags struct {
	rounds   int
	engines  string
	cold     bool
	markdown bool
	yes      bool
	set      string
}

func newBenchCopyCmd() *cobra.Command {
	var f benchFlags
	cmd := &cobra.Command{
		Use:   "copy <src> <dst-folder>",
		Short: "Time Explorer, robocopy, DriveDex v1 and DriveDex v2 copying the same folder",
		Long: `Copies <src> into a new folder <dst-folder>\.drivedex-bench-<date-time> once per
engine per round, times each copy, checks every file arrived, and deletes the
copy again. At the end the benchmark folder itself is deleted. Nothing outside
that folder is ever written or deleted.

Engines (default: all that apply here):
  explorer      Windows' own copy engine (Shell.Application CopyHere)  [experimental]
  robocopy-1    robocopy, one thread
  robocopy-mt   robocopy /MT:16
  drivedex-v1   drivedex copy --v1  (same program, every 2.0 feature off)
  drivedex      drivedex copy       (defaults)
For a phone source (phone:/DCIM/Camera): adb-pull (one 'adb pull -a' of the
folder), drivedex-v1, drivedex.

--cold asks you to unplug and replug the source before every run: the only
honest way to clear Windows' cache for a USB drive without Administrator
rights. Without it the results are labelled "warm cache".`,
		Example: `  drivedex bench copy E:\SomeFolder D:\BenchTmp --rounds 3 --cold --markdown
  drivedex bench copy phone:/DCIM/Camera D:\BenchTmp --rounds 3
  drivedex bench copy E:\Photos D:\BenchTmp --engines drivedex-v1,drivedex`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBenchCopy(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), args[0], args[1], f)
		},
	}
	cmd.Flags().IntVar(&f.rounds, "rounds", 3, "how many times each engine copies the folder")
	cmd.Flags().StringVar(&f.engines, "engines", "", "comma-separated engines to run (default: all that apply)")
	cmd.Flags().BoolVar(&f.cold, "cold", false, "ask to unplug and replug the source before every run (honest cold-cache numbers)")
	cmd.Flags().BoolVar(&f.markdown, "markdown", false, "also print a row ready to paste into docs/BENCHMARKS.md")
	cmd.Flags().BoolVar(&f.yes, "yes", false, "don't ask before writing to the destination")
	cmd.Flags().StringVar(&f.set, "set", "", `test set name for the Markdown row, e.g. "A" (5,000+ files under 1 MB)`)
	return cmd
}

// benchSource is the resolved source of a benchmark.
type benchSource struct {
	arg      string // as typed
	local    string // local folder ("" for a phone)
	remote   string // phone folder ("" for a local source)
	serial   string
	adbPath  string
	size     benchcopy.Size
	describe string
}

func runBenchCopy(ctx context.Context, in io.Reader, stdout io.Writer, srcArg, dstArg string, f benchFlags) error {
	out := bufio.NewWriter(stdout)
	defer out.Flush()
	if f.rounds < 1 || f.rounds > 50 {
		return userErr(fmt.Sprintf("--rounds %d isn't valid", f.rounds), "use a number from 1 to 50 (3 is a good choice)", nil)
	}
	dst, err := resolveDestination(dstArg)
	if err != nil {
		return err
	}
	src, err := resolveBenchSource(ctx, srcArg)
	if err != nil {
		return err
	}
	if src.local != "" && insidePath(src.local, dst) {
		return userErr("the benchmark folder can't be inside the folder being copied", "choose a destination outside "+src.local, nil)
	}
	if src.size.Files == 0 {
		return userErr("the source has no files to copy", "choose a folder with files in it", nil)
	}

	exe, _ := benchExe()
	engines, err := benchEngines(src, exe, f.engines)
	if err != nil {
		return err
	}

	// Room for one copy at a time (each is deleted before the next).
	if free, err := copyChecks().FreeBytes(nearestDir(dst)); err == nil && uint64(src.size.Bytes) > free {
		return userErr("the destination doesn't have enough free space for one copy of the source",
			fmt.Sprintf("the source holds %s; free up space or choose another destination", ui.HumanSize(src.size.Bytes)), nil)
	}

	fmt.Fprintf(out, "Source: %s - %s files, %s (%s)\n", srcArg, ui.Thousands(src.size.Files), ui.HumanSize(src.size.Bytes), src.describe)
	fmt.Fprintf(out, "Destination: %s (%s)\n", dst, describePath(dst))
	names := make([]string, len(engines))
	for i, e := range engines {
		names[i] = e.Name()
	}
	fmt.Fprintf(out, "Engines: %s · %d rounds, order rotates every round\n", strings.Join(names, ", "), f.rounds)
	if !f.yes {
		out.Flush()
		q := fmt.Sprintf("This copies the source into a new folder %s\\%s<date-time> %d times and deletes each copy after timing it. Continue?",
			dst, benchcopy.RootPrefix, f.rounds*len(engines))
		if !askYesNo(stdout, in, q) {
			fmt.Fprintln(out, "Cancelled. Nothing was written.")
			return nil
		}
	}

	// Explorer gets 2x the slowest other engine as its time limit.
	var mu sync.Mutex
	var slowest time.Duration
	for _, e := range engines {
		if x, ok := e.(*benchcopy.Explorer); ok {
			x.Timeout = func() time.Duration {
				mu.Lock()
				defer mu.Unlock()
				return 2 * slowest
			}
		}
	}
	reader := sharedReader(in)
	cfg := benchcopy.Config{
		Src: benchSrcArg(src), Dst: dst, Engines: engines, Rounds: f.rounds, Expected: src.size, Now: benchNow,
		Log: func(s string) { fmt.Fprintln(out, ui.Dim("  "+s)); out.Flush() },
		OnRun: func(r benchcopy.Run) {
			mu.Lock()
			if r.Engine != "explorer" && r.Err == nil && r.Elapsed > slowest {
				slowest = r.Elapsed
			}
			mu.Unlock()
		},
	}
	if f.cold {
		cfg.BeforeRun = func(engine string, round int) error {
			fmt.Fprintf(out, "Round %d, %s: unplug the source drive, plug it back in, press Enter… ", round, engine)
			out.Flush()
			if _, err := reader.ReadString('\n'); err != nil {
				return errors.New("no answer (stdin closed); stopping")
			}
			return nil
		}
	}
	results, _, err := benchcopy.Bench(ctx, cfg)
	if err != nil && !errors.Is(err, context.Canceled) {
		return userErr("the benchmark stopped", "check the destination can be written to and try again", err)
	}
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(out, "Stopped. The benchmark folder was deleted.")
		return nil
	}

	fmt.Fprintln(out)
	benchcopy.WriteTable(out, results, src.size)
	if v := benchcopy.Verdict(results); v != "" {
		fmt.Fprintln(out, v)
	}
	if !f.cold {
		fmt.Fprintln(out, ui.Dim("Warm cache: Windows may have answered reads from RAM. Use --cold for honest drive numbers."))
	}
	if f.markdown {
		fmt.Fprintln(out)
		benchcopy.WriteMarkdown(out, benchcopy.Describe{
			Date: benchNow(), TestSet: f.set, Source: src.describe, Dest: describePath(dst), Cold: f.cold, Rounds: f.rounds,
		}, results, src.size)
	}
	return nil
}

// resolveBenchSource works out what is being copied and how big it is.
func resolveBenchSource(ctx context.Context, arg string) (benchSource, error) {
	spec, isPhone, err := parsePhoneSource(arg)
	if err != nil {
		return benchSource{}, err
	}
	if !isPhone {
		p, err := resolveSource(arg)
		if err != nil {
			return benchSource{}, err
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return benchSource{}, userErr("bench copy needs a folder, not a single file", "give the folder that holds the files", nil)
		}
		size, err := benchcopy.Measure(p)
		if err != nil {
			return benchSource{}, userErr("couldn't read the whole source folder", "check the drive is connected", err)
		}
		return benchSource{arg: arg, local: p, size: size, describe: describePath(p)}, nil
	}
	adb, err := openADB()
	if err != nil {
		return benchSource{}, err
	}
	devs, err := adb.Devices(ctx)
	if err != nil {
		return benchSource{}, userErr("couldn't ask adb for the connected phones", "run 'adb kill-server' and try again", err)
	}
	dev, err := phone.PickDevice(devs, spec.serial)
	if err != nil {
		return benchSource{}, phoneSelectionError(err)
	}
	remote := copier.PhonePathRoot(spec.path, activePhoneRoot())
	b := index.NewBuilder(index.Header{})
	if err := phone.NewScanner(adb, dev.Serial, remote).Scan(ctx, b, func(scan.Progress) {}); err != nil {
		return benchSource{}, planError(arg, err)
	}
	var size benchcopy.Size
	ix := b.Index()
	for slot := 0; slot < ix.Len(); slot++ {
		if ix.Flags[slot]&index.FlagDir == 0 {
			size.Files++
			size.Bytes += max(ix.Sizes[slot], 0)
		}
	}
	adbPath, _ := phone.FindADB(activeAdbPath())
	desc := dev.Name() + " (Android, adb)"
	return benchSource{arg: arg, remote: remote, serial: dev.Serial, adbPath: adbPath, size: size, describe: desc}, nil
}

// benchSrcArg is what each engine is given as the source.
func benchSrcArg(s benchSource) string {
	if s.local != "" {
		return s.local
	}
	return s.remote
}

// benchEngines builds the engine list (all that apply, or the --engines list).
func benchEngines(src benchSource, exe, list string) ([]benchcopy.Engine, error) {
	var all []benchcopy.Engine
	if src.local != "" {
		all = []benchcopy.Engine{
			benchcopy.NewExplorer(benchRunner, src.size, nil),
			benchcopy.Robocopy{Run: benchRunner},
			benchcopy.Robocopy{Run: benchRunner, Threads: 16},
			benchcopy.DriveDex{Run: benchRunner, Exe: exe, V1: true},
			benchcopy.DriveDex{Run: benchRunner, Exe: exe},
		}
	} else {
		// The drivedex engines get the phone path in phone:<serial>:<path> form.
		all = []benchcopy.Engine{
			benchcopy.ADBPull{Run: benchRunner, ADB: src.adbPath, Serial: src.serial},
			phoneEngine{benchcopy.DriveDex{Run: benchRunner, Exe: exe, V1: true}, src.serial},
			phoneEngine{benchcopy.DriveDex{Run: benchRunner, Exe: exe}, src.serial},
		}
	}
	if strings.TrimSpace(list) == "" {
		return all, nil
	}
	var out []benchcopy.Engine
	for _, name := range strings.Split(list, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		found := false
		for _, e := range all {
			if e.Name() == name {
				out = append(out, e)
				found = true
			}
		}
		if !found {
			valid := make([]string, len(all))
			for i, e := range all {
				valid[i] = e.Name()
			}
			return nil, userErr(fmt.Sprintf("unknown engine %q", name), "choose from: "+strings.Join(valid, ", "), nil)
		}
	}
	if len(out) == 0 {
		return nil, userErr("--engines is empty", "name at least one engine, e.g. --engines drivedex-v1,drivedex", nil)
	}
	return out, nil
}

// phoneEngine runs a drivedex engine with the source written as
// phone:<serial>:<absolute phone path>.
type phoneEngine struct {
	benchcopy.DriveDex
	serial string
}

func (p phoneEngine) Copy(ctx context.Context, src, dst string) error {
	return p.DriveDex.Copy(ctx, "phone:"+p.serial+":"+src, dst)
}

// describePath says what drive a path is on: "SANDISK 64GB (FAT32, USB 3 · 5 Gbps)".
func describePath(p string) string {
	letter := driveOf(p)
	if letter == "" {
		return "local folder"
	}
	parts := []string{}
	name := letter
	if v, err := volumeForPath(letter + `\`); err == nil {
		if v.Label != "" {
			name += " " + v.Label
		}
		if v.FileSystem != "" {
			parts = append(parts, v.FileSystem)
		}
	}
	switch k := driveKind(p); k {
	case "usb-hdd", "hdd":
		parts = append(parts, "hard disk")
	case "usb-flash", "ssd":
		parts = append(parts, "flash/SSD")
	}
	if l, ok := usbLinkOf(p); ok {
		parts = append(parts, l.Speed.String())
	} else if bus, err := busTypeFor(letter + `\`); err == nil && bus.Bus != 0 {
		parts = append(parts, bus.Name())
	}
	if len(parts) == 0 {
		return name
	}
	return name + " (" + strings.Join(parts, ", ") + ")"
}

// insidePath reports whether p is dir or inside it.
func insidePath(dir, p string) bool {
	_, ok := index.RelativeTo(dir, p)
	return ok
}

// nearestDir returns p or its closest existing parent.
func nearestDir(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := strings.TrimRight(p, `\/`)
		i := strings.LastIndexAny(parent, `\/`)
		if i <= 0 {
			return p
		}
		p = parent[:i]
	}
}
