// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package triage

import (
	"context"
	"encoding/json"
	"testing"

	"papio/internal/job"
	"papio/internal/protocol"
)

func TestNativeViewerDiagnosisFactProjectionAndWire(t *testing.T) {
	service, _, jobs := triageTestService(t)
	wanted := make(map[string]bool)
	for _, tc := range []struct {
		name, kind, diagnosis string
		want                  bool
	}{
		{"native", "manual_download", job.DiagnosisReasonNativeViewerDownload, true},
		{"other-reason", "manual_download", job.DiagnosisReasonLandingPageOnly, false},
		{"legacy", "manual_download", "", false},
		{"other-action", "openurl_handoff", job.DiagnosisReasonNativeViewerDownload, false},
	} {
		var opts []job.OpenHumanActionOption
		if tc.diagnosis != "" {
			opts = append(opts, job.WithHumanActionDiagnosis(tc.diagnosis))
		}
		// All controls claim the native reason in prose. Only the durable diagnosis
		// on the correct action kind may grant the automatic continuation.
		id := createProjectionAction(t, jobs, "native-fact-"+tc.name, tc.kind,
			job.DiagnosisReasonNativeViewerDownload, job.Access(false, "landing_page"), opts...)
		wanted[id] = tc.want
	}
	for _, schema := range []int{2, 5} {
		snapshot, err := service.Snapshot(context.Background(), SnapshotRequest{Limit: 100, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Items) != len(wanted) {
			t.Fatalf("schema %d: items=%d", schema, len(snapshot.Items))
		}
		for _, item := range snapshot.Items {
			count := 0
			for _, fact := range item.Facts {
				if fact.Label == "Diagnosis" {
					count++
					if fact.Text != job.DiagnosisReasonNativeViewerDownload {
						t.Fatalf("unexpected diagnosis %q", fact.Text)
					}
				}
			}
			want := 0
			if wanted[item.HumanAction.JobID] {
				want = 1
			}
			if count != want {
				t.Fatalf("schema %d job %s: diagnosis count=%d want=%d", schema, item.HumanAction.JobID, count, want)
			}
		}
		if schema != 2 {
			continue
		}
		// Marshal the real projected items into the existing legacy browser shape.
		// Its strict parser proves the additive fact needs no new wire field.
		rawItems, err := json.Marshal(snapshot.Items)
		if err != nil {
			t.Fatal(err)
		}
		var items []protocol.TriageSnapshotItem
		if err := json.Unmarshal(rawItems, &items); err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(protocol.BrowserMessage{
			Protocol: "papio-browser/1", Type: protocol.MsgTriageSnapshotResponse, MsgID: "native-fact-frame", Seq: 1,
			Payload: protocol.TriageSnapshotResponsePayload{RequestID: "native-fact-request", Schema: 2,
				GeneratedAt: snapshot.GeneratedAt, Counts: protocol.TriageCounts{
					PendingTotal: int64(snapshot.Counts.PendingTotal), Actions: int64(snapshot.Counts.Actions),
					JobsWorking: int64(snapshot.Counts.JobsWorking), JobsNeedsReview: int64(snapshot.Counts.JobsNeedsReview),
				}, Items: items},
		})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := protocol.DecodeBrowserMessage(wire)
		if err != nil {
			t.Fatalf("projected native diagnosis failed browser parser: %v", err)
		}
		response := parsed.Payload.(*protocol.TriageSnapshotResponsePayload)
		for _, item := range response.Items {
			found := false
			for _, fact := range item.Facts {
				if fact.Label == "Diagnosis" && fact.Text == job.DiagnosisReasonNativeViewerDownload {
					found = true
				}
			}
			if found != wanted[item.JobID] {
				t.Fatalf("wire diagnosis for %s=%v", item.JobID, found)
			}
		}
	}
}
