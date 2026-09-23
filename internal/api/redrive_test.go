// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"os"
	"path/filepath"
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

// An open-access browser route that answered HTML is a spent route too: the
// live Wiley job parked on a pdfdirect URL could not be redriven because its
// open action was the OA handoff rather than a manual download.
func TestRedriveIPCReplacesSpentOpenAccessHandoff(t *testing.T) {
	system := testSystem(t)
	system.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "wr_redrive_oa", work.Work{DOI: "10.1111/j.1469-7610.2010.02303.x"}, "", "",
		job.Policy{AccessMode: "delegated", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	oa := app.OABrowserHandoffActionDetail("https://onlinelibrary.wiley.com/doi/pdfdirect/10.1111/j.1469-7610.2010.02303.x")
	if err := system.Jobs.ParkWithHumanAction(ctx, id, job.StateResolving, job.StateAwaitingHuman,
		"openurl_handoff", oa, nil, job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.RecordEvent(ctx, id, "browser.error", map[string]any{"code": "download_not_pdf"}); err != nil {
		t.Fatal(err)
	}
	var result RedriveResult
	if rpcErr := callMethod(t, Router(system), "jobs.redrive", map[string]any{"job_id": id, "expected_revision": 1}, &result); rpcErr != nil {
		t.Fatalf("redrive of a spent OA handoff: %+v", rpcErr)
	}
	open, err := system.Jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].ID != result.ActionID || open[0].Detail != app.InstitutionalOpenURLHandoffDetail {
		t.Fatalf("replacement=%+v err=%v, want the institutional handoff only", open, err)
	}
}

// An adopted file that failed validation and could not be moved to rejected/
// parks the job in needs_review. Redrive reopens it only once the adoption
// directory holds no file the sweep would adopt and reject again.
func TestRedriveIPCReopensUnquarantinedAdoptionParkOnceFileIsGone(t *testing.T) {
	system := testSystem(t)
	system.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "wr_redrive_unquarantined", work.Work{DOI: "10.1111/j.1545-5300.2009.01299.x"}, "", "",
		job.Policy{AccessMode: "delegated", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.ParkWithHumanAction(ctx, id, job.StateResolving, job.StateNeedsReview, "manual_download",
		"the adopted download failed validation and could not be quarantined; remove or replace the file in the adoption directory",
		nil, job.Access(false, ""), job.WithHumanActionDiagnosis(job.DiagnosisReasonAdoptedPDFInvalid)); err != nil {
		t.Fatal(err)
	}
	landing := filepath.Join(system.Config.EffectiveAdoptionRoot(), id)
	if err := os.MkdirAll(landing, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(landing, "paper.pdf")
	if err := os.WriteFile(stale, []byte("not a pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(landing, ".DS_Store"), []byte{0}, 0o600); err != nil {
		t.Fatal(err)
	}
	router := Router(system)
	params := map[string]any{"job_id": id, "expected_revision": 1}
	if rpcErr := callMethod(t, router, "jobs.redrive", params, nil); rpcErr == nil || rpcErr.Code != "conflict" {
		t.Fatalf("redrive with the rejected file still landed=%+v; want conflict", rpcErr)
	}
	if err := os.Remove(stale); err != nil {
		t.Fatal(err)
	}
	var result RedriveResult
	if rpcErr := callMethod(t, router, "jobs.redrive", params, &result); rpcErr != nil {
		t.Fatalf("redrive once the file is gone: %+v", rpcErr)
	}
	row, err := system.Jobs.Get(ctx, id)
	if err != nil || row.State != job.StateAwaitingHuman {
		t.Fatalf("row=%+v err=%v; want awaiting_human", row, err)
	}
	open, err := system.Jobs.ListOpenHumanActionsForJobs(ctx, []string{id})
	if err != nil || len(open) != 1 || open[0].ID != result.ActionID || open[0].Detail != app.InstitutionalOpenURLHandoffDetail {
		t.Fatalf("replacement=%+v err=%v, want the institutional handoff only", open, err)
	}
}
