// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"
	"testing"

	"papio/internal/batch"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

// A job a real store transitioned to StateImported reports its Zotero keys from
// the ready->imported transition event, with no zotio.auto_import event.
func TestRouterAcquireReportReadsKeysFromImportedTransition(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	const requestID = "batch-dededededededededededededededede-22222222222222222222222222222222"
	id, err := system.Jobs.CreateRequest(ctx, requestID, work.Work{DOI: "10.1000/imported"}, "", "Reading", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any", Collection: "Reading",
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateReady}} {
		if err := system.Jobs.Transition(ctx, id, step[0], step[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := system.Jobs.Transition(ctx, id, job.StateReady, job.StateImported, map[string]any{
		"parent_key": "PI123", "attachment_key": "AI456",
	}); err != nil {
		t.Fatal(err)
	}
	manifest := &batch.Manifest{
		SchemaVersion: batch.SchemaVersion, ID: "batch-dededededededededededededededede", CreatedAt: "2026-07-15T12:00:00Z", Collection: "Reading",
		Works: []batch.ManifestWork{{
			RequestID: requestID, JobID: id, Status: "submitted",
			Work: protocol.WorkRequest{SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: requestID, Identifiers: &protocol.Identifiers{DOI: "10.1000/imported"}, Collection: "Reading"},
		}},
	}
	if err := batch.Write(system.Config.DataDir, manifest); err != nil {
		t.Fatal(err)
	}
	var report batch.Report
	if rpcErr := callMethod(t, Router(system), "acquire.report", AcquireReportParams{BatchID: manifest.ID}, &report); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(report.Works) != 1 || report.Works[0].Outcome != batch.OutcomeImported || report.Works[0].ParentKey != "PI123" || report.Works[0].AttachmentKey != "AI456" || !report.Settled() {
		t.Fatalf("report = %+v", report)
	}
}
