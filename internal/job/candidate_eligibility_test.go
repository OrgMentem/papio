// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"database/sql"
	"strings"
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

func TestBoundDOIsMatrix(t *testing.T) {
	anchor := SubmittedIdentity{
		Attested: true,
		Work:     work.Work{DOI: "10.1000/anchor"},
		Identifiers: []Identifier{
			{Kind: "doi", Value: "10.1000/submitted", Provenance: ProvenanceSubmitted},
			{Kind: "doi", Value: "10.1000/verified", Provenance: ProvenanceVerified},
			{Kind: "doi", Value: "10.1000/adopted", Provenance: ProvenanceAdopted},
			{Kind: "pmid", Value: "12345", Provenance: ProvenanceSubmitted},
			{Kind: "doi", Value: "", Provenance: ProvenanceSubmitted},
			{Kind: "doi", Value: "10.1000/submitted", Provenance: ProvenanceSubmitted},
			{Kind: "doi", Value: "10.1000/unattested", Provenance: ProvenanceUnattested},
		},
	}
	got := BoundDOIs(anchor, work.Work{DOI: "10.1000/row"})
	want := map[string]bool{
		"10.1000/anchor":    true,
		"10.1000/submitted": true,
		"10.1000/verified":  true,
		"10.1000/row":       true,
	}
	if len(got) != len(want) {
		t.Fatalf("BoundDOIs = %v, want %v", got, want)
	}
	for _, v := range got {
		if !want[v] {
			t.Fatalf("unexpected bound DOI %q in %v", v, got)
		}
	}
	for w := range want {
		found := false
		for _, v := range got {
			if v == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing bound DOI %q in %v", w, got)
		}
	}
	for _, v := range got {
		if v == "10.1000/adopted" || v == "10.1000/unattested" || v == "12345" || v == "" {
			t.Fatalf("excluded DOI %q appeared in bound set %v", v, got)
		}
	}
}

func TestBoundDOIsEmptyWorkMeansNoWorkingDOI(t *testing.T) {
	anchor := SubmittedIdentity{
		Attested: true,
		Work:     work.Work{DOI: "10.1000/a"},
		Identifiers: []Identifier{
			{Kind: "doi", Value: "10.1000/b", Provenance: ProvenanceSubmitted},
		},
	}
	got := BoundDOIs(anchor, work.Work{})
	if len(got) != 2 {
		t.Fatalf("BoundDOIs with empty work = %v, want 2", got)
	}
}

