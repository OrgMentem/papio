// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"testing"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/work"
)

// dismissResult is the actions.dismiss result body.
type dismissResult struct {
	JobID    string `json:"job_id"`
	ActionID int64  `json:"action_id"`
}

// terminalAdvisorySystem parks an openurl_available advisory on a job that has
// already reached a terminal state — the exact shape actions.dismiss exists
// for. The terminal transition deliberately spares that advisory and cancel
// refuses a terminal job, so dismissal is the only supported way out.
func terminalAdvisorySystem(t *testing.T, workKey string) (*bootstrap.System, string, job.HumanAction) {
	t.Helper()
	ctx := context.Background()
	system := testSystem(t)
	id, err := system.Jobs.CreateRequest(ctx, workKey, work.Work{DOI: "10.1000/" + workKey}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any",
	}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateUnavailable, nil,
		job.WithTerminalReason(job.TerminalReasonCandidatesExhausted)); err != nil {
		t.Fatal(err)
	}
	actionID, err := system.Jobs.OpenHumanAction(ctx, id, "openurl_available",
		"an institutional route exists; conservative mode will not take it", job.Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	return system, id, openActionByID(t, system, actionID)
}

// openActionByID reads one action straight from the store so a test asserts on
// persisted state rather than on the handler's own echo.
func openActionByID(t *testing.T, system *bootstrap.System, actionID int64) job.HumanAction {
	t.Helper()
	actions, err := system.Jobs.ListHumanActions(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.ID == actionID {
			return action
		}
	}
	t.Fatalf("human action %d missing from store: %+v", actionID, actions)
	return job.HumanAction{}
}

func TestDismissActionResolvesAdvisoryAndSparesTerminalJob(t *testing.T) {
	system, id, action := terminalAdvisorySystem(t, "wr_dismiss_advisory")
	router := Router(system)

	var result dismissResult
	if rpcErr := callMethod(t, router, "actions.dismiss", map[string]any{
		"action_id": action.ID, "expected_revision": action.Revision,
	}, &result); rpcErr != nil {
		t.Fatalf("dismiss = %+v", rpcErr)
	}
	if result.JobID != id || result.ActionID != action.ID {
		t.Fatalf("dismiss result = %+v, want job %s action %d", result, id, action.ID)
	}

	dismissed := openActionByID(t, system, action.ID)
	if dismissed.Status != "cancelled" {
		t.Fatalf("dismissed action status = %q, want cancelled", dismissed.Status)
	}
	open, err := system.Jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("open actions after dismiss = %+v, want none", open)
	}
	// The advisory's job is terminal and not parked on the action, so
	// dismissal must close the advisory without rewriting the job.
	row, err := system.Jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateUnavailable || row.TerminalReason != string(job.TerminalReasonCandidatesExhausted) {
		t.Fatalf("job after dismiss = state %q reason %q, want unavailable/%s",
			row.State, row.TerminalReason, job.TerminalReasonCandidatesExhausted)
	}
}

func TestDismissActionRevisionMismatchConflicts(t *testing.T) {
	system, _, action := terminalAdvisorySystem(t, "wr_dismiss_cas")
	router := Router(system)

	rpcErr := callMethod(t, router, "actions.dismiss", map[string]any{
		"action_id": action.ID, "expected_revision": action.Revision + 1,
	}, nil)
	if rpcErr == nil || rpcErr.Code != "conflict" {
		t.Fatalf("stale revision dismiss = %+v, want conflict", rpcErr)
	}
	// A lost compare-and-swap must leave the advisory exactly as it was:
	// reporting a conflict while closing the row is the failure mode here.
	unchanged := openActionByID(t, system, action.ID)
	if unchanged.Status != "open" || unchanged.Revision != action.Revision {
		t.Fatalf("action after failed CAS = %+v, want open at revision %d", unchanged, action.Revision)
	}

	// The matching revision still succeeds, so the conflict above was the CAS
	// verdict and not a broken fixture.
	var result dismissResult
	if rpcErr := callMethod(t, router, "actions.dismiss", map[string]any{
		"action_id": action.ID, "expected_revision": action.Revision,
	}, &result); rpcErr != nil {
		t.Fatalf("dismiss at current revision = %+v", rpcErr)
	}
	// Replay of an already-dismissed action is a conflict, not a silent
	// success: the action is no longer open at that revision.
	if rpcErr := callMethod(t, router, "actions.dismiss", map[string]any{
		"action_id": action.ID, "expected_revision": action.Revision,
	}, nil); rpcErr == nil || rpcErr.Code != "conflict" {
		t.Fatalf("replayed dismiss = %+v, want conflict", rpcErr)
	}
}

