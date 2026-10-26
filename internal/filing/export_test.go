// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package filing

import "testing"

// SetBeforeIOForTest exposes the existing worker seam to integration tests.
func SetBeforeIOForTest(t *testing.T, hook func(string)) {
	t.Helper()
	old := beforeIO
	beforeIO = hook
	t.Cleanup(func() {
		waitFor(t, func() bool { return !Stalled() && len(gate) == 0 })
		beforeIO = old
	})
}
