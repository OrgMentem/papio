// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"testing"
	"time"

	"papio/internal/work"
)

func TestListCandidateEligibleJobsEligibleReturnedWithWork(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	id, err := js.CreateRequest(ctx, "wr_elig_ok", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("to resolving: %v", err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatalf("to awaiting_human: %v", err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "manual_download", "please download", Access(false, "")); err != nil {
		t.Fatalf("open manual_download: %v", err)
	}

	got, err := js.ListCandidateEligibleJobs(ctx)
	if err != nil {
		t.Fatalf("ListCandidateEligibleJobs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("eligible = %v, want 1", got)
	}
	if got[0].JobID != id {
		t.Fatalf("JobID = %q, want %q", got[0].JobID, id)
	}
	// Work hydration must survive the join — title and at least one strong
	// identifier round-trip through work_requests + identifiers.
	if got[0].Work.Title != testWork().Title {
		t.Fatalf("Work.Title = %q, want %q", got[0].Work.Title, testWork().Title)
	}
	if got[0].Work.DOI != testWork().DOI {
		t.Fatalf("Work.DOI = %q, want %q", got[0].Work.DOI, testWork().DOI)
	}
}

func TestListCandidateEligibleJobsExcludesDifferentKind(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	id, err := js.CreateRequest(ctx, "wr_elig_wrong_kind", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, ActionKindDocumentDelivery, "delivery pending", Access(false, "")); err != nil {
		t.Fatalf("open document_delivery: %v", err)
	}

	got, err := js.ListCandidateEligibleJobs(ctx)
	if err != nil {
		t.Fatalf("ListCandidateEligibleJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("eligible = %+v, want empty (document_delivery must not qualify)", got)
	}
}

func TestListCandidateEligibleJobsExcludesClosedAction(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	id, err := js.CreateRequest(ctx, "wr_elig_closed", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	actionID, err := js.OpenHumanAction(ctx, id, "manual_download", "please download", Access(false, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := js.ResolveHumanAction(ctx, actionID, "resolved"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got, err := js.ListCandidateEligibleJobs(ctx)
	if err != nil {
		t.Fatalf("ListCandidateEligibleJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("eligible = %+v, want empty (resolved action must not qualify)", got)
	}
}

func TestListCandidateEligibleJobsExcludesWrongState(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	// Live but not awaiting_human — fetching with an open manual_download.
	fetchID, err := js.CreateRequest(ctx, "wr_elig_fetching", work.Work{DOI: "10.1002/fetching", Title: "Fetching"}, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create fetching: %v", err)
	}
	if err := js.Transition(ctx, fetchID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, fetchID, StateResolving, StateFetching, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, fetchID, "manual_download", "pending", Access(false, "")); err != nil {
		t.Fatal(err)
	}

	// Terminal — ready with an open manual_download created after the
	// terminal transition so the closeTerminalHumanActions sweep has nothing
	// to close, but the state itself must still exclude the job.
	readyID, err := js.CreateRequest(ctx, "wr_elig_ready", work.Work{DOI: "10.1002/ready", Title: "Ready"}, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create ready: %v", err)
	}
	if err := js.Transition(ctx, readyID, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, readyID, StateResolving, StateReady, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, readyID, "manual_download", "pending", Access(false, "")); err != nil {
		t.Fatal(err)
	}

	got, err := js.ListCandidateEligibleJobs(ctx)
	if err != nil {
		t.Fatalf("ListCandidateEligibleJobs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("eligible = %+v, want empty (fetching and ready must not qualify)", got)
	}
}

func TestListCandidateEligibleJobsDeterministicOrder(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	idA, err := js.CreateRequest(ctx, "wr_elig_order_a", work.Work{DOI: "10.1002/order-a", Title: "Order A"}, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := js.Transition(ctx, idA, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, idA, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, idA, "manual_download", "pending a", Access(false, "")); err != nil {
		t.Fatal(err)
	}

	idB, err := js.CreateRequest(ctx, "wr_elig_order_b", work.Work{DOI: "10.1002/order-b", Title: "Order B"}, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if err := js.Transition(ctx, idB, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, idB, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, idB, "manual_download", "pending b", Access(false, "")); err != nil {
		t.Fatal(err)
	}

	// Force deterministic created_at values so the ordering does not depend
	// on wall-clock granularity or insertion order.
	older := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	newer := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE human_actions SET created_at = ? WHERE job_id = ?`, older, idA); err != nil {
		t.Fatalf("backdate a: %v", err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE human_actions SET created_at = ? WHERE job_id = ?`, newer, idB); err != nil {
		t.Fatalf("backdate b: %v", err)
	}

	got, err := js.ListCandidateEligibleJobs(ctx)
	if err != nil {
		t.Fatalf("ListCandidateEligibleJobs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("eligible len = %d, want 2: %+v", len(got), got)
	}
	if got[0].JobID != idA || got[1].JobID != idB {
		t.Fatalf("order = [%q %q], want [%q %q] (oldest action first)", got[0].JobID, got[1].JobID, idA, idB)
	}
}

func TestListCandidateEligibleJobsDeduplicatesDuplicateActions(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	id, err := js.CreateRequest(ctx, "wr_elig_dedup", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "manual_download", "please download", Access(false, "")); err != nil {
		t.Fatal(err)
	}
	// OpenHumanAction upserts on (job_id, kind, open), so a second
	// manual_download via the API would collapse. Insert a second open row
	// directly to emulate the "somehow has two" case.
	if _, err := js.S.DB().ExecContext(ctx,
		`INSERT INTO human_actions (job_id, kind, status, detail, requires_auth, blocked_by, revision, created_at)
		 VALUES (?, 'manual_download', 'open', 'dup', 0, '', 1, ?)`,
		id, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert duplicate action: %v", err)
	}

	got, err := js.ListCandidateEligibleJobs(ctx)
	if err != nil {
		t.Fatalf("ListCandidateEligibleJobs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("eligible = %+v, want exactly one row for job %q", got, id)
	}
	if got[0].JobID != id {
		t.Fatalf("JobID = %q, want %q", got[0].JobID, id)
	}
}
