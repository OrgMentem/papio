//go:build windows

package daemon

import (
	"os"
	"os/exec"
)

var (
	graceful = os.Interrupt
	hard     = os.Kill
)

func configureDaemonProcessGroup(*exec.Cmd) {}

func terminateSignal(cmd *exec.Cmd, sig os.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Signal(sig); err == nil {
		return nil
	}
	if sig == os.Interrupt {
		return cmd.Process.Signal(os.Kill)
	}
	return cmd.Process.Kill()
}
