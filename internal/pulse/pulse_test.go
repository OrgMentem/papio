// Copyright 2026 OrgMentem. Licensed under MIT.

package pulse

import (
	"context"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/triage"
	"papio/internal/watch"
	"papio/internal/work"
)

func pulseJobs(t *testing.T) *job.Store {
	t.Helper()
	s, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &job.Store{S: s}
}

func pulsePolicy() job.Policy {
	return job.Policy{AccessMode: "conservative", DesiredVersion: "any", Resolver: "test", FetchMaxBytes: 1 << 20}
}

func pulseWork() work.Work { return work.Work{DOI: "10.1000/pulse", Title: "Pulse"} }

func TestReadFutureRetryIsScheduledNotStalled(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	id, err := js.CreateRequest(ctx, "wr_pulse_retry", pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	if err := js.Transition(ctx, id, job.StateResolving, job.StateRetryWait, nil, job.WithRetryAt(future)); err != nil {
		t.Fatal(err)
	}
	snap, err := (&Service{Jobs: js, Now: func() time.Time { return now }}).Read(ctx)
	if snap.ProjectionComplete == nil || !*snap.ProjectionComplete {
		t.Fatalf("projection_complete = %v", snap.ProjectionComplete)
	}
	if snap.Scheduled == nil || *snap.Scheduled != 1 {
		t.Fatalf("scheduled = %v, want 1", snap.Scheduled)
	}
	if snap.Stalled != nil && *snap.Stalled != 0 {
		t.Fatalf("stalled = %v", *snap.Stalled)
	}
	if got := PrimaryLabel(snap); got != "Scheduled" {
		t.Fatalf("label = %q, want Scheduled", got)
	}
}
func TestReadSourceGatedQueuedJobIsScheduled(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	id, err := js.CreateRequest(ctx, "wr_pulse_source_gate", pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO candidates (job_id, source, url_redacted, url_key, version, access_basis, reuse_license, created_at)
		VALUES (?, 'openalex', 'https://openalex.example/work', 'openalex:work', 'published', 'open_access', 'unknown', ?)`,
		id, now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO source_budgets (source, identity, next_allowed_at)
		VALUES ('openalex', 'test', ?)`, future); err != nil {
		t.Fatal(err)
	}
	snap, err := (&Service{Jobs: js, Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Scheduled == nil || *snap.Scheduled != 1 || snap.Continuing == nil || *snap.Continuing != 0 {
		t.Fatalf("source-gated buckets = scheduled %v continuing %v", snap.Scheduled, snap.Continuing)
	}
}

func TestReadTypedGateCountsOneTurnForOwnerAndSiblings(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	ids := make([]string, 4)
	for i := range ids {
		id, err := js.CreateRequest(ctx, "wr_pulse_gate_"+string(rune('a'+i)), pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		if _, err := js.S.DB().ExecContext(ctx, `UPDATE jobs SET state = 'awaiting_human' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := js.OpenHumanAction(ctx, id, "manual_download", "download", job.Access(false, "landing_page")); err != nil {
			t.Fatal(err)
		}
	}
	if err := js.UpsertHumanGateObservation(ctx, job.HumanGateObservation{
		ID: "pulse-gate", GateType: job.HumanGateLogin,
		ScopeClass: string(job.HumanGateScopeInstitutionProfile), ScopeKey: "profile",
		DependentJobIDs: ids[1:], ClaimMemberJobIDs: ids,
		ObservationRevision: 1, Status: job.HumanGateOpen, DetailJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	snap, err := (&Service{Jobs: js, Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.WaitingRequired == nil || *snap.WaitingRequired != 1 || snap.NonterminalTotal == nil || *snap.NonterminalTotal != 1 {
		t.Fatalf("typed gate buckets = waiting %v total %v", snap.WaitingRequired, snap.NonterminalTotal)
	}
}

// TestTerminalJobActionKeepsTurnAndPulseScopesDistinct pins the live case that
// motivated this contract: an open openurl_available action survived its job's
// terminal unavailable outcome ("no legal candidates"). The inbox still owns
// one actionable turn, while the pulse must partition only nonterminal work.
func TestTerminalJobActionKeepsTurnAndPulseScopesDistinct(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	id, err := js.CreateRequest(ctx, "wr_pulse_terminal_action", pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, id, job.StateResolving, job.StateUnavailable, nil,
		job.WithTerminalReason(job.TerminalReasonNoLegalCandidates)); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, id, "openurl_available", "open the source page", job.Access(false, "landing_page")); err != nil {
		t.Fatal(err)
	}

	triageCounts, err := triage.New(js.S, watch.NewStore(js.S), js).Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if triageCounts.TurnsRequired == nil || *triageCounts.TurnsRequired != 1 {
		t.Fatalf("turns_required = %v, want 1 for the open terminal-job action", triageCounts.TurnsRequired)
	}

	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	snap, err := (&Service{Jobs: js, Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ProjectionComplete == nil || !*snap.ProjectionComplete {
		t.Fatalf("projection_complete = %v, want complete terminal-only projection", snap.ProjectionComplete)
	}
	if snap.WaitingRequired == nil || *snap.WaitingRequired != 0 {
		t.Fatalf("waiting_required = %v, want 0 because the action's job is terminal", snap.WaitingRequired)
	}
	if snap.NonterminalTotal == nil || *snap.NonterminalTotal != 0 {
		t.Fatalf("nonterminal_total = %v, want 0", snap.NonterminalTotal)
	}
	if snap.InFlight == nil || snap.Scheduled == nil || snap.Continuing == nil || snap.Stalled == nil {
		t.Fatalf("complete projection omitted a bucket: %+v", snap)
	}
	if got := *snap.InFlight + *snap.Scheduled + *snap.Continuing + *snap.WaitingRequired + *snap.Stalled; got != *snap.NonterminalTotal {
		t.Fatalf("pulse buckets sum to %d, want nonterminal_total %d", got, *snap.NonterminalTotal)
	}
}

func TestReadEmptyCompleteProjectionIsIdle(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	snap, err := (&Service{Jobs: js, Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ProjectionComplete == nil || !*snap.ProjectionComplete {
		t.Fatalf("projection_complete = %v", snap.ProjectionComplete)
	}
	if snap.NonterminalTotal == nil || *snap.NonterminalTotal != 0 {
		t.Fatalf("nonterminal_total = %v, want 0", snap.NonterminalTotal)
	}
	if got := PrimaryLabel(snap); got != "Idle" {
		t.Fatalf("label = %q, want Idle", got)
	}
}

func TestPrimaryLabelUnknownIncompleteClaim(t *testing.T) {
	incomplete := false
	continuing := int64(0)
	snap := Snapshot{Schema: 1, GeneratedAt: "2026-08-12T12:00:00Z", ProjectionComplete: &incomplete, Continuing: &continuing}
	if got := PrimaryLabel(snap); got != "Unknown" {
		t.Fatalf("label = %q, want Unknown", got)
	}
}
