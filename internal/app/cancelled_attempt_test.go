package app

import (
	"context"
	"database/sql"
	"testing"

	"papio/internal/config"
	"papio/internal/resolver"
	"papio/internal/work"
)

// cancellingResolver cancels the processing context from inside a resolver
// call, which is what an orderly daemon shutdown looks like to the resolve
// loop: the scheduler cancels the job context while a source request is in
// flight.
type cancellingResolver struct {
	name   string
	cancel context.CancelFunc
}

func (c *cancellingResolver) Name() string { return c.name }

func (c *cancellingResolver) Resolve(context.Context, work.Work) ([]resolver.Candidate, error) {
	c.cancel()
	return nil, context.Canceled
}

// An attempt interrupted by cancellation must still be closed. FinishAttempt
// writes through ExecContext, so passing the already-cancelled context is a
// guaranteed no-op that leaves ended_at/outcome NULL forever - an orderly
// shutdown then reads as unfinished work, and openalexyield's FreeReport
// counts the row as LostToFinishAttempt. The cleanup write therefore runs on
// context.WithoutCancel; this test fails if any site drops back to ctx.
func TestCancelledResolveClosesItsAttempt(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc.Resolvers = []ResolverEntry{{
		Adapter: &cancellingResolver{name: "fixture", cancel: cancel},
		Policy:  config.Source{Enabled: true},
	}}

	id, err := svc.Submit(context.Background(), doiRequest("wr_cancelled_attempt"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.resolve(ctx, row); err == nil {
		t.Fatal("resolve returned nil, want the cancellation error")
	}

	var ended, outcome sql.NullString
	if err := jobs.S.DB().QueryRowContext(context.Background(),
		`SELECT ended_at, outcome FROM attempts WHERE job_id = ? ORDER BY id DESC LIMIT 1`, id,
	).Scan(&ended, &outcome); err != nil {
		t.Fatal(err)
	}
	if !ended.Valid || !outcome.Valid {
		t.Fatalf("attempt ended_at=%v outcome=%v, want a settled attempt: the cleanup write was made through the cancelled context", ended, outcome)
	}
	if outcome.String != "cancelled" {
		t.Fatalf("attempt outcome = %q, want cancelled", outcome.String)
	}
}
