//go:build !windows

package daemon

import (
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
