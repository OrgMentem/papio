// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bench

import (
	"os"
	"testing"

	"papio/internal/store/storetest"
)

// TestMain seeds every overlay data dir from storetest's migrated template.
// Run opens two stores (baseline and current); without the seed each open runs
// every migration, which dominates the package's -race time.
func TestMain(m *testing.M) {
	seedOverlayDir = storetest.Seed
	os.Exit(m.Run())
}
