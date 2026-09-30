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

// parkedSupplyJob creates a job parked for a human download, the state a
// supplied PDF normally meets.
func parkedSupplyJob(t *testing.T, system *bootstrap.System, requestID string) string {
	t.Helper()
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, requestID, work.Work{DOI: "10.1000/" + requestID, Title: "Supplied work"}, "", "",
		job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := system.Jobs.Transition(ctx, id, step[0], step[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := system.Jobs.OpenHumanAction(ctx, id, job.CandidateEligibleKind, "please download the paper", job.Access(false, "")); err != nil {
		t.Fatal(err)
	}
	return id
}

func stageSupplyFixture(t *testing.T, system *bootstrap.System, jobID, name string) {
	t.Helper()
	dir, err := app.SuppliedPDFStagingDir(system.Config.DataDir, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := append([]byte("%PDF-1.4\nsupplied\n"), make([]byte, pdf.MinimumPayloadBytes+100)...)
	body = append(body, []byte("\n%%EOF")...)
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// jobs.supply_pdf takes no path: an extra field is refused by the strict
// decoder, and a name that leaves the staging directory is refused without
// echoing it.
func TestSupplyPDFRPCAcceptsOnlyAStagedName(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	id := parkedSupplyJob(t, system, "wr_supply_rpc_name")
	outside := filepath.Join(system.Config.DataDir, "outside.pdf")

	if rpcErr := callMethod(t, router, "jobs.supply_pdf", map[string]string{"job_id": id, "path": outside}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("supply_pdf with a path = %+v; want invalid_argument", rpcErr)
	}
	for _, name := range []string{"../../outside.pdf", outside, "missing.pdf"} {
		rpcErr := callMethod(t, router, "jobs.supply_pdf", map[string]string{"job_id": id, "name": name}, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("supply_pdf name %q = %+v; want invalid_argument", name, rpcErr)
		}
		if strings.Contains(rpcErr.Message, "outside") || strings.Contains(rpcErr.Message, system.Config.DataDir) {
			t.Fatalf("supply_pdf error %q echoes the caller's path", rpcErr.Message)
		}
	}
	row, err := system.Jobs.Get(context.Background(), id)
	if err != nil || row.State != job.StateAwaitingHuman {
		t.Fatalf("job after refused supply = %+v, %v; want awaiting_human", row, err)
	}
}

func TestSupplyPDFRPCRefusesATerminalJob(t *testing.T) {
	system := testSystem(t)
	id := parkedSupplyJob(t, system, "wr_supply_rpc_terminal")
	if err := system.Jobs.Cancel(context.Background(), id, job.TerminalReasonBrowserCancelled); err != nil {
		t.Fatal(err)
	}
	stageSupplyFixture(t, system, id, "supplied.pdf")
	rpcErr := callMethod(t, Router(system), "jobs.supply_pdf", map[string]string{"job_id": id, "name": "supplied.pdf"}, nil)
	if rpcErr == nil || rpcErr.Code != "precondition_failed" || !strings.Contains(rpcErr.Message, job.StateCancelled) {
		t.Fatalf("supply_pdf to a cancelled job = %+v; want precondition_failed naming the state", rpcErr)
	}
}

func TestSupplyPDFRPCReportsTheOutcome(t *testing.T) {
	system := testSystem(t)
	system.App.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 2},
			Text:       pdf.TextReport{Chars: 2000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"doi match"}},
		}, nil
	}
	id := parkedSupplyJob(t, system, "wr_supply_rpc_ok")
	stageSupplyFixture(t, system, id, "supplied.pdf")
	var result SupplyPDFResult
	if rpcErr := callMethod(t, Router(system), "jobs.supply_pdf", map[string]string{"job_id": id, "name": "supplied.pdf"}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.JobID != id || result.Outcome != app.AdoptionAccepted || result.State != job.StateReady || len(result.SHA256) != 64 || result.CandidateID == 0 {
		t.Fatalf("supply_pdf result = %+v; want accepted and ready", result)
	}
}
