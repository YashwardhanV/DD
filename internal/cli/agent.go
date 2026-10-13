package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yashwardhanv/dd/internal/agent"
	"github.com/yashwardhanv/dd/internal/ui"
)

// Hooks for the operating-system parts of the agent commands (replaced in tests).
var (
	agentDataDir       = agent.DataDir
	agentStart         = agent.StartDetached
	agentKill          = agent.KillProcess
	processRunning     = agent.ProcessRunning
	agentExe           = func() (string, error) { return agent.SiblingExe("drivedex-agent") }
	newStartupRegistry = func() agent.Registry { return agent.WindowsRegistry{} }
	newStartupTasks    = func() agent.Tasks { return agent.WindowsTasks{} }
)

// agentWait is how long start/stop wait for the agent to appear or disappear.
var agentWait = 4 * time.Second

// ---- drivedex agent ---------------------------------------------------------------------

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Control the background agent that indexes drives when you plug them in",
		Long: `The agent is a small program with no window that watches for anything with
files on it being plugged in, and indexes it by itself - so you never have to
remember to run 'drivedex index':

  - external drives: USB disks and pen drives, memory cards (including the
    laptop's own card reader), FireWire, eSATA and Thunderbolt disks;
  - phones, iPhones, cameras and other portable devices that Explorer shows
    under This PC (on a phone, choose "File transfer" in its USB notification);
  - Android phones with USB debugging on (read over adb, which is faster).

Internal disks are left alone unless listed in auto_index ('drivedex config
edit'). A locked phone is tried again until it is unlocked. The agent writes a
log you can read with 'drivedex agent status'.

To make it start every time you log in, use 'drivedex startup enable'.`,
		Example: `  drivedex agent start
  drivedex agent status
  drivedex agent stop`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		&cobra.Command{
			Use: "start", Short: "Start the background agent", Args: cobra.NoArgs,
			Long: `Starts the agent in the background with no window. If it is already running
nothing happens. From then on, plugging in an external drive, a phone or
another portable device refreshes its index automatically.`,
			Example: `  drivedex agent start
  drivedex agent start && drivedex agent status`,
			RunE: func(cmd *cobra.Command, args []string) error { return runAgentStart(cmd.OutOrStdout()) },
		},
		&cobra.Command{
			Use: "stop", Short: "Stop the background agent", Args: cobra.NoArgs,
			Long: `Stops the background agent. Your saved indexes are not touched; drives just
stop being refreshed automatically until you start the agent again.`,
			Example: `  drivedex agent stop
  drivedex agent stop && drivedex agent start   (restart after changing config.toml)`,
			RunE: func(cmd *cobra.Command, args []string) error { return runAgentStop(cmd.OutOrStdout()) },
		},
		&cobra.Command{
			Use: "status", Short: "Show whether the agent is running and what it did recently", Args: cobra.NoArgs,
			Long: `Says whether the agent is running, whether it starts with Windows, and shows
the last lines of its log - a good first stop if a plugged-in drive wasn't
indexed.`,
			Example: `  drivedex agent status
  drivedex doctor`,
			RunE: func(cmd *cobra.Command, args []string) error { return runAgentStatus(cmd.OutOrStdout()) },
		},
	)
	return cmd
}

// currentAgent reports the agent recorded in the PID file, if it is really alive.
func currentAgent(dir string) (agent.PIDInfo, bool) {
	info, err := agent.ReadPID(dir)
	if err != nil {
		return agent.PIDInfo{}, false
	}
	return info, processRunning(info.PID)
}

func runAgentStart(out io.Writer) error {
	dir, err := agentDataDir()
	if err != nil {
		return userErr("couldn't find DriveDex's data folder", "set the DRIVEDEX_DATA_DIR environment variable to a writable folder", err)
	}
	if info, running := currentAgent(dir); running {
		fmt.Fprintf(out, "The agent is already running (PID %d).\n", info.PID)
		return nil
	}
	exe, err := agentExe()
	if err != nil {
		return userErr("the background agent program wasn't found", "it should sit next to drivedex.exe - run scripts\\build.ps1 again, or reinstall DriveDex", err)
	}
	return startAgentProgram(out, dir, exe)
}

