// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"strings"
	"testing"

	"papio/internal/job"
)

// Every list method refuses a state filter that names no state: an unknown
// value matches nothing, so it would answer an empty page for a typo.
func TestJobListMethodsRefuseAnUnknownStateFilter(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	for _, method := range []string{"jobs.list", "jobs.list_v2", "jobs.list_v3"} {
		rpcErr := callMethod(t, router, method, map[string]any{"state": "awaiting-human"}, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" || !strings.Contains(rpcErr.Message, "awaiting_human") {
			t.Fatalf("%s with state awaiting-human = %+v; want invalid_argument listing the valid states", method, rpcErr)
		}
		if rpcErr := callMethod(t, router, method, map[string]any{"state": job.StateAwaitingHuman}, nil); rpcErr != nil {
			t.Fatalf("%s with state %s = %+v; want it accepted", method, job.StateAwaitingHuman, rpcErr)
		}
	}
}
