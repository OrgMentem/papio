//go:build !windows

package daemon

import (
	"errors"
	"os/exec"
	"syscall"
)

const (
	graceful = syscall.SIGTERM
	hard     = syscall.SIGKILL
)

func configureDaemonProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateSignal(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if cmd.Process.Pid > 0 {
		if err := syscall.Kill(-cmd.Process.Pid, sig); err == nil {
			return nil
		}
	}
	return cmd.Process.Signal(sig)
}

// processExited reports whether no process has pid. A process owned by
// another user answers EPERM, which means it exists.
func processExited(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
