// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"strings"
	"testing"
)

// The daemon's working directory is not the caller's, so a relative
// destination would write into a directory the caller never named.
func TestBundleExportRefusesARelativeOutputDirectory(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	for _, method := range []string{"bundle.export", "bundle.export_v2"} {
		rpcErr := callMethod(t, router, method, map[string]any{"job_id": "job_any", "output_dir": "bundle-out"}, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" || !strings.Contains(rpcErr.Message, "absolute") {
			t.Fatalf("%s with a relative output_dir = %+v; want invalid_argument asking for an absolute path", method, rpcErr)
		}
	}
}
