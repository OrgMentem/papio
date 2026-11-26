// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !linux && !darwin && !windows

package daemon

// platformProcessStart is unavailable here, so StopDaemon names a daemon by its
// process id alone.
func platformProcessStart(int) (uint64, bool) { return 0, false }
