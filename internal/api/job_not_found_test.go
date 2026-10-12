// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"strings"
	"testing"
)

// A mistyped or foreign job id must name itself and the command that lists the
// real ones, not surface the database's "record not found".
func TestJobScopedMethodsNameAnUnknownJobID(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	for _, method := range []string{"jobs.get", "jobs.get_v2", "jobs.get_v3", "jobs.cancel", "jobs.retry", "jobs.refile", "jobs.refile_preview", "jobs.diagnose_v2", "delivery.get"} {
		rpcErr := callMethod(t, router, method, map[string]string{"job_id": "job_doesnotexist"}, nil)
		if rpcErr == nil || rpcErr.Code != "not_found" {
			t.Fatalf("%s for an unknown job = %+v; want not_found", method, rpcErr)
		}
		if !strings.Contains(rpcErr.Message, `"job_doesnotexist"`) || !strings.Contains(rpcErr.Message, "papio jobs list") {
			t.Fatalf("%s message %q must name the id and `papio jobs list`", method, rpcErr.Message)
		}
	}
}
