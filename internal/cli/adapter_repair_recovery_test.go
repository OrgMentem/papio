// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/job"
	"papio/internal/work"
)

const recoveryArtifactSHA = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

type historyEvent struct {
	kind   string
	detail map[string]any
}

// jobDetailOverIPC numbers the events and round-trips them through JSON, so
// the code under test sees the float64 sequence numbers the daemon's IPC
// decoder produces rather than Go integers.
func jobDetailOverIPC(t *testing.T, row job.Row, history []historyEvent) api.JobDetail {
	t.Helper()
	events := make([]map[string]any, 0, len(history))
	for i, event := range history {
		events = append(events, map[string]any{"seq": i + 1, "kind": event.kind, "at": "2026-09-20T12:00:00Z", "detail": event.detail})
	}
	data, err := json.Marshal(api.JobDetail{Job: &row, Events: events, Actions: []job.HumanAction{}})
	if err != nil {
		t.Fatal(err)
	}
	var detail api.JobDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatal(err)
	}
	return detail
}

func recoveryHistory(capturePath string) []historyEvent {
	return []historyEvent{
		{"job.created", map[string]any{"work": "doi:10.2307/repair"}},
		{"browser.page_capture", map[string]any{"adapter_id": "jstor", "adapter_version": "0.3.0", "path": capturePath, "scenario": "success"}},
		{"browser.provider_outcome", map[string]any{"outcome": "ui_changed", "adapter_id": "jstor", "adapter_version": "0.3.0"}},
		{"handoff.opened", map[string]any{"principal": "cli"}},
		{"browser.agent_decision_requested", map[string]any{"permit_id": "p1", "request_id": "r1"}},
		{"browser.agent_decision_completed", map[string]any{"permit_id": "p1", "request_id": "r1", "outcome": "decision", "choice": "c1"}},
		{"browser.download_started", map[string]any{"download_id": 7}},
		{"job.transition", map[string]any{"from": "awaiting_human", "to": "validating", "reason": "adopt_browser_download"}},
		{"job.transition", map[string]any{"from": "validating", "to": "ready", "sha256": recoveryArtifactSHA}},
	}
}

func recoveryRow(id string) job.Row {
	return job.Row{ID: id, State: job.StateReady, ArtifactSHA256: recoveryArtifactSHA, CreatedAt: "2026-09-20T12:00:00Z", Work: work.Work{DOI: "10.2307/REPAIR"}}
}

func passingArtifact() *job.Artifact {
	return &job.Artifact{SHA256: recoveryArtifactSHA, SizeBytes: 4096, MIME: "application/pdf", PageCount: 5, IdentityResult: "pass"}
}

