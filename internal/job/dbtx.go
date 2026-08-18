// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"database/sql"
)

// dbtx is satisfied by both *sql.DB and *sql.Tx. Store methods that used to
// open their own transaction are split into a thin public wrapper (which
// still opens and commits its own transaction for standalone callers) and an
// unexported *Tx-agnostic core that runs the same statements against
// whichever dbtx it is handed. ApplyClaimObservation (claim_observation_apply.go)
// is the reason this exists: journaling one claim_observation and every side
// effect it authorizes (lease renewal/promotion, profile evidence, claim
// abandonment, close-authorization consumption) must commit or roll back
// together, so it drives every one of those cores with one shared *sql.Tx
// instead of each opening its own.
type dbtx interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
