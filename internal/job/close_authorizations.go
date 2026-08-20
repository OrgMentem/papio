// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrCloseAuthorizationConflict reports that a live close-authorization
// token already exists for this binding with a different disposition than
// the one now requested, or that a concurrent mint raced the partial unique
// index (close_authorizations_live_binding). Bridge.Sync maps both cases to
// the wire outcome "busy" (dev/active/claim-observation-protocol.md §2.3):
// the token is genuinely live, just not for the reason this request claims.
var ErrCloseAuthorizationConflict = errors.New("close authorization conflict")

// closeAuthorizationDispositions is the closed vocabulary
// surface_close_request carries (dev/active/claim-observation-protocol.md
// §2.3). Storage only checks membership; whether a disposition is actually
// consistent with a binding's live claim phase is Bridge.Sync's job
// (internal/browser/bridge.go), not this package's.
var closeAuthorizationDispositions = map[string]bool{
	"scaffold_idle":           true,
	"materialization_settled": true,
	"claim_abandoned":         true,
	"job_inactive":            true,
}

// IssueCloseAuthorization mints, or idempotently re-issues, a one-use close
// token for one binding (close_authorizations, migration 0041). At most one
// live ('issued') token exists per binding at a time
// (close_authorizations_live_binding): a repeated authorized request for the
// same live binding returns the SAME close_authorization_id/nonce rather
// than racing a second token into existence against the partial unique
// index. A repeat request carrying a strictly higher browser_holder_generation
// than the live row's re-stamps the row to the new generation; a stale
// (lower or equal) request generation is left untouched — refusing a stale
// request before ever reaching here is the caller's job (Bridge.Sync checks
// the holder fence against materialization state first).
func (js *Store) IssueCloseAuthorization(ctx context.Context, bindingID string, generation int64, disposition string, now time.Time) (id, nonce string, err error) {
	if bindingID == "" || len(bindingID) > 256 || generation < 0 || !closeAuthorizationDispositions[disposition] {
		return "", "", errors.New("invalid close authorization input")
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	tx, err := js.S.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		existingID          string
		existingNonce       string
		existingGeneration  int64
		existingDisposition string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, nonce, browser_holder_generation, disposition
		FROM close_authorizations
		WHERE binding_id = ? AND status = 'issued'`,
		bindingID).Scan(&existingID, &existingNonce, &existingGeneration, &existingDisposition)
	switch {
	case err == nil:
		if existingDisposition != disposition {
			return "", "", ErrCloseAuthorizationConflict
		}
		if generation > existingGeneration {
			if _, err := tx.ExecContext(ctx, `
				UPDATE close_authorizations SET browser_holder_generation = ? WHERE id = ?`,
				generation, existingID); err != nil {
				return "", "", err
			}
		}
		if err := tx.Commit(); err != nil {
			return "", "", err
		}
		return existingID, existingNonce, nil
	case errors.Is(err, sql.ErrNoRows):
		// No live token for this binding: mint a fresh one below.
	default:
		return "", "", err
	}

	mintedID, err := opaqueMaterializationID("close")
	if err != nil {
		return "", "", err
	}
	mintedNonce, err := opaqueMaterializationID("nonce")
	if err != nil {
		return "", "", err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO close_authorizations
		  (id, binding_id, browser_holder_generation, nonce, disposition, status, issued_at)
		VALUES (?, ?, ?, ?, ?, 'issued', ?)`,
		mintedID, bindingID, generation, mintedNonce, disposition, nowText); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return "", "", ErrCloseAuthorizationConflict
		}
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return mintedID, mintedNonce, nil
}

// ExpireCloseAuthorizations marks every still-'issued' token minted before
// the given instant as 'expired' and returns the number of rows changed.
// Rows are never deleted (matching close_authorizations' §4.3 "never
// deleted" design), so a startup reconciliation pass can still distinguish
// "never asked" from "asked and already used" from "asked and it timed out".
func (js *Store) ExpireCloseAuthorizations(ctx context.Context, before time.Time) (int, error) {
	res, err := js.S.DB().ExecContext(ctx, `
		UPDATE close_authorizations
		   SET status = 'expired'
		 WHERE status = 'issued' AND issued_at < ?`,
		before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
