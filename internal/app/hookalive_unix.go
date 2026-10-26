//go:build !windows

package app

import (
	"errors"
	"syscall"
)

// hookOwnerAlive reports whether a hook reservation owner with pid may still
// run. Signal 0 probes existence without disturbing the process: ESRCH means
// the pid is gone, while EPERM (another user's process) means it exists.
func hookOwnerAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
