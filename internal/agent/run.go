package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/yashwardhanv/dd/internal/config"
	"github.com/yashwardhanv/dd/internal/index"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/portable"
	"github.com/yashwardhanv/dd/internal/volume"
)

// commandFor builds the process that re-indexes a target, and findDrivedex
// locates the CLI to run. Variables so tests can run a stand-in program.
var (
	commandFor   = exec.CommandContext
	findDrivedex = func() (string, error) {
		if exe, err := SiblingExe("drivedex"); err == nil {
			return exe, nil
		}
		if p, err := exec.LookPath("drivedex"); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("can't find drivedex.exe next to the agent or on PATH")
	}
)

// Main runs the agent until ctx ends and returns the process exit code. It is
// the whole body of drivedex-agent.exe.
func Main(ctx context.Context, version string) int {
	dir, err := DataDir()
	if err != nil {
		return 1
	}
	release, already, err := AcquireInstance()
	if err != nil {
		return 1
	}
	if already {
		return 0 // another agent is running: exit silently, as specified
	}
	defer release()

	log, err := NewLogger(LogPath(dir))
	if err != nil {
		return 1
	}
	defer log.Close()
	log.Infof("drivedex-agent %s starting (pid file %s)", version, PIDPath(dir))

	if err := WritePID(dir); err != nil {
		log.Errorf("couldn't write the PID file: %v", err)
	}
	defer RemovePID(dir)

	volume.InitProcess() // no "There is no disk in the drive" pop-ups for empty card readers

	deps, err := RealDeps(log)
	if err != nil {
		log.Errorf("couldn't start: %v", err)
		return 1
	}
	set := DefaultSettings()
	if path, err := config.Path(); err == nil {
		if cfg, err := config.Load(path); err != nil {
			log.Errorf("%v - using the default settings", err)
		} else {
			set = SettingsFrom(cfg)
			deps.Devices = (&adbFinder{path: cfg.AdbPath}).devices
		}
	}
	New(deps, set, log).Run(ctx)
	return 0
}

// SettingsFrom takes the agent's options from the config file. Everything the
// file doesn't cover (polling intervals ...) keeps its default.
func SettingsFrom(cfg config.Config) Settings {
	s := DefaultSettings()
	s.Notifications = cfg.Notifications
	s.AutoIndex = append([]string(nil), cfg.AutoIndex...)
	if d, err := cfg.StaleDuration(); err == nil {
		s.StaleAfter = d
	}
	return s
}

// RealDeps wires the agent to the real system.
func RealDeps(log *Logger) (Deps, error) {
	store, err := index.OpenDefault()
	if err != nil {
		return Deps{}, err
	}
	finder := &adbFinder{}
	return Deps{
		Mask:        driveMask(),
		Volumes:     volume.ListLocal,
		Bus:         volume.BusType,
		Devices:     finder.devices,
		Portables:   portable.List,
		SystemDrive: os.Getenv("SystemDrive"),
		Index:       ChildIndexer(store),
		Locked:      func(t Target) bool { return IsLocked(store.LockPath(t.ID)) },
		Header: func(id string) (index.Header, bool) {
			h, err := index.ReadHeaderFile(store.Path(id))
			return h, err == nil
		},
		Notify:  Notify,
		USBLink: volume.USBLinkFor,
	}, nil
}

// adbFinder looks for adb lazily, so installing Platform-Tools while the agent
// is running is noticed without restarting it.
type adbFinder struct {
	path string // the adb_path setting ("" = look for it)
	adb  *phone.ADB
}

func (f *adbFinder) devices(ctx context.Context) ([]phone.Device, error) {
	if f.adb == nil {
		path, err := phone.FindADB(f.path)
		if err != nil {
			return nil, nil // no adb installed: nothing to watch, and nothing to complain about
		}
		f.adb = phone.NewADB(path)
	}
	return f.adb.Devices(ctx)
}

// ChildIndexer returns an indexing function that runs `drivedex index <arg>`
// (the CLI installed next to the agent) in a hidden window.
//
// Using the real command rather than duplicating its logic means the agent
// gets everything the CLI does for free: NTFS turbo mode when elevated,
// incremental refresh from the change journal, the shared lock, and so on.
func ChildIndexer(store *index.Store) func(ctx context.Context, t Target) (Outcome, error) {
	return func(ctx context.Context, t Target) (Outcome, error) {
		exe, err := findDrivedex()
		if err != nil {
			return Outcome{}, err
		}
		out, err := RunHidden(commandFor(ctx, exe, "index", t.Arg))
		if err != nil {
			return Outcome{}, fmt.Errorf("`drivedex index %s` failed: %w (%s)", t.Arg, err, lastLineOf(string(out)))
		}
		// The saved index tells us how many entries it holds - no need to parse the command's output.
		h, err := index.ReadHeaderFile(store.Path(t.ID))
		if err != nil {
			return Outcome{}, fmt.Errorf("the index for %s wasn't saved: %w", t.Display, err)
		}
		return Outcome{Entries: h.EntryCount}, nil
	}
}

// lastLineOf returns the last non-empty line of s.
func lastLineOf(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
