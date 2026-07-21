//go:build !windows

package hook

import (
	"os/exec"
	"syscall"
)

// setProcessGroup makes the shell the leader of a fresh process group so the
// deadline can address the whole hook process tree.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup terminates every process in the hook's group. Falls back
// to killing the shell alone if the group signal fails.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
