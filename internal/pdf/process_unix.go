// Copyright 2026 OrgMentem. Licensed under MIT.
//go:build !windows

package pdf

import (
	"os/exec"
	"syscall"
)

// configureProcessTree puts an external converter in a process group so a
// timeout cannot leave a child (for example shell-launched tesseract) running.
func configureProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