func TestLinkAdapterRepairRecoveryRequiresEveryDurableLink(t *testing.T) {
	capture := adapterRepairCapture{Path: "/data/captures/www.jstor.org/2026-09-20T12:00:00Z-success.html", Provider: "jstor", Scenario: "success", AdapterVersion: "0.3.0"}
	without := func(kinds ...string) func([]historyEvent) []historyEvent {
		return func(history []historyEvent) []historyEvent {
			out := history[:0:0]
			for _, event := range history {
				keep := true
				for _, kind := range kinds {
					keep = keep && event.kind != kind
				}
				if keep {
					out = append(out, event)
				}
			}
			return out
		}
	}
	for _, tc := range []struct {
		name     string
		history  func([]historyEvent) []historyEvent
		row      func(*job.Row)
		artifact func(*job.Artifact)
		wantErr  string
		route    string
	}{
		{name: "agent recovery", route: "agent_decision"},
		{name: "unattributed browser delivery", history: without("browser.agent_decision_requested", "browser.agent_decision_completed"), route: "unattributed"},
		{name: "capture from another job", history: func(h []historyEvent) []historyEvent {
			h[1].detail = map[string]any{"adapter_id": "jstor", "adapter_version": "0.3.0", "path": "/data/captures/other.html", "scenario": "success"}
			return h
		}, wantErr: "did not record this capture"},
		{name: "capture relabelled", history: func(h []historyEvent) []historyEvent {
			h[1].detail["adapter_version"] = "0.2.9"
			return h
		}, wantErr: "different adapter or scenario"},
		{name: "no declarative failure", history: without("browser.provider_outcome", "browser.agent_decision_requested", "browser.agent_decision_completed"), wantErr: "no ui_changed outcome or agent fallback"},
		{name: "ready from resolver fetch", history: without("browser.download_started", "browser.agent_decision_requested", "browser.agent_decision_completed"), wantErr: "without a browser delivery"},
		{name: "identity only reviewed", artifact: func(a *job.Artifact) { a.IdentityResult = "review" }, wantErr: "identity result"},
		{name: "artifact row for other bytes", artifact: func(a *job.Artifact) { a.SHA256 = strings.Repeat("b", 64) }, wantErr: "accepted artifact digest"},
		{name: "not ready", row: func(r *job.Row) { r.State = job.StateAwaitingHuman }, wantErr: "not ready"},
		{name: "title-only work", row: func(r *job.Row) { r.Work.DOI = "" }, wantErr: "DOI-identified"},
		{name: "DOI with a control byte", row: func(r *job.Row) { r.Work.DOI = "10.2307/rep\x07air" }, wantErr: "DOI-identified"},
		{name: "identity accepted by a person", history: func(h []historyEvent) []historyEvent {
			h[8].detail["reason"] = "human_identity_override"
			return h
		}, wantErr: "human identity override"},
		{name: "imported after ready", row: func(r *job.Row) { r.State = job.StateImported }, route: "agent_decision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := recoveryHistory(capture.Path)
			if tc.history != nil {
				history = tc.history(history)
			}
			row := recoveryRow("job_recovered")
			if tc.row != nil {
				tc.row(&row)
			}
			artifact := passingArtifact()
			if tc.artifact != nil {
				tc.artifact(artifact)
			}
			got, err := linkAdapterRepairRecovery(capture, "job_recovered", jobDetailOverIPC(t, row, history), artifact)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Route != tc.route || got.WorkDOI != "10.2307/repair" || got.Delivery != repairDeliveryBrowser ||
				got.CaptureSeq != 2 || got.FailureSeq != 3 || got.Artifact.PageCount != 5 || got.ExplicitOpens != 1 {
				t.Fatalf("recovery = %+v", got)
			}
		})
	}
}