// startAgentProgram launches the agent at exe and waits for it to report in.
// (`drivedex install` uses it to start the freshly installed copy.)
func startAgentProgram(out io.Writer, dir, exe string) error {
	if err := agentStart(exe); err != nil {
		return userErr("couldn't start the background agent", "run with --debug for details", err)
	}
	// The agent writes its PID file once it is up; wait for it to appear.
	deadline := time.Now().Add(agentWait)
	for time.Now().Before(deadline) {
		if info, running := currentAgent(dir); running {
			fmt.Fprintf(out, "Agent started (PID %d). It will index external drives, phones and other devices when you plug them in.\n", info.PID)
			fmt.Fprintln(out, ui.Dim("Log: "+agent.LogPath(dir)))
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return userErr("the agent was launched but didn't report in", "look at the log for the reason: "+agent.LogPath(dir), nil)
}

func runAgentStop(out io.Writer) error {
	dir, err := agentDataDir()
	if err != nil {
		return userErr("couldn't find DriveDex's data folder", "set the DRIVEDEX_DATA_DIR environment variable to a writable folder", err)
	}
	info, running := currentAgent(dir)
	if !running {
		agent.RemovePID(dir) // a leftover file from a crash
		fmt.Fprintln(out, "The agent isn't running.")
		return nil
	}
	if err := agentKill(info.PID); err != nil {
		return userErr("couldn't stop the agent", "if it was started as administrator, stop it from an Administrator terminal", err)
	}
	deadline := time.Now().Add(agentWait)
	for time.Now().Before(deadline) && processRunning(info.PID) {
		time.Sleep(50 * time.Millisecond)
	}
	agent.RemovePID(dir)
	fmt.Fprintf(out, "Agent stopped (was PID %d).\n", info.PID)
	return nil
}

func runAgentStatus(out io.Writer) error {
	dir, err := agentDataDir()
	if err != nil {
		return userErr("couldn't find DriveDex's data folder", "set the DRIVEDEX_DATA_DIR environment variable to a writable folder", err)
	}
	if info, running := currentAgent(dir); running {
		up := ""
		if !info.Started.IsZero() {
			up = ", up " + ui.FormatDuration(time.Since(info.Started).Round(time.Second))
		}
		fmt.Fprintf(out, "Agent: %s (PID %d%s)\n", ui.Green("running"), info.PID, up)
	} else {
		fmt.Fprintf(out, "Agent: %s. Start it with: drivedex agent start\n", ui.Yellow("not running"))
	}

	if st, err := agent.Status(newStartupRegistry(), newStartupTasks()); err == nil {
		fmt.Fprintln(out, "Start-up: "+st.Describe())
	}

	lines, err := agent.TailLines(agent.LogPath(dir), 10)
	if err != nil {
		return userErr("couldn't read the agent log", "check the file is readable: "+agent.LogPath(dir), err)
	}
	if len(lines) == 0 {
		fmt.Fprintln(out, ui.Dim("Log is empty."))
		return nil
	}
	fmt.Fprintln(out, "\nLast log lines ("+agent.LogPath(dir)+"):")
	for _, l := range lines {
		fmt.Fprintln(out, ui.Dim("  "+l))
	}
	return nil
}

// ---- drivedex startup -----------------------------------------------------------------

func newStartupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "startup",
		Short: "Make the background agent start when you log in to Windows",
		Long: `Controls whether the background agent starts automatically every time you
log in. 'enable' adds a per-user entry (no administrator rights needed);
'enable --elevated' instead creates a scheduled task that starts the agent with
administrator rights, which lets it use NTFS turbo mode in the background.`,
		Example: `  drivedex startup enable
  drivedex startup status
  drivedex startup disable`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	var elevated, yes bool
	enable := &cobra.Command{
		Use:   "enable",
		Short: "Start the agent at log-in",
		Long: `Makes the background agent start every time you log in to Windows. It first
shows exactly what it will add and asks. Without --elevated it only adds a
per-user entry, so no administrator rights are needed.`,
		Example: `  drivedex startup enable
  drivedex startup enable --elevated   (run from an Administrator terminal)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStartupEnable(cmd.OutOrStdout(), cmd.InOrStdin(), elevated, yes)
		},
	}
	enable.Flags().BoolVar(&elevated, "elevated", false, "start with administrator rights (needs an Administrator terminal once)")
	enable.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")

	cmd.AddCommand(enable,
		&cobra.Command{
			Use: "disable", Short: "Stop the agent starting at log-in", Args: cobra.NoArgs,
			Long: `Removes whatever 'startup enable' added (the per-user entry and/or the
scheduled task). The agent keeps running until you log out or stop it.`,
			Example: `  drivedex startup disable
  drivedex agent stop`,
			RunE: func(cmd *cobra.Command, args []string) error { return runStartupDisable(cmd.OutOrStdout()) },
		},
		&cobra.Command{
			Use: "status", Short: "Show how the agent starts at log-in", Args: cobra.NoArgs,
			Long: `Says whether the agent starts when you log in, and if so whether through the
per-user entry or the elevated scheduled task.`,
			Example: `  drivedex startup status
  drivedex agent status`,
			RunE: func(cmd *cobra.Command, args []string) error { return runStartupStatus(cmd.OutOrStdout()) },
		},
	)
	return cmd
}

func startupUnsupported(err error) error {
	if errors.Is(err, errors.ErrUnsupported) {
		return userErr("start-up settings only work on Windows", "run DriveDex on your Windows PC", err)
	}
	return nil
}

func runStartupEnable(out io.Writer, in io.Reader, elevated, yes bool) error {
	exe, err := agentExe()
	if err != nil {
		return userErr("the background agent program wasn't found", "it should sit next to drivedex.exe - run scripts\\build.ps1 again, or reinstall DriveDex", err)
	}
	return enableStartup(out, in, elevated, yes, exe)
}

// enableStartup makes the agent at exe start at log-in. (`drivedex install`
// calls it with the agent it just installed.)
func enableStartup(out io.Writer, in io.Reader, elevated, yes bool, exe string) error {
	if !yes {
		if elevated {
			fmt.Fprintln(out, "This creates a Windows scheduled task that starts the DriveDex agent with ADMINISTRATOR rights every time you log in.")
			fmt.Fprintln(out, "That lets it use NTFS turbo mode in the background, but it also means a program runs with administrator rights at every log-in.")
			fmt.Fprintln(out, "(This command must be run from an Administrator terminal.)")
		} else {
			fmt.Fprintln(out, `This adds an entry under HKEY_CURRENT_USER\`+agent.RunKeyPath+` so the DriveDex agent starts every time you log in.`)
			fmt.Fprintln(out, "No administrator rights are needed, and 'drivedex startup disable' removes it again.")
		}
		fmt.Fprint(out, "Continue? [y/N] ")
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Cancelled. Nothing was changed.")
			return nil
		}
	}
	err := agent.Enable(newStartupRegistry(), newStartupTasks(), exe, elevated)
	switch {
	case startupUnsupported(err) != nil:
		return startupUnsupported(err)
	case errors.Is(err, agent.ErrNeedAdminForTask):
		return userErr("creating the elevated start-up task needs an Administrator terminal",
			"right-click PowerShell, choose \"Run as administrator\", and run the command again - or use 'drivedex startup enable' without --elevated", err)
	case err != nil:
		return userErr("couldn't set up start-up", "run with --debug for details", err)
	}
	if elevated {
		fmt.Fprintln(out, "Done. The agent will start with administrator rights at your next log-in.")
	} else {
		fmt.Fprintln(out, "Done. The agent will start at your next log-in.")
	}
	fmt.Fprintln(out, ui.Dim("To start it right now: drivedex agent start"))
	return nil
}

func runStartupDisable(out io.Writer) error {
	run, task, err := agent.Disable(newStartupRegistry(), newStartupTasks())
	if startupUnsupported(err) != nil {
		return startupUnsupported(err)
	}
	if err != nil {
		return userErr("couldn't remove the start-up entry", "if it was created with --elevated, run this from an Administrator terminal", err)
	}
	switch {
	case !run && !task:
		fmt.Fprintln(out, "The agent wasn't set to start at log-in. Nothing to remove.")
	default:
		var what []string
		if run {
			what = append(what, "the Run entry")
		}
		if task {
			what = append(what, "the scheduled task")
		}
		fmt.Fprintf(out, "Removed %s. The agent will no longer start at log-in.\n", strings.Join(what, " and "))
	}
	return nil
}

func runStartupStatus(out io.Writer) error {
	st, err := agent.Status(newStartupRegistry(), newStartupTasks())
	if startupUnsupported(err) != nil {
		return startupUnsupported(err)
	}
	if err != nil {
		return userErr("couldn't read the start-up settings", "run with --debug for details", err)
	}
	fmt.Fprintln(out, "The agent "+st.Describe()+".")
	return nil
}
