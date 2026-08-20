// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIssueCloseAuthorizationIsIdempotentPerBinding(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	id1, nonce1, err := js.IssueCloseAuthorization(ctx, "binding-close-1", 5, "scaffold_idle", now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if id1 == "" || nonce1 == "" {
		t.Fatalf("issue returned empty id/nonce: %q %q", id1, nonce1)
	}

	// A repeated authorized request for the same live binding at the same
	// generation must return the exact same token, not mint a second one.
	id2, nonce2, err := js.IssueCloseAuthorization(ctx, "binding-close-1", 5, "scaffold_idle", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	if id2 != id1 || nonce2 != nonce1 {
		t.Fatalf("re-issue minted a new token: got (%q,%q), want (%q,%q)", id2, nonce2, id1, nonce1)
	}

	var count int
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM close_authorizations WHERE binding_id = ?`, "binding-close-1",
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("close_authorizations rows for binding = %d, want 1", count)
	}
}

// TestIssueCloseAuthorizationAcceptsJobInactive pins both copies of the
// closed vocabulary: the Go admission map and migration 0044's SQLite CHECK.
func TestIssueCloseAuthorizationAcceptsJobInactive(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, nonce, err := js.IssueCloseAuthorization(ctx, "binding-job-inactive", 7, "job_inactive", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || nonce == "" {
		t.Fatalf("job_inactive issue returned empty id/nonce: %q %q", id, nonce)
	}
	var disposition string
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT disposition FROM close_authorizations WHERE id=?`, id,
	).Scan(&disposition); err != nil {
		t.Fatal(err)
	}
	if disposition != "job_inactive" {
		t.Fatalf("stored disposition = %q, want job_inactive", disposition)
	}
}

func TestIssueCloseAuthorizationRejectsDispositionConflict(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	id, nonce, err := js.IssueCloseAuthorization(ctx, "binding-close-conflict", 1, "scaffold_idle", now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// A live token exists with a different disposition than this request:
	// the store must refuse to silently repurpose it.
	_, _, err = js.IssueCloseAuthorization(ctx, "binding-close-conflict", 1, "materialization_settled", now)
	if !errors.Is(err, ErrCloseAuthorizationConflict) {
		t.Fatalf("re-issue with a different disposition = %v, want ErrCloseAuthorizationConflict", err)
	}

	// The original live token is untouched by the rejected request.
	var gotID, gotNonce, gotDisposition string
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT id, nonce, disposition FROM close_authorizations WHERE binding_id = ? AND status = 'issued'`,
		"binding-close-conflict",
	).Scan(&gotID, &gotNonce, &gotDisposition); err != nil {
		t.Fatal(err)
	}
	if gotID != id || gotNonce != nonce || gotDisposition != "scaffold_idle" {
		t.Fatalf("live token mutated by rejected conflict: got (%q,%q,%q), want (%q,%q,scaffold_idle)", gotID, gotNonce, gotDisposition, id, nonce)
	}
}

func TestIssueCloseAuthorizationReStampsHigherGeneration(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	id, _, err := js.IssueCloseAuthorization(ctx, "binding-close-gen", 3, "claim_abandoned", now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// A repeat with a strictly higher generation re-stamps the live row
	// rather than minting a second token.
	id2, _, err := js.IssueCloseAuthorization(ctx, "binding-close-gen", 7, "claim_abandoned", now)
	if err != nil {
		t.Fatalf("re-issue at higher generation: %v", err)
	}
	if id2 != id {
		t.Fatalf("higher-generation re-issue minted a new token: got %q, want %q", id2, id)
	}
	var generation int64
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT browser_holder_generation FROM close_authorizations WHERE id = ?`, id,
	).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 7 {
		t.Fatalf("browser_holder_generation = %d, want 7 (re-stamped)", generation)
	}

	// A stale (lower) generation on the same live token is left untouched:
	// refusing it is the caller's job, not this store method's.
	id3, _, err := js.IssueCloseAuthorization(ctx, "binding-close-gen", 1, "claim_abandoned", now)
	if err != nil {
		t.Fatalf("re-issue at lower generation: %v", err)
	}
	if id3 != id {
		t.Fatalf("lower-generation re-issue minted a new token: got %q, want %q", id3, id)
	}
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT browser_holder_generation FROM close_authorizations WHERE id = ?`, id,
	).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 7 {
		t.Fatalf("browser_holder_generation = %d after stale re-issue, want unchanged 7", generation)
	}
}

func TestCloseAuthorizationsLiveBindingIndexRejectsSecondLiveToken(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO close_authorizations
		  (id, binding_id, browser_holder_generation, nonce, disposition, status, issued_at)
		VALUES ('close-idx-1', 'binding-close-idx', 1, 'nonce-idx-1', 'scaffold_idle', 'issued', ?)`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed first live token: %v", err)
	}

	// A second row for the same binding while the first is still 'issued'
	// must violate close_authorizations_live_binding.
	_, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO close_authorizations
		  (id, binding_id, browser_holder_generation, nonce, disposition, status, issued_at)
		VALUES ('close-idx-2', 'binding-close-idx', 1, 'nonce-idx-2', 'scaffold_idle', 'issued', ?)`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err == nil {
		t.Fatal("second live token for the same binding was accepted; want a unique-index violation")
	}

	// Once the first token is consumed, a second live token for the same
	// binding is permitted again — the index only forbids two *live* rows.
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE close_authorizations SET status = 'consumed', consumed_at = ? WHERE id = 'close-idx-1'`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("consume first token: %v", err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO close_authorizations
		  (id, binding_id, browser_holder_generation, nonce, disposition, status, issued_at)
		VALUES ('close-idx-3', 'binding-close-idx', 2, 'nonce-idx-3', 'scaffold_idle', 'issued', ?)`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("insert live token after prior consumed: %v", err)
	}
}

func TestExpireCloseAuthorizationsSweepsOnlyOldIssuedTokens(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	old, _, err := js.IssueCloseAuthorization(ctx, "binding-close-old", 1, "materialization_settled", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("issue old: %v", err)
	}
	fresh, _, err := js.IssueCloseAuthorization(ctx, "binding-close-fresh", 1, "materialization_settled", now)
	if err != nil {
		t.Fatalf("issue fresh: %v", err)
	}
	consumed, _, err := js.IssueCloseAuthorization(ctx, "binding-close-consumed", 1, "materialization_settled", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("issue consumed: %v", err)
	}
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE close_authorizations SET status = 'consumed', consumed_at = ? WHERE id = ?`,
		now.Format(time.RFC3339Nano), consumed); err != nil {
		t.Fatalf("mark consumed: %v", err)
	}

	n, err := js.ExpireCloseAuthorizations(ctx, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("ExpireCloseAuthorizations swept %d rows, want 1 (only the old issued row)", n)
	}

	var oldStatus, freshStatus, consumedStatus string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT status FROM close_authorizations WHERE id = ?`, old).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT status FROM close_authorizations WHERE id = ?`, fresh).Scan(&freshStatus); err != nil {
		t.Fatal(err)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT status FROM close_authorizations WHERE id = ?`, consumed).Scan(&consumedStatus); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "expired" {
		t.Fatalf("old token status = %q, want expired", oldStatus)
	}
	if freshStatus != "issued" {
		t.Fatalf("fresh token status = %q, want unchanged issued", freshStatus)
	}
	if consumedStatus != "consumed" {
		t.Fatalf("already-consumed token status = %q, want unchanged consumed", consumedStatus)
	}
}
