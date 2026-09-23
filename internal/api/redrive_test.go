// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"testing"

	"papio/internal/app"
	"papio/internal/job"
	"papio/internal/work"
)

func TestRedriveIPCRequiresRevision(t *testing.T) {
	system := testSystem(t)
	if rpcErr := callMethod(t, Router(system), "jobs.redrive", map[string]string{"job_id": "job_01"}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("missing revision error=%+v", rpcErr)
	}
}

func TestRedriveIPCUsesOrdinaryInstitutionalHandoff(t *testing.T) {
	system := testSystem(t)
	system.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "wr_redrive_ipc", work.Work{DOI: "10.1000/redrive-ipc"}, "", "",
		job.Policy{AccessMode: "delegated", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.ParkWithHumanAction(ctx, id, job.StateResolving, job.StateAwaitingHuman,
		"manual_download", "legacy failure without diagnosis", nil, job.Access(true, "landing_page")); err != nil {
		t.Fatal(err)
	}
	router := Router(system)
	var result RedriveResult
	if rpcErr := callMethod(t, router, "jobs.redrive", map[string]any{"job_id": id, "expected_revision": 1}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.JobID != id || result.ActionID <= 0 {
		t.Fatalf("redrive result=%+v", result)
	}
	open, err := system.Jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].ID != result.ActionID || open[0].Detail != app.InstitutionalOpenURLHandoffDetail {
		t.Fatalf("ordinary handoff=%+v err=%v", open, err)
	}
	if rpcErr := callMethod(t, router, "jobs.redrive", map[string]any{"job_id": id, "expected_revision": 1}, nil); rpcErr == nil || rpcErr.Code != "conflict" {
		t.Fatalf("repeat rpc error=%+v, want conflict", rpcErr)
	}
}
