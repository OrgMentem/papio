// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"strings"
	"testing"
)

func TestRefilePreviewRPCRequiresAJobAndAConfiguredHook(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	if rpcErr := callMethod(t, router, "jobs.refile_preview", map[string]string{"job_id": " "}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("preview without a job id = %+v; want invalid_argument", rpcErr)
	}
	id := readyComponentJob(t, system, "wr_refile_preview_rpc")
	rpcErr := callMethod(t, router, "jobs.refile_preview", map[string]string{"job_id": id}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" || !strings.Contains(rpcErr.Message, "on_ready") {
		t.Fatalf("preview without a hook = %+v; want invalid_argument naming [hooks] on_ready", rpcErr)
	}
}
