package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// EnvDataDir overrides where the agent keeps its log and PID file (tests).
const EnvDataDir = "DRIVEDEX_DATA_DIR"

// DataDir is %LOCALAPPDATA%\DriveDex on Windows: the agent's log and PID file
// live here, next to the "indexes" folder.
func DataDir() (string, error) {
	if d := os.Getenv(EnvDataDir); d != "" {
		return d, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("finding the cache folder: %w", err)
	}
	return filepath.Join(cache, "DriveDex"), nil
}

// LogPath is where the agent writes its log.
func LogPath(dir string) string { return filepath.Join(dir, "agent.log") }

// PIDPath is where the running agent records its process ID.
func PIDPath(dir string) string { return filepath.Join(dir, "agent.pid") }

// PIDInfo is what the PID file holds.
type PIDInfo struct {
	PID     int
	Started time.Time
}

// WritePID records this process as the running agent.
func WritePID(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf("%d\n%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	return os.WriteFile(PIDPath(dir), []byte(body), 0o644)
}

// ErrNoPID means there is no PID file (the agent isn't running).
var ErrNoPID = errors.New("no agent PID file")

// ReadPID reads the PID file.
func ReadPID(dir string) (PIDInfo, error) {
	data, err := os.ReadFile(PIDPath(dir))
	if os.IsNotExist(err) {
		return PIDInfo{}, ErrNoPID
	}
	if err != nil {
		return PIDInfo{}, err
	}
	lines := strings.Fields(string(data))
	if len(lines) == 0 {
		return PIDInfo{}, fmt.Errorf("the PID file %s is empty", PIDPath(dir))
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid <= 0 {
		return PIDInfo{}, fmt.Errorf("the PID file %s doesn't contain a process ID", PIDPath(dir))
	}
	info := PIDInfo{PID: pid}
	if len(lines) > 1 {
		if t, err := time.Parse(time.RFC3339, lines[1]); err == nil {
			info.Started = t
		}
	}
	return info, nil
}

// RemovePID deletes the PID file.
func RemovePID(dir string) { os.Remove(PIDPath(dir)) }

// Running reports whether an agent is running, according to the PID file and
// the operating system. A leftover PID file from a crash counts as not running.
func Running(dir string) (PIDInfo, bool) {
	info, err := ReadPID(dir)
	if err != nil {
		return PIDInfo{}, false
	}
	return info, ProcessRunning(info.PID)
}

// SiblingExe returns the path of a program installed next to the running one
// (for example drivedex-agent.exe next to drivedex.exe).
func SiblingExe(name string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	p := filepath.Join(filepath.Dir(self), name+exeSuffix)
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		return "", fmt.Errorf("%s was not found next to %s", name+exeSuffix, filepath.Base(self))
	}
	return p, nil
}
