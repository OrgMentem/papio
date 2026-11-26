// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import (
	"os"
	"runtime"
	"testing"
)

// The start time StopDaemon compares must read the same for one running
// process every time, or a daemon that is still stopping would look recycled.
func TestProcessStartIsStableForARunningProcess(t *testing.T) {
	first, ok := platformProcessStart(os.Getpid())
	if !ok {
		switch runtime.GOOS {
		case "linux", "darwin", "windows":
			t.Fatalf("%s cannot read a process start time, so StopDaemon cannot tell a recycled pid", runtime.GOOS)
		}
		t.Skipf("%s has no process start time", runtime.GOOS)
	}
	for range 3 {
		if again, ok := platformProcessStart(os.Getpid()); !ok || again != first {
			t.Fatalf("process start = %d, %v; want %d on every read", again, ok, first)
		}
	}
}
