//go:build windows

package daemon

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
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

// processExited reports whether the process with pid has ended. A process
// this user may not open is treated as still running.
func processExited(pid int) bool {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: pid came from GetNamedPipeServerProcessId, a DWORD.
	if err != nil {
		return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	event, err := windows.WaitForSingleObject(handle, 0)
	return err == nil && event == windows.WAIT_OBJECT_0
}