func TestScaffoldAdapterRepairLabelsRegressionWithRecoveredWork(t *testing.T) {
	root := t.TempDir()
	writeAdapterRepairTestRepo(t, root)
	row, _ := storeAdapterRepairTestCapture(t, root, true)
	capture := adapterRepairCapture{
		Path: row.Path, Provider: row.AdapterID, Scenario: row.Scenario,
		Host: row.Host, Captured: row.Timestamp, AdapterVersion: row.AdapterVersion,
		SHA256: row.SHA256, SanitizerProvenance: row.SanitizerProvenance,
		SanitizerVersion: row.SanitizerVersion, IndependentEvidence: row.IndependentEvidence,
	}
	recovery, err := linkAdapterRepairRecovery(capture, "job_recovered", jobDetailOverIPC(t, recoveryRow("job_recovered"), recoveryHistory(row.Path)), passingArtifact())
	if err != nil {
		t.Fatal(err)
	}
	capture.Recovery = &recovery
	deps := adapterRepairDeps{
		RepoRoot: root,
		Now:      func() time.Time { return time.Date(2026, 9, 20, 13, 0, 0, 0, time.UTC) },
		Run: adapterRepairRunnerFunc(func(_ context.Context, _ string, tool string, _ ...string) (string, error) {
			if tool == "tools/adapter-repair.ts" {
				return completeAdapterRepairOutput(t, root), nil
			}
			return "adapter-try found a missing selector", nil
		}),
	}
	result, err := scaffoldAdapterRepair(context.Background(), capture, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "proposal" || result.RecoveryJob != "job_recovered" {
		t.Fatalf("result = %+v", result)
	}
	testPatch, err := os.ReadFile(filepath.Join(result.Workspace, "adapters.test.ts.patch"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(testPatch), `planExecution(page, spec, { doi: "10.2307/repair" }, {})`) ||
		!strings.Contains(string(testPatch), `{ doi: "`+adapterRepairOtherDOI+`" }`) ||
		strings.Contains(string(testPatch), "const expectedWork = evidence.kind") {
		t.Fatalf("generated regression is not labelled by the recovered work: %s", testPatch)
	}
	var manifest adapterRepairManifest
	if err := json.Unmarshal(mustReadRepairFile(t, result.Manifest), &manifest); err != nil {
		t.Fatal(err)
	}
	if next, _ := nextAdapterRevision(manifest.CurrentVersion); manifest.FixtureSHA256 != row.SHA256 ||
		manifest.Recovery.Artifact.SHA256 != recoveryArtifactSHA || manifest.NextRevision != next {
		t.Fatalf("manifest confuses the capture and the artifact or the revisions: %+v", manifest)
	}
	canary, err := os.ReadFile(filepath.Join(result.Workspace, "canary.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canary), "PAPIO_TYPESAFE_API_KEY= papio --config <canary-config> doctor") ||
		!strings.Contains(string(canary), "acquire --doi '10.2307/repair'") {
		t.Fatalf("canary.md does not disable the agent or submit the recovered work: %s", canary)
	}

	// A validated PDF labels an article page only.
	terms := capture
	terms.Scenario = "terms"
	before, _ := os.ReadDir(filepath.Join(root, "dev", "scratch", "repair"))
	if _, err := scaffoldAdapterRepair(context.Background(), terms, deps); err == nil {
		t.Fatal("recovery labelled a terms repair")
	}
	if after, _ := os.ReadDir(filepath.Join(root, "dev", "scratch", "repair")); len(after) != len(before) {
		t.Fatal("refused recovery created a workspace")
	}
}

// The integrated agent path reports no provider outcome, so its automatic
// capture is never marked independent; the recovery link supplies the
// correlation. Without either, promotion stays locked.
func TestScaffoldAdapterRepairAcceptsAgentPathRecoveryAsCorrelation(t *testing.T) {
	root := t.TempDir()
	writeAdapterRepairTestRepo(t, root)
	row, _ := storeAdapterRepairTestCapture(t, root, false)
	capture := adapterRepairCapture{
		Path: row.Path, Provider: row.AdapterID, Scenario: row.Scenario,
		Host: row.Host, Captured: row.Timestamp, AdapterVersion: row.AdapterVersion,
		SHA256: row.SHA256, SanitizerProvenance: row.SanitizerProvenance,
		SanitizerVersion: row.SanitizerVersion, IndependentEvidence: row.IndependentEvidence,
	}
	history := recoveryHistory(row.Path)
	history = append(history[:2:2], history[3:]...) // agent fallback instead of a ui_changed report
	recovery, err := linkAdapterRepairRecovery(capture, "job_recovered", jobDetailOverIPC(t, recoveryRow("job_recovered"), history), passingArtifact())
	if err != nil {
		t.Fatal(err)
	}
	if recovery.FailureOutcome != "agent_fallback" || recovery.Route != "agent_decision" || recovery.AgentDecisions != 1 {
		t.Fatalf("recovery = %+v", recovery)
	}
	capture.Recovery = &recovery
	result, err := scaffoldAdapterRepair(context.Background(), capture, adapterRepairDeps{
		RepoRoot: root,
		Now:      func() time.Time { return time.Date(2026, 9, 20, 13, 0, 0, 0, time.UTC) },
		Run: adapterRepairRunnerFunc(func(_ context.Context, _ string, tool string, _ ...string) (string, error) {
			if tool == "tools/adapter-repair.ts" {
				return completeAdapterRepairOutput(t, root), nil
			}
			return "adapter-try found a missing selector", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "proposal" || result.IndependentEvidence {
		t.Fatalf("result = %+v; want a proposal that still reports the capture row as caller-labelled", result)
	}
}

// fixedRPCOptions answers each daemon method from a per-job table and fails on
// any job or method the caller did not declare.
func fixedRPCOptions(t *testing.T, details map[string]api.JobDetail, artifacts map[string]*job.Artifact) *options {
	t.Helper()
	call := func(_ context.Context, method string, params, result any) error {
		id := params.(map[string]string)["job_id"]
		var value any
		switch method {
		case "jobs.get":
			detail, ok := details[id]
			if !ok {
				t.Fatalf("unexpected jobs.get %q", id)
			}
			value = detail
		case "artifacts.get":
			artifact, ok := artifacts[id]
			if !ok {
				t.Fatalf("unexpected artifacts.get %q", id)
			}
			value = api.ArtifactResult{Artifact: artifact}
		default:
			t.Fatalf("unexpected method %q", method)
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, result)
	}
	return &options{
		daemonVersionChecked: true,
		configLoader:         func(string) (config.Config, error) { return config.Config{DataDir: t.TempDir()}, nil },
		newAutostarter: func(socket string) *daemon.Autostarter {
			return &daemon.Autostarter{SocketPath: socket, Ready: func(context.Context, string) error { return nil }}
		},
		rpcCall: func(ctx context.Context, _ string, method string, params, result any) error {
			return call(ctx, method, params, result)
		},
	}
}

// A decision reservation is written before inference, so on its own it only
// orders the failure before ready. Without the same request's completion the
// link is temporal correlation: it still labels the regression, but it must
// not stand in for daemon correlation at the revision gate.
func TestAgentReservationWithoutCompletionIsTemporalCorrelation(t *testing.T) {
	root := t.TempDir()
	writeAdapterRepairTestRepo(t, root)
	row, _ := storeAdapterRepairTestCapture(t, root, false)
	capture := adapterRepairCapture{
		Path: row.Path, Provider: row.AdapterID, Scenario: row.Scenario,
		Host: row.Host, Captured: row.Timestamp, AdapterVersion: row.AdapterVersion,
		SHA256: row.SHA256, SanitizerProvenance: row.SanitizerProvenance,
		SanitizerVersion: row.SanitizerVersion, IndependentEvidence: row.IndependentEvidence,
	}
	agentPath := func() []historyEvent {
		history := recoveryHistory(row.Path)
		return append(history[:2:2], history[3:]...) // no ui_changed report
	}
	for _, tc := range []struct {
		name    string
		history func() []historyEvent
		route   string
	}{
		{name: "completed decision", history: agentPath, route: "agent_decision"},
		{name: "reservation alone", history: func() []historyEvent {
			history := agentPath()
			return append(history[:4:4], history[5:]...)
		}, route: repairRouteTemporal},
		{name: "completion of another request", history: func() []historyEvent {
			history := agentPath()
			history[4].detail = map[string]any{"permit_id": "p1", "request_id": "r0", "outcome": "decision", "choice": "c1"}
			return history
		}, route: repairRouteTemporal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recovery, err := linkAdapterRepairRecovery(capture, "job_recovered", jobDetailOverIPC(t, recoveryRow("job_recovered"), tc.history()), passingArtifact())
			if err != nil {
				t.Fatal(err)
			}
			if recovery.Route != tc.route || recovery.FailureOutcome != "agent_fallback" {
				t.Fatalf("recovery = %+v, want route %s", recovery, tc.route)
			}
			withRecovery := capture
			withRecovery.Recovery = &recovery
			result, err := scaffoldAdapterRepair(context.Background(), withRecovery, adapterRepairDeps{
				RepoRoot: root,
				Now:      func() time.Time { return time.Date(2026, 9, 20, 13, 0, 0, 0, time.UTC) },
				Run: adapterRepairRunnerFunc(func(_ context.Context, _ string, tool string, _ ...string) (string, error) {
					if tool == "tools/adapter-repair.ts" {
						return completeAdapterRepairOutput(t, root), nil
					}
					return "adapter-try found a missing selector", nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			wantOutcome := "proposal"
			if tc.route == repairRouteTemporal {
				wantOutcome = "blocked"
			}
			if result.Outcome != wantOutcome {
				t.Fatalf("outcome = %s, want %s", result.Outcome, wantOutcome)
			}
			var manifest adapterRepairManifest
			if err := json.Unmarshal(mustReadRepairFile(t, result.Manifest), &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Recovery == nil || manifest.Recovery.Route != tc.route {
				t.Fatalf("repair.json recovery = %+v", manifest.Recovery)
			}
			_, typesErr := os.Stat(filepath.Join(result.Workspace, "types.ts.patch"))
			_, canaryErr := os.Stat(filepath.Join(result.Workspace, "canary.md"))
			if tc.route == repairRouteTemporal && (!os.IsNotExist(typesErr) || !os.IsNotExist(canaryErr)) {
				t.Fatalf("temporal link unlocked the revision: types=%v canary=%v", typesErr, canaryErr)
			}
		})
	}
}

func mustReadRepairFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
