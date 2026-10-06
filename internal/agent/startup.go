package agent

import (
	"errors"
	"fmt"
	"strings"
)

// Names used for starting the agent at log-in.
const (
	// RunValueName is the value under HKCU\Software\Microsoft\Windows\CurrentVersion\Run.
	RunValueName = "DriveDexAgent"
	// TaskName is the scheduled task used for the elevated (administrator) start-up.
	TaskName = "DriveDex Agent"
	// RunKeyPath is the registry key, under HKEY_CURRENT_USER, that Windows reads at log-in.
	RunKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
)

// Registry is the tiny part of the Windows registry we use (values under the
// per-user Run key). Tests use an in-memory fake.
type Registry interface {
	Get(name string) (value string, exists bool, err error)
	Set(name, value string) error
	// Delete removes a value; removed is false if it wasn't there.
	Delete(name string) (removed bool, err error)
}

// Tasks is the part of the Windows Task Scheduler we use.
type Tasks interface {
	// Create makes (or replaces) a task that runs command at log-in with the highest privileges.
	Create(name, command string) error
	Exists(name string) (bool, error)
	Delete(name string) (removed bool, err error)
}

// RunCommand is the registry value: the agent's path in quotes (so a path with
// spaces works).
func RunCommand(exe string) string { return `"` + exe + `"` }

// StartupStatus says how (and whether) the agent starts at log-in.
type StartupStatus struct {
	RunValue string // the registry value, "" if not set
	HasRun   bool
	HasTask  bool
}

// Enabled reports whether the agent starts at log-in any way.
func (s StartupStatus) Enabled() bool { return s.HasRun || s.HasTask }

// Elevated reports whether the start-up task (admin rights) is the one in use.
func (s StartupStatus) Elevated() bool { return s.HasTask }

// Status reads the current start-up configuration.
func Status(reg Registry, tasks Tasks) (StartupStatus, error) {
	var st StartupStatus
	v, ok, err := reg.Get(RunValueName)
	if err != nil {
		return st, fmt.Errorf("reading the Run registry key: %w", err)
	}
	st.RunValue, st.HasRun = v, ok
	if tasks != nil {
		if st.HasTask, err = tasks.Exists(TaskName); err != nil {
			return st, fmt.Errorf("checking the scheduled task: %w", err)
		}
	}
	return st, nil
}

// ErrNeedAdminForTask is returned when the elevated start-up task can't be created.
var ErrNeedAdminForTask = errors.New("creating the scheduled task needs an Administrator terminal")

// Enable makes the agent start at log-in - in exactly ONE way, so two agents
// never fight over the drives:
//
//   - normal: a value in the per-user Run key (no administrator rights needed);
//   - elevated: a scheduled task that runs with highest privileges (needs an
//     Administrator terminal once). The agent then runs as administrator, which
//     enables NTFS turbo mode in the background.
func Enable(reg Registry, tasks Tasks, exe string, elevated bool) error {
	if elevated {
		if err := tasks.Create(TaskName, RunCommand(exe)); err != nil {
			return err
		}
		// The task replaces the Run value (if both existed the plain one could win the race).
		if _, err := reg.Delete(RunValueName); err != nil {
			return fmt.Errorf("removing the old Run entry: %w", err)
		}
		return nil
	}
	if err := reg.Set(RunValueName, RunCommand(exe)); err != nil {
		return fmt.Errorf("writing the Run registry value: %w", err)
	}
	if _, err := tasks.Delete(TaskName); err != nil {
		// Removing an old elevated task needs admin rights; if that fails, say so.
		if exists, _ := tasks.Exists(TaskName); exists {
			return fmt.Errorf("the Run entry was written, but the old elevated task couldn't be removed (%w)", err)
		}
	}
	return nil
}

// Disable removes both start-up methods.
func Disable(reg Registry, tasks Tasks) (removedRun, removedTask bool, err error) {
	removedRun, err = reg.Delete(RunValueName)
	if err != nil {
		return removedRun, false, fmt.Errorf("removing the Run registry value: %w", err)
	}
	if tasks != nil {
		if exists, _ := tasks.Exists(TaskName); exists {
			removedTask, err = tasks.Delete(TaskName)
			if err != nil {
				return removedRun, false, fmt.Errorf("removing the scheduled task: %w", err)
			}
		}
	}
	return removedRun, removedTask, nil
}

// Describe is a one-line description of the start-up state, for messages.
func (s StartupStatus) Describe() string {
	switch {
	case s.HasTask:
		return "starts at log-in with administrator rights (scheduled task)"
	case s.HasRun:
		return "starts at log-in (Run entry: " + strings.TrimSpace(s.RunValue) + ")"
	}
	return "does not start automatically"
}
