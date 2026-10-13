package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yashwardhanv/dd/internal/copier"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/ui"
)

// copyConfirmThreshold: copying more files than this asks first.
const copyConfirmThreshold = 100

// copyGroup is the files of one source (one drive, folder or phone) that a
// search matched.
type copyGroup struct {
	index  *index.Index
	label  string
	serial string // phones
	jobs   []copier.Job
	bytes  int64
}

// invalidNameChars can't appear in a Windows file name.
var invalidNameChars = strings.NewReplacer("<", "_", ">", "_", ":", "_", `"`, "_", "/", "_", `\`, "_", "|", "_", "?", "_", "*", "_")

// sourceDirName is the folder a source's files are copied into, so matches from
// different drives (which may share file names) never overwrite each other.
func sourceDirName(h *index.Header) string {
	for _, name := range []string{h.Label, h.SourceID} {
		if name = strings.Trim(invalidNameChars.Replace(strings.TrimSpace(name)), ". "); name != "" {
			return name
		}
	}
	return "source"
}

// copyMatches implements `search --copy-to`: copy every matching FILE (not
// just the displayed ones) into a folder, one sub-folder per source, keeping
// each file's folder structure.
func copyMatches(ctx context.Context, in io.Reader, out *bufio.Writer, indexes []*index.Index, res index.SearchResult, env index.Env, f searchFlags) error {
	dst, err := resolveDestination(f.copyTo)
	if err != nil {
		return err
	}

	// Two drives can have the same label (many pen drives are just "USB
	// DRIVE"). Their files must not land in one folder and overwrite each
	// other, so a shared name gets the drive's id added - for every index
	// with that name, so the folder doesn't depend on which drive matched first.
	nameCount := map[string]int{}
	for _, ix := range indexes {
		nameCount[strings.ToLower(sourceDirName(&ix.Header))]++
	}
	dirName := func(h *index.Header) string {
		name := sourceDirName(h)
		if nameCount[strings.ToLower(name)] > 1 {
			name += " (" + strings.Trim(invalidNameChars.Replace(h.SourceID), ". ") + ")"
		}
		return name
	}

	groups := map[int]*copyGroup{} // by index position
	var folders, offline, unreadable, onDevice int
	offlineNames, deviceNames := map[string]bool{}, map[string]bool{}

	forEachHit(indexes, res, env, func(ix *index.Index, h index.Hit, rel string) {
		hd := &ix.Header
		if ix.Flags[h.Slot]&index.FlagDir != 0 {
			folders++
			return
		}
		g := groups[h.Index]
		if g == nil {
			g = &copyGroup{index: ix, label: dirName(hd)}
			if hd.SourceKind == index.KindPhone {
				g.serial = strings.TrimPrefix(hd.SourceID, "adb-")
			}
			groups[h.Index] = g
		}
		relOS := strings.ReplaceAll(rel, `\`, string(filepath.Separator))
		job := copier.Job{Rel: filepath.Join(g.label, relOS), Size: ix.Sizes[h.Slot], ModTime: ix.MTimes[h.Slot]}

		switch hd.SourceKind {
		case index.KindDevice:
			// Read through Windows, not a file path: DriveDex finds these but doesn't copy them.
			onDevice++
			deviceNames[hd.Label] = true
			return
		case index.KindPhone:
			if !env.Online(hd) {
				offline++
				offlineNames[hd.Label] = true
				return
			}
			job.Src = path.Join(strings.TrimRight(hd.RootPath, "/")+"/", strings.ReplaceAll(rel, `\`, "/"))
		default:
			local, ok := index.LocalPath(hd, rel, env)
			if !ok {
				offline++
				offlineNames[sourceDirName(hd)] = true
				return
			}
			job.Src = local
			if ix.Flags[h.Slot]&index.FlagNoMeta != 0 || job.Size < 0 { // turbo-mode entry: ask the drive
				st, err := os.Lstat(local)
				if err != nil {
					unreadable++
					return
				}
				job.Size, job.ModTime = st.Size(), st.ModTime().Unix()
			}
		}
		if job.Size < 0 {
			job.Size = 0
		}
		g.jobs = append(g.jobs, job)
		g.bytes += job.Size
	})

	var order []int
	var files, bytes int64
	for i, g := range groups {
		if len(g.jobs) > 0 {
			order = append(order, i)
			files += int64(len(g.jobs))
			bytes += g.bytes
		}
	}
	sort.Ints(order)

	// Tell the user about everything that will NOT be copied.
	if folders > 0 {
		fmt.Fprintln(out, ui.Dim(fmt.Sprintf("%d matching %s skipped: only files are copied.", folders, plural(folders, "folder was", "folders were"))))
	}
	if offline > 0 {
		var names []string
		for n := range offlineNames {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintln(out, ui.Yellow("note:")+fmt.Sprintf(" %d matching %s on drives that aren't connected (%s) and %s skipped.",
			offline, plural(offline, "file is", "files are"), strings.Join(names, ", "), plural(offline, "was", "were")))
	}
	if onDevice > 0 {
		var names []string
		for n := range deviceNames {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintln(out, ui.Yellow("note:")+fmt.Sprintf(" %d matching %s on %s and %s skipped: copy %s in Explorer (This PC → %s).",
			onDevice, plural(onDevice, "file is", "files are"), strings.Join(names, ", "), plural(onDevice, "was", "were"),
			plural(onDevice, "it", "them"), names[0]))
	}
	if unreadable > 0 {
		fmt.Fprintln(out, ui.Yellow("note:")+fmt.Sprintf(" %d %s couldn't be read from the drive and %s skipped.", unreadable, plural(unreadable, "file", " files"), plural(unreadable, "was", "were")))
	}
	if files == 0 {
		fmt.Fprintln(out, "Nothing to copy: none of the matches is a file on a connected drive or phone.")
		return nil
	}

	if files > copyConfirmThreshold && !f.yes {
		q := fmt.Sprintf("Copy %s files (%s) into %s?", ui.Thousands(files), ui.HumanSize(bytes), dst)
		if !askYesNo(out, in, q) {
			fmt.Fprintln(out, "Cancelled. Nothing was copied.")
			return nil
		}
	}

	status := ui.NewStatusLine(os.Stderr)
	n, _, _ := parseWorkersCfg()
	dstPenalty, dstKnown := seekPenaltyFor(dst)
	var failed int64
	for _, i := range order {
		g := groups[i]
		workers, why := n, "set in config.toml / --workers"
		if n == 0 {
			workers = copier.AutoWorkers(false, false, dstPenalty, dstKnown)
			why = autoWhy(workers)
		}

		var plan *copier.Plan
		if g.serial != "" {
			adb, err := openADB()
			if err != nil {
				return err
			}
			plan = copier.PlanPhoneJobs(adb, g.serial, g.jobs, dst, copier.OnConflictNewer, max(workers, 4))
		} else {
			plan = copier.PlanJobs(dst, g.jobs, copier.OnConflictNewer, max(workers, 4))
		}
		fmt.Fprintf(out, "\n%s\n", ui.Bold(g.label))
		if err := finishPlan(ctx, out, status, plan, planRun{workers: workers, why: why, dstArg: f.copyTo}); err != nil {
			failed++
			fmt.Fprintln(out, ui.Red("  "+err.Error()))
		}
	}
	out.Flush()
	if failed > 0 {
		return userErr(fmt.Sprintf("%d of the %d sources had problems copying", failed, len(order)),
			"see the messages above; run the same command again to retry", nil)
	}
	return nil
}

// parseWorkersCfg reads the workers setting.
func parseWorkersCfg() (int, bool, error) { return parseWorkers(string(activeConfig.Workers)) }
