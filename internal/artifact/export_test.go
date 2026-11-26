// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package artifact

import "testing"

// FailFlushesForTest makes Promote's file flush and directory flush return the
// given errors; nil leaves that flush real. The returned function restores both
// flushes early, and the test cleanup restores them regardless. It is visible
// only to this package's tests, including the external artifact_test package
// that needs the job store (which imports artifact).
func FailFlushesForTest(t *testing.T, fileErr, dirErr error) (restore func()) {
	t.Helper()
	origFile, origDir := syncFile, syncDir
	if fileErr != nil {
		syncFile = func(string) error { return fileErr }
	}
	if dirErr != nil {
		syncDir = func(string) error { return dirErr }
	}
	restore = func() { syncFile, syncDir = origFile, origDir }
	t.Cleanup(restore)
	return restore
}
