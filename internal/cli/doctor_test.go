package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yashwardhanv/dd/internal/doctor"
	"github.com/yashwardhanv/dd/internal/phone"
	"github.com/yashwardhanv/dd/internal/phone/phonetest"
	"github.com/yashwardhanv/dd/internal/volume"
)

// fakeBench makes the speed test small and fast, and "the drive" a temp folder.
// It returns that folder.
func fakeBench(t *testing.T) string {
	t.Helper()
	drive := t.TempDir()
	old, oldVol := benchOptions, volumeForPath
	benchOptions = doctor.BenchOptions{Size: 1 << 20, Chunk: 256 << 10, MinFree: 1}
	volumeForPath = func(string) (volume.Volume, error) {
		return volume.Volume{Root: drive, Label: "TEST", FileSystem: "exFAT", Ready: true, DriveType: volume.DriveRemovable}, nil
	}
	t.Cleanup(func() { benchOptions, volumeForPath = old, oldVol })
	return drive
}

func TestDoctorReportsEveryArea(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	tree, _ := withFixture(t)
	plug(t, fakeDrive("E:"))
	indexAsDrive(t, tree, fakeDrive("E:"))
	fs := phonetest.NewFS()
	useFS(t, fs)
	fakeBus(t, "USB")
	oldElevated := isElevated
	isElevated = func() bool { return false } // (the CI machine's real account is an administrator)
	defer func() { isElevated = oldElevated }()

	out, err := run(t, "--no-color", "doctor")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"DriveDex", "version test", "index folder is writable", "settings file is fine",
		"Privileges", "not running as Administrator", "turbo mode",
		"Phone", "adb found", "Android Debug Bridge version 1.0.41", "Pixel 7 is connected and authorized",
		"Drive E:  SANDISK 32GB", "file system exFAT", "connected by USB", "indexed", "entries",
		"Tips", "Write caching", "Defender", "zip",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func fakeBus(t *testing.T, name string) {
	t.Helper()
	old := busTypeFor
	busTypeFor = func(string) (volume.BusInfo, error) { return volume.BusInfo{Bus: volume.BusTypeUsb}, nil }
	t.Cleanup(func() { busTypeFor = old })
	_ = name
}

func TestDoctorExplainsPhoneProblems(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	plug(t)
	fs := phonetest.NewFS()
	fs.Devices = []string{"ABCDEF0123             unauthorized usb:1-2 transport_id:2"}
	useFS(t, fs)
	out, _ := run(t, "--no-color", "doctor")
	if !strings.Contains(out, "waiting for permission") || !strings.Contains(out, `tap "Allow"`) {
		t.Errorf("an unauthorized phone needs the Allow tip:\n%s", out)
	}

	old := openADB
	openADB = func() (*phone.ADB, error) { return nil, adbNotFound(errors.New("nope")) }
	defer func() { openADB = old }()
	out, _ = run(t, "--no-color", "doctor")
	if !strings.Contains(out, "adb (Android Debug Bridge) was not found") {
		t.Errorf("missing adb:\n%s", out)
	}
}

func TestDoctorSurvivesABrokenConfigAndSaysSo(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	cfg := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("DRIVEDEX_CONFIG", cfg)
	os.WriteFile(cfg, []byte("default_limit = \n"), 0o644)
	plug(t)
	out, err := run(t, "--no-color", "doctor")
	if err != nil {
		t.Fatalf("doctor must still run when the settings are broken: %v", err)
	}
	if !strings.Contains(out, "settings file has a problem") || !strings.Contains(out, "drivedex config edit") {
		t.Errorf("should name the problem and the fix:\n%s", out)
	}
}

func TestDoctorChecksNTFSJournalOnlyWhenElevated(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	v := fakeDrive("E:")
	v.FileSystem = "NTFS"
	plug(t, v)

	oldE, oldJ := isElevated, journalStateFor
	defer func() { isElevated, journalStateFor = oldE, oldJ }()
	asked := 0
	journalStateFor = func(string) doctor.JournalState { asked++; return doctor.JournalInactive }

	isElevated = func() bool { return false }
	out, _ := run(t, "--no-color", "doctor")
	if asked != 0 || !strings.Contains(out, "change journal not checked (needs Administrator)") {
		t.Errorf("without Administrator the journal must not be opened (asked=%d):\n%s", asked, out)
	}

	isElevated = func() bool { return true }
	out, _ = run(t, "--no-color", "doctor")
	if asked != 1 || !strings.Contains(out, "journal is off") || !strings.Contains(out, "drivedex journal enable E:") {
		t.Errorf("elevated: asked=%d\n%s", asked, out)
	}
	if !strings.Contains(out, "running as Administrator") {
		t.Errorf("elevated state should be reported:\n%s", out)
	}
}

func TestDoctorSkipsNetworkAndNotReadyDrives(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	net := fakeDrive("Z:")
	net.DriveType = volume.DriveRemote
	empty := fakeDrive("F:")
	empty.Ready = false
	plug(t, net, empty)
	out, _ := run(t, "--no-color", "doctor")
	if strings.Contains(out, "Drive Z:") || strings.Contains(out, "Drive F:") || !strings.Contains(out, "no ready drives") {
		t.Errorf("network and empty drives must be left out:\n%s", out)
	}
}

func TestDoctorBenchWritesMeasuresAndCleansUp(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	plug(t)
	drive := fakeBench(t)

	out, err := runIn(t, "y\n", "--no-color", "doctor", "--bench", "E:")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Speed test", "writes one temporary 1 MB file (.drivedex-bench.tmp)", "[y/N]", "write speed", "MB/s", "the test file was deleted"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if entries, _ := os.ReadDir(drive); len(entries) != 0 {
		t.Errorf("the drive must be left exactly as it was: %v", entries)
	}
}

