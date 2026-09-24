//go:build !windows

package hook

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand runs command through sh -c.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

// procGuard confines one hook run so the deadline can address the whole hook
// process tree. On Unix the confinement is a POSIX process group, which the
// kernel establishes at fork and which owns no per-run resources.
type procGuard struct{}

// newProcGuard makes the shell the leader of a fresh process group.
func newProcGuard(cmd *exec.Cmd) *procGuard {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &procGuard{}
}

// confine is a no-op: Setpgid took effect at fork, so the shell is already
// confined before it could spawn anything.
func (*procGuard) confine(*exec.Cmd) error { return nil }

// kill terminates every process in the hook's group. Falls back to killing
// the shell alone if the group signal fails.
func (*procGuard) kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// close is a no-op: a process group holds no handle to release.
func (*procGuard) close() {}
