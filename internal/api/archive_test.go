// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"strings"
	"testing"

	"papio/internal/job"
	"papio/internal/work"
)

func TestArchivePublicAPIRequiresConfirmationAndPreservesJobInspection(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, "wr_archive_api", work.Work{DOI: "10.1000/archive-api", Title: "Archive API"}, "", "", job.Policy{AccessMode: "conservative", FetchMaxBytes: 1 << 20}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 64)
	if err := system.Jobs.UpsertArtifact(ctx, job.Artifact{SHA256: sha, SizeBytes: 5000, MIME: "application/pdf", IdentityResult: "pass", Path: "/fixture/archive.pdf"}); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil, job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	router := Router(system)
	if rpcErr := callMethod(t, router, "jobs.archive", map[string]any{"job_id": id}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("unconfirmed archive = %+v", rpcErr)
	}
	var result job.DispositionResult
	if rpcErr := callMethod(t, router, "jobs.archive", map[string]any{"job_id": id, "confirmed": true}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.Disposition != job.DispositionArchived || result.State != job.StateReady || result.ArtifactSHA256 != sha {
		t.Fatalf("archive result = %+v", result)
	}
	var detail JobDetail
	if rpcErr := callMethod(t, router, "jobs.get", map[string]string{"job_id": id}, &detail); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if detail.Job.Work.DOI != "10.1000/archive-api" || detail.Job.ArtifactSHA256 != sha {
		t.Fatalf("archived inspection = %+v", detail.Job)
	}
	if rpcErr := callMethod(t, router, "jobs.restore", map[string]string{"job_id": id}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.Disposition != job.DispositionActive || !result.Changed {
		t.Fatalf("restore result = %+v", result)
	}
}