func TestDoctorBenchAsksFirstAndNoMeansNothingIsWritten(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	plug(t)
	drive := fakeBench(t)
	for _, answer := range []string{"\n", "n\n", ""} {
		out, err := runIn(t, answer, "--no-color", "doctor", "--bench", "E:")
		if err != nil || !strings.Contains(out, "Cancelled. Nothing was written.") || strings.Contains(out, "write speed") {
			t.Errorf("answer %q: err=%v\n%s", answer, err, out)
		}
	}
	if entries, _ := os.ReadDir(drive); len(entries) != 0 {
		t.Errorf("nothing may be written without a yes: %v", entries)
	}
	// --yes skips the question.
	out, err := runIn(t, "", "--no-color", "doctor", "--bench", "E:", "--yes")
	if err != nil || strings.Contains(out, "[y/N]") || !strings.Contains(out, "write speed") {
		t.Errorf("--yes: err=%v\n%s", err, out)
	}
}

func TestDoctorBenchRefusesWhenSpaceIsShort(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	plug(t)
	drive := fakeBench(t)
	benchOptions.MinFree = 600 << 20
	benchOptions.FreeBytes = func(string) (uint64, error) { return 50 << 20, nil }
	_, err := run(t, "doctor", "--bench", "E:", "--yes")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "not enough free space") {
		t.Errorf("err = %v", err)
	}
	if entries, _ := os.ReadDir(drive); len(entries) != 0 {
		t.Errorf("nothing may be written: %v", entries)
	}
}

func TestDoctorBenchDoesNotTouchAnExistingFile(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	plug(t)
	drive := fakeBench(t)
	existing := filepath.Join(drive, doctor.BenchFileName)
	os.WriteFile(existing, []byte("mine"), 0o644)
	_, err := run(t, "doctor", "--bench", "E:", "--yes")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Hint, "never overwrites or deletes") {
		t.Errorf("err = %v", err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "mine" {
		t.Error("a pre-existing file must be left alone")
	}
}

func TestDoctorBenchBadArguments(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	plug(t)
	if _, err := run(t, "doctor", "--bench", "banana"); err == nil {
		t.Error("a non-drive should be rejected")
	}
	old := volumeForPath
	volumeForPath = func(string) (volume.Volume, error) { return volume.Volume{Root: `E:\`, Ready: false}, nil }
	defer func() { volumeForPath = old }()
	if _, err := run(t, "doctor", "--bench", "E:"); err == nil || !strings.Contains(err.Error(), "isn't ready") && !strings.Contains(userMsg(err), "isn't ready") {
		t.Errorf("a drive that isn't ready should be rejected: %v", err)
	}
}

func userMsg(err error) string {
	var ue *UserError
	if errors.As(err, &ue) {
		return ue.Msg
	}
	return err.Error()
}

// The speed test also stops cleanly on Ctrl+C and still deletes its file.
func TestDoctorBenchCancelledLeavesNothing(t *testing.T) {
	t.Setenv("DRIVEDEX_INDEX_DIR", filepath.Join(t.TempDir(), "idx"))
	plug(t)
	drive := fakeBench(t)
	ctx, cancel := context.WithCancel(context.Background())
	benchOptions.Progress = nil
	benchOptions.FreeBytes = func(string) (uint64, error) { cancel(); return 1 << 40, nil }
	root := NewRootCmd("test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"doctor", "--bench", "E:", "--yes"})
	err := root.ExecuteContext(ctx)
	if err == nil || !strings.Contains(userMsg(err), "cancelled") {
		t.Errorf("err = %v", err)
	}
	if entries, _ := os.ReadDir(drive); len(entries) != 0 {
		t.Errorf("file left behind: %v", entries)
	}
}
