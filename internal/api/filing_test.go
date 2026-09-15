// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/hook"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/work"
)

func TestJobsUnfiledReturnsEnvelope(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "wr_jobs_unfiled_api", work.Work{
		Title: "Filing API", DOI: "10.1000/filing-api",
	}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20,
	}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}

	var envelope map[string]json.RawMessage
	if rpcErr := callMethod(t, Router(system), "jobs.unfiled", map[string]any{
		"filter": "missing", "limit": 10,
	}, &envelope); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	assertRatifiedKeySet(t, envelope, "jobs", "truncated")
	var rows []app.UnfiledJob
	if err := json.Unmarshal(envelope["jobs"], &rows); err != nil {
		t.Fatal(err)
	}
	var truncated bool
	if err := json.Unmarshal(envelope["truncated"], &truncated); err != nil {
		t.Fatal(err)
	}
	if truncated || len(rows) != 1 || rows[0].JobID != id || rows[0].Filing != "missing" {
		t.Fatalf("jobs.unfiled envelope = %+v", envelope)
	}
}

func TestJobsRefileWithoutHookIsInvalidArgument(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "wr_jobs_refile_api", work.Work{DOI: "10.1000/refile-api"}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20,
	}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}

	rpcErr := callMethod(t, Router(system), "jobs.refile", map[string]string{"job_id": id}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" || !strings.Contains(rpcErr.Message, "[hooks] on_ready") {
		t.Fatalf("jobs.refile error = %#v, want invalid_argument naming [hooks] on_ready", rpcErr)
	}
}

func TestFailedHookAppearsInUnfiledAndDoctor(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	system.Config.Hooks.OnReady = "exit 1"
	system.Config.Hooks.TimeoutSeconds = 5
	system.Config.Zotio.Executable = ""
	system.Config.Browser.AdoptionRoot = filepath.Join(system.Config.DataDir, "adoptions")
	system.App.ReadyHook = &hook.Runner{Command: "exit 1", Timeout: 5 * time.Second}
	id, err := system.Jobs.CreateRequest(ctx, "wr_filing_doctor", work.Work{
		Title: "Failed filing doctor", DOI: "10.1000/filing-doctor",
	}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20,
	}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := system.Store.DB().ExecContext(ctx, `UPDATE jobs SET artifact_sha256 = ? WHERE id = ?`, sha, id); err != nil {
		t.Fatal(err)
	}
	result, err := system.App.RefileJob(ctx, id)
	if err != nil || result.Status != "failed" || result.ExitCode != 1 {
		t.Fatalf("refile result = %+v, %v", result, err)
	}

	var page UnfiledPage
	if rpcErr := callMethod(t, Router(system), "jobs.unfiled", map[string]any{"limit": 10}, &page); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(page.Jobs) != 1 || page.Jobs[0].JobID != id || page.Jobs[0].Filing != "failed" {
		t.Fatalf("jobs.unfiled page = %+v", page)
	}

	report := doctor.Run(ctx, system.Config, system.Store, pdf.Capability{}, "", nil)
	for _, check := range report.Checks {
		if check.Name != "filing" {
			continue
		}
		if check.Status != doctor.Warn || !strings.Contains(check.Detail, "1 ready or imported job") ||
			!strings.Contains(check.Remediation, "papio jobs unfiled") ||
			!strings.Contains(check.Remediation, "papio jobs refile <id>") {
			t.Fatalf("filing doctor check = %+v", check)
		}
		return
	}
	t.Fatalf("filing doctor check missing: %+v", report.Checks)
}
