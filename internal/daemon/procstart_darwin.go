// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import "golang.org/x/sys/unix"

// platformProcessStart reads when the process with pid began, in microseconds
// since the epoch, from the kernel's process table.
func platformProcessStart(pid int) (uint64, bool) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, false
	}
	began := proc.Proc.P_starttime
	return uint64(began.Sec)*1_000_000 + uint64(began.Usec), true //nolint:gosec // G115: a start time is not negative.
}