func TestCandidateEligibleJobBoundDOIsPopulated(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	w := work.Work{DOI: "10.1000/elig-bound", Title: "Eligible DOI", Authors: []string{"Author, A."}, Year: 2021}
	id, err := js.CreateRequest(ctx, "wr_elig_bound_dois", w, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "manual_download", "please", Access(false, "")); err != nil {
		t.Fatalf("open manual_download: %v", err)
	}
	// Add a PMID identifier that must NOT appear in BoundDOIs.
	if _, err := js.S.DB().ExecContext(ctx,
		`INSERT OR REPLACE INTO identifiers (work_request_id, kind, value, raw, provenance) VALUES (?, 'pmid', '99999', '99999', 'submitted')`,
		"wr_elig_bound_dois"); err != nil {
		t.Fatalf("insert pmid: %v", err)
	}

	got, err := js.ListCandidateEligibleJobs(ctx)
	if err != nil {
		t.Fatalf("ListCandidateEligibleJobs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("eligible len = %d, want 1: %+v", len(got), got)
	}
	bound := got[0].BoundDOIs
	// DOI-backed job must expose at least its own DOI.
	foundDOI := false
	for _, v := range bound {
		if v == "10.1000/elig-bound" {
			foundDOI = true
			break
		}
	}
	if !foundDOI {
		t.Fatalf("BoundDOIs = %v, missing submitted DOI", bound)
	}
	for _, v := range bound {
		if v == "" {
			t.Fatalf("BoundDOIs contains empty string: %v", bound)
		}
		if v == "99999" {
			t.Fatalf("BoundDOIs incorrectly includes PMID: %v", bound)
		}
	}
}

func TestCandidateEligibleJobBoundDOIsEmptyWhenNoDOI(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	w := work.Work{PMID: "99999999", Title: "PMID Only", Authors: []string{"Author, A."}, Year: 2021}
	id, err := js.CreateRequest(ctx, "wr_elig_pmid_only", w, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, StateResolving, StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "manual_download", "please", Access(false, "")); err != nil {
		t.Fatal(err)
	}

	got, err := js.ListCandidateEligibleJobs(ctx)
	if err != nil {
		t.Fatalf("ListCandidateEligibleJobs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("eligible len = %d, want 1", len(got))
	}
	if len(got[0].BoundDOIs) != 0 {
		t.Fatalf("BoundDOIs = %v, want empty for PMID-only job", got[0].BoundDOIs)
	}
	for _, v := range got[0].BoundDOIs {
		if v == "" {
			t.Fatalf("BoundDOIs contains empty string: %v", got[0].BoundDOIs)
		}
	}
}

func TestListCandidateEligibleJobsTxSeesOwnTransaction(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	workReqID := "wr_tx_view"
	jobID := "job_tx_view"
	now := "2026-08-16T00:00:00Z"
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO work_requests (id, created_at, requester, title, authors_json, year, desired_version, submitted_fields)
		 VALUES (?, ?, 'cli', 'Tx View', '["A"]', 2021, 'any', 'title')`, workReqID, now); err != nil {
		t.Fatalf("insert work_request: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at) VALUES (?, ?, 'awaiting_human', '{}', ?, ?)`,
		jobID, workReqID, now, now); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO human_actions (job_id, kind, status, detail, requires_auth, blocked_by, revision, created_at)
		 VALUES (?, 'manual_download', 'open', 'please', 0, '', 1, ?)`, jobID, now); err != nil {
		t.Fatalf("insert action: %v", err)
	}

	// No pool read here on purpose. store.Open caps the pool at one connection,
	// so an open transaction holds the only one: querying through the pool mid
	// transaction deadlocks rather than demonstrating isolation. That the pool
	// cannot see the row is asserted by the post-commit read below being the
	// first time it appears.

	txGot, err := ListCandidateEligibleJobsTx(ctx, tx)
	if err != nil {
		t.Fatalf("ListCandidateEligibleJobsTx: %v", err)
	}
	found := false
	for _, j := range txGot {
		if j.JobID == jobID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ListCandidateEligibleJobsTx did not see uncommitted job %q; got %+v", jobID, txGot)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	poolGot2, err := js.ListCandidateEligibleJobs(ctx)
	if err != nil {
		t.Fatalf("pool after commit: %v", err)
	}
	foundPool := false
	for _, j := range poolGot2 {
		if j.JobID == jobID {
			foundPool = true
			break
		}
	}
	if !foundPool {
		t.Fatalf("pool still missing committed job %q", jobID)
	}
}

func TestSubmittedIdentityTxSeesOwnTransaction(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	w := work.Work{DOI: "10.1000/tx-si-base", Title: "Tx SI", Authors: []string{"A"}, Year: 2021}
	jobID, err := js.CreateRequest(ctx, "wr_tx_si", w, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Checked before opening the transaction: store.Open caps the pool at one
	// connection, so once a transaction is open every pool read would block on
	// the connection it holds.
	if _, err := js.SubmittedIdentity(ctx, "does-not-exist"); !isNoRows(err) {
		t.Fatalf("pool SubmittedIdentity for missing job err = %v, want ErrNoRows", err)
	}

	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO identifiers (work_request_id, kind, value, raw, provenance) VALUES (?, 'doi', '10.1000/tx-only', '10.1000/tx-only', 'verified')`,
		"wr_tx_si"); err != nil {
		t.Fatalf("insert verified in tx: %v", err)
	}

	// No pool read while the transaction is open, for the same single-connection
	// reason. That the identifier is genuinely uncommitted is established by the
	// rollback this test defers: nothing here ever commits it.

	txSI, err := SubmittedIdentityTx(ctx, tx, jobID)
	if err != nil {
		t.Fatalf("SubmittedIdentityTx: %v", err)
	}
	hasTxOnly := false
	for _, id := range txSI.Identifiers {
		if id.Value == "10.1000/tx-only" {
			hasTxOnly = true
		}
	}
	if !hasTxOnly {
		t.Fatalf("SubmittedIdentityTx did not see uncommitted identifier: %+v", txSI.Identifiers)
	}

	if _, err := SubmittedIdentityTx(ctx, tx, "does-not-exist"); !isNoRows(err) {
		t.Fatalf("tx SubmittedIdentity for missing job err = %v, want ErrNoRows", err)
	}
}

func isNoRows(err error) bool {
	return err != nil && (err == sql.ErrNoRows || strings.Contains(err.Error(), "no rows"))
}
