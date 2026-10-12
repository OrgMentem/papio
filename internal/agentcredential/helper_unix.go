// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build !windows

package agentcredential

import (
	"os/exec"
	"syscall"
)

// configureHelperProcess puts the helper in its own process group and kills the
// whole group on cancellation. The OS credential library can itself start a
// program (macOS runs /usr/bin/security), and a hung one must not outlive the
// call that was cancelled.
func configureHelperProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
