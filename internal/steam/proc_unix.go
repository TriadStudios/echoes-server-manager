//go:build !windows

package steam

import (
	"os/exec"
	"syscall"
)

// ownGroup runs SteamCMD in a process group of its own, and ending the run
// ends the whole group: steamcmd.sh starts the real SteamCMD as a child.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