func TestDismissActionRejectsNonPositiveParams(t *testing.T) {
	router := Router(testSystem(t))
	for _, params := range []map[string]any{
		{"action_id": 0, "expected_revision": 1},
		{"action_id": -1, "expected_revision": 1},
		{"action_id": 1, "expected_revision": 0},
		{"action_id": 1, "expected_revision": -7},
		{},
	} {
		rpcErr := callMethod(t, router, "actions.dismiss", params, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("dismiss %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}

// TestDismissActionRejectsUnknownField pins the ratified decode contract: a
// misspelled or invented field is refused rather than silently ignored, so a
// caller never believes it dismissed at a revision the daemon never read.
func TestDismissActionRejectsUnknownField(t *testing.T) {
	router := Router(testSystem(t))
	rpcErr := callMethod(t, router, "actions.dismiss", map[string]any{
		"action_id": 1, "expected_revision": 1, "verdict": "dismiss",
	}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("dismiss with unknown field = %+v, want invalid_argument", rpcErr)
	}
}

func TestGetArtifactRequiresExactlyOneSelector(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	router := Router(testSystem(t))
	for _, params := range []map[string]any{
		{},
		{"job_id": "", "sha256": ""},
		{"job_id": "job-1", "sha256": sha},
	} {
		rpcErr := callMethod(t, router, "artifacts.get", params, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("artifacts.get %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
	// Exactly one selector passes the XOR gate and reaches the lookup, which
	// answers not_found. An inverted gate would report invalid_argument here.
	for _, params := range []map[string]any{
		{"sha256": sha},
		{"job_id": "job-does-not-exist"},
	} {
		rpcErr := callMethod(t, router, "artifacts.get", params, nil)
		if rpcErr == nil || rpcErr.Code != "not_found" {
			t.Fatalf("artifacts.get %v = %+v, want not_found", params, rpcErr)
		}
	}
	// A job that exists but holds no validated artifact is its own answer, and
	// it must say so rather than reporting a missing record.
	system, id, _ := terminalAdvisorySystem(t, "wr_artifact_none")
	rpcErr := callMethod(t, Router(system), "artifacts.get", map[string]any{"job_id": id}, nil)
	if rpcErr == nil || rpcErr.Code != "not_found" || rpcErr.Message != "job has no validated artifact" {
		t.Fatalf("artifacts.get for job without artifact = %+v, want not_found/job has no validated artifact", rpcErr)
	}
}

func TestExportBundleReportsMissingJobAndUnreadyJob(t *testing.T) {
	system, id, _ := terminalAdvisorySystem(t, "wr_export_no_bundle")
	router := Router(system)
	output := t.TempDir()

	rpcErr := callMethod(t, router, "bundle.export", map[string]any{
		"job_id": "job-does-not-exist", "output_dir": output,
	}, nil)
	if rpcErr == nil || rpcErr.Code != "not_found" {
		t.Fatalf("export of unknown job = %+v, want not_found", rpcErr)
	}

	// A job that exists but never produced an artifact is a routine conflict,
	// not a missing record: collapsing the two would hide the reason from a
	// consumer polling for readiness.
	rpcErr = callMethod(t, router, "bundle.export", map[string]any{
		"job_id": id, "output_dir": output,
	}, nil)
	if rpcErr == nil || rpcErr.Code != "conflict" {
		t.Fatalf("export of job without bundle = %+v, want conflict", rpcErr)
	}

	for _, params := range []map[string]any{
		{"job_id": id},
		{"output_dir": output},
		{},
	} {
		if rpcErr := callMethod(t, router, "bundle.export", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("export %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}
