// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/app"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/work"
)

// readyComponentJob returns a ready job that holds a main artifact and has no
// browser adoption directory, which is what the daemon's terminal sweep leaves.
func readyComponentJob(t *testing.T, system *bootstrap.System, requestID string) string {
	t.Helper()
	system.App.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true},
		}, nil
	}
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, requestID, work.Work{DOI: "10.1000/" + requestID}, "", "",
		job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	const mainSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := system.Jobs.UpsertArtifact(ctx, job.Artifact{SHA256: mainSHA, SizeBytes: 1, MIME: "application/pdf", Path: filepath.Join(t.TempDir(), "main.pdf"), IdentityResult: "pass"}); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil, job.WithArtifact(mainSHA)); err != nil {
		t.Fatal(err)
	}
	return id
}

func stageComponentFixture(t *testing.T, system *bootstrap.System, jobID, name string) string {
	t.Helper()
	dir, err := app.ComponentStagingDir(system.Config.DataDir, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("%PDF-1.4\nsupplement\n%%EOF"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A ready job's adoption directory is gone, and that must not read as a job
// without a main artifact: it holds one, so the message has to say what is
// really missing and where to go instead.
func TestAddComponentRPCNamesAMissingAdoptionDirectoryHonestly(t *testing.T) {
	system := testSystem(t)
	id := readyComponentJob(t, system, "wr_component_rpc_no_dir")
	path := filepath.Join(system.App.Config.EffectiveAdoptionRoot(), id, "supp.pdf")

	rpcErr := callMethod(t, Router(system), "jobs.add_component", map[string]string{
		"job_id": id, "path": path, "role": job.ComponentSupplement,
	}, nil)
	if rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("add_component without an adoption dir = %+v; want precondition_failed", rpcErr)
	}
	if strings.Contains(rpcErr.Message, "no main artifact") {
		t.Fatalf("message %q claims the ready job holds no main artifact", rpcErr.Message)
	}
	if !strings.Contains(rpcErr.Message, "adoption directory") || strings.Contains(rpcErr.Message, id) || strings.Contains(rpcErr.Message, system.Config.DataDir) {
		t.Fatalf("message %q should name the adoption directory and echo no path", rpcErr.Message)
	}
}

func TestAddComponentRPCStillNamesAMissingMainArtifact(t *testing.T) {
	system := testSystem(t)
	id := parkedSupplyJob(t, system, "wr_component_rpc_no_main")
	rpcErr := callMethod(t, Router(system), "jobs.add_component_staged", map[string]string{
		"job_id": id, "name": "supp.pdf", "role": job.ComponentSupplement,
	}, nil)
	if rpcErr == nil || rpcErr.Code != "precondition_failed" || !strings.Contains(rpcErr.Message, "no main artifact") {
		t.Fatalf("add_component_staged for a job without a main artifact = %+v; want precondition_failed naming it", rpcErr)
	}
}

func TestAddStagedComponentRPCFilesAFileForAReadyJob(t *testing.T) {
	system := testSystem(t)
	id := readyComponentJob(t, system, "wr_component_rpc_staged")
	staged := stageComponentFixture(t, system, id, "supp.pdf")

	var result struct {
		Components []job.Component `json:"components"`
		Truncated  bool            `json:"truncated"`
	}
	if rpcErr := callMethod(t, Router(system), "jobs.add_component_staged", map[string]string{
		"job_id": id, "name": "supp.pdf", "role": job.ComponentSupplement,
	}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(result.Components) != 2 || result.Components[1].Role != job.ComponentSupplement || result.Truncated {
		t.Fatalf("components = %+v, truncated %v; want main plus supplement", result.Components, result.Truncated)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged copy remains after adoption: %v", err)
	}
}

// The method takes a name, never a path: an extra path field is refused by the
// strict decoder, and a name that leaves the staging directory is refused
// without echoing it.
func TestAddStagedComponentRPCAcceptsOnlyAStagedName(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	id := readyComponentJob(t, system, "wr_component_rpc_name")
	outside := filepath.Join(system.Config.DataDir, "outside.pdf")

	rpcErr := callMethod(t, router, "jobs.add_component_staged", map[string]string{"job_id": id, "path": outside, "name": "x.pdf", "role": job.ComponentSupplement}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("add_component_staged with a path = %+v; want invalid_argument", rpcErr)
	}
	for _, name := range []string{"../../outside.pdf", outside, "missing.pdf"} {
		rpcErr := callMethod(t, router, "jobs.add_component_staged", map[string]string{"job_id": id, "name": name, "role": job.ComponentSupplement}, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("add_component_staged name %q = %+v; want invalid_argument", name, rpcErr)
		}
		if strings.Contains(rpcErr.Message, "outside") || strings.Contains(rpcErr.Message, system.Config.DataDir) {
			t.Fatalf("add_component_staged error %q echoes the caller's path", rpcErr.Message)
		}
	}
	missing := callMethod(t, router, "jobs.add_component_staged", map[string]string{"job_id": id, "name": "x.pdf"}, nil)
	if missing == nil || missing.Code != "invalid_argument" {
		t.Fatalf("add_component_staged without a role = %+v; want invalid_argument", missing)
	}
}

func TestAddStagedComponentRPCRefusesHTMLFullText(t *testing.T) {
	system := testSystem(t)
	rpcErr := callMethod(t, Router(system), "jobs.add_component_staged", map[string]string{
		"job_id": "job_whatever", "name": "x.html", "role": job.ComponentHTMLFullText,
	}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("add_component_staged html_fulltext = %+v, want invalid_argument", rpcErr)
	}
}
