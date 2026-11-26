// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import "golang.org/x/sys/windows"

// platformProcessStart reads when the process with pid was created, as a
// FILETIME. Windows reuses a process id soon after its process ends, so the
// id alone does not name a daemon for as long as one takes to stop.
func platformProcessStart(pid int) (uint64, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // G115: pid came from GetNamedPipeServerProcessId, a DWORD.
	if err != nil {
		return 0, false
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, false
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), true
}
