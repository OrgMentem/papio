//go:build windows

package hook

import "os/exec"

// setProcessGroup is a no-op on Windows: there is no POSIX process group to
// create; WaitDelay still bounds the pipe wait for orphaned descendants.
func setProcessGroup(*exec.Cmd) {}

// killProcessGroup kills the shell process. Descendants that survive are
// bounded by WaitDelay releasing the pipes.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
