// Copyright 2026 OrgMentem. Licensed under MIT.

package pulse

import (
	"context"
	"encoding/json"
	"strings"
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
	if err != nil {
		t.Fatal(err)
	}
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

func TestNextActionCountsEveryJobSharingTheDeadline(t *testing.T) {
	// A backoff cohort is scheduled on one common deadline, so reporting the
	// first row's count told the researcher "retrying 1" beside "3 scheduled".
	ctx := context.Background()
	js := pulseJobs(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	shared := now.Add(time.Hour)
	for i, at := range []time.Time{shared, shared, shared, shared.Add(time.Minute)} {
		id, err := js.CreateRequest(ctx, "wr_pulse_cohort_"+string(rune('a'+i)), pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
		if err != nil {
			t.Fatal(err)
		}
		if err := js.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
			t.Fatal(err)
		}
		if err := js.Transition(ctx, id, job.StateResolving, job.StateRetryWait, nil, job.WithRetryAt(at)); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := (&Service{Jobs: js, Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.NextAction == nil || snap.NextAction.Count == nil {
		t.Fatalf("next_action = %+v, want a counted action", snap.NextAction)
	}
	if *snap.NextAction.Count != 3 {
		t.Fatalf("next_action.count = %d, want 3 (the later retry is a different instant)", *snap.NextAction.Count)
	}
	if snap.NextAction.At != shared.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("next_action.at = %q, want %q", snap.NextAction.At, shared.UTC().Format(time.RFC3339Nano))
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

func TestReadUnknownEffectPermitExposesExactOccupancy(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	jobID, err := js.CreateRequest(ctx, "wr_pulse_effect", pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO effect_permits (
			id, job_id, job_attempt_revision, browser_holder_generation,
			safety_domain_id, effect_kind, slot_index, drive_attempt_id,
			ordinal, strategy, revision, status, lease_until, created_at, updated_at
		) VALUES (?, ?, 1, 1, 'domain', 'generic_drive', 0, 'drive-attempt', 0,
			'generic', '1', 'unknown_completion', ?, ?, ?)`,
		"permit_pulse_unknown", jobID, now.Add(-time.Minute).Format(time.RFC3339Nano),
		now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	snap, err := (&Service{Jobs: js, EffectLimit: 1, Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.EffectCapacity == nil || snap.EffectCapacity.Busy != 1 || snap.EffectCapacity.Limit != 1 {
		t.Fatalf("effect capacity = %+v, want busy 1 limit 1", snap.EffectCapacity)
	}
	if len(snap.EffectPermits) != 1 || snap.EffectPermits[0].PermitID != "permit_pulse_unknown" ||
		snap.EffectPermits[0].Status != string(job.EffectPermitUnknownCompletion) {
		t.Fatalf("effect permits = %+v, want exact unknown occupancy", snap.EffectPermits)
	}
	if snap.Stalled == nil || *snap.Stalled != 1 {
		t.Fatalf("stalled = %v, want 1", snap.Stalled)
	}
	if len(snap.StallEpisodes) != 1 || snap.StallEpisodes[0].EpisodeKey != "permit_pulse_unknown" {
		t.Fatalf("stall episodes = %+v, want exact permit id", snap.StallEpisodes)
	}
}

func TestReadLegacyEffectBlockerRefusesAdmissionWithoutOccupyingCapacity(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	jobID, err := js.CreateRequest(ctx, "wr_pulse_legacy", pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	since := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO legacy_effect_blockers
		  (id, effect_kind, job_id, safety_domain_id, drive_attempt_id, ordinal,
		   strategy, revision, reconstructed_attempt, reconstructed_holder,
		   cleanup_only, status, created_at, updated_at)
		VALUES (?, 'generic_drive', ?, 'must-not-leak', 'legacy-drive', 0,
		        'generic', 'r1', NULL, NULL, 1, 'unresolved', ?, ?)`,
		"legacy-pulse-blocker", jobID, since, since); err != nil {
		t.Fatal(err)
	}
	snap, err := (&Service{Jobs: js, EffectLimit: 1, Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.EffectCapacity == nil || snap.EffectCapacity.Busy != 0 || snap.EffectCapacity.Limit != 1 {
		t.Fatalf("effect capacity = %+v, want busy 0 despite global refusal", snap.EffectCapacity)
	}
	if snap.EffectAdmissionBlocked == nil || !*snap.EffectAdmissionBlocked {
		t.Fatalf("effect admission blocked = %v, want true", snap.EffectAdmissionBlocked)
	}
	if len(snap.LegacyEffectBlockers) != 1 {
		t.Fatalf("legacy blockers = %+v, want one exact blocker", snap.LegacyEffectBlockers)
	}
	blocker := snap.LegacyEffectBlockers[0]
	if blocker.BlockerID != "legacy-pulse-blocker" || blocker.JobID != jobID ||
		blocker.DriveAttemptID != "legacy-drive" || blocker.Strategy != "generic" ||
		blocker.Revision != "r1" || blocker.Recovery != "exact_result_or_correlated_winner" {
		t.Fatalf("legacy blocker projection = %+v", blocker)
	}
	if blocker.Since != since {
		t.Fatalf("legacy blocker since = %q, want %q", blocker.Since, since)
	}
	encoded, _ := json.Marshal(snap)
	if strings.Contains(string(encoded), "must-not-leak") {
		t.Fatalf("pulse leaked safety-domain/provider text: %s", encoded)
	}
}

func TestReadFreshHeldEffectPermitNamesOccupancyWithoutCallingItStalled(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	jobID, err := js.CreateRequest(ctx, "wr_pulse_effect_held", pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, jobID, job.StateResolving, job.StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.OpenHumanAction(ctx, jobID, "openurl_handoff", "pulse effect permit fixture", job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	permit, _, err := js.AcquireEffectPermit(ctx, job.EffectPermitAcquireInput{
		Identity: job.EffectPermitIdentity{
			JobID: jobID, Kind: job.EffectKindGenericDrive,
			DriveAttemptID: "drive-held", Ordinal: 0, Strategy: "generic", Revision: "1",
		},
		JobAttemptRevision: 1, BrowserHolderGeneration: 1, SafetyDomainID: "domain-held",
		LeaseUntil: now.Add(time.Minute),
		Authorization: job.EffectPermitEvent{Kind: "browser.provider_drive_epoch_started", Detail: map[string]any{
			"drive_attempt_id": "drive-held", "ordinal": int64(0), "strategy": "generic",
			"revision": "1", "safety_domain": "domain-held",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := (&Service{Jobs: js, EffectLimit: 1, Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.EffectPermits) != 1 || snap.EffectPermits[0].PermitID != permit.ID ||
		snap.EffectPermits[0].Status != string(job.EffectPermitHeld) {
		t.Fatalf("effect permits = %+v, want exact held occupancy", snap.EffectPermits)
	}
	if len(snap.StallEpisodes) != 0 {
		t.Fatalf("fresh held permit reported as stalled: %+v", snap.StallEpisodes)
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
