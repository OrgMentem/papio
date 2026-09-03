// Copyright 2026 OrgMentem. Licensed under MIT.

package pulse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"papio/internal/batch"
	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/triage"
	"papio/internal/watch"
	"papio/internal/work"
)

func pulseJobs(t *testing.T) *job.Store {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.DataDir(t))
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

func TestReadTypedGateAlgebraIgnoresTerminalMembers(t *testing.T) {
	// Live algebra failures came from human_gate claim_member_job_ids listing
	// terminal siblings: gateMemberCount subtracted every member id while only
	// nonterminal rows are skipped in the bucket loop.
	ctx := context.Background()
	js := pulseJobs(t)
	nonterminal := make([]string, 4)
	for i := range nonterminal {
		id, err := js.CreateRequest(ctx, "wr_pulse_gate_nt_"+string(rune('a'+i)), pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
		if err != nil {
			t.Fatal(err)
		}
		nonterminal[i] = id
		if _, err := js.S.DB().ExecContext(ctx, `UPDATE jobs SET state = 'awaiting_human' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := js.OpenHumanAction(ctx, id, "manual_download", "download", job.Access(false, "landing_page")); err != nil {
			t.Fatal(err)
		}
	}
	ready := make([]string, 2)
	for i := range ready {
		id, err := js.CreateRequest(ctx, "wr_pulse_gate_ready_"+string(rune('a'+i)), pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
		if err != nil {
			t.Fatal(err)
		}
		ready[i] = id
		if err := js.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
			t.Fatal(err)
		}
		if err := js.Transition(ctx, id, job.StateResolving, job.StateReady, nil); err != nil {
			t.Fatal(err)
		}
	}
	cancelled, err := js.CreateRequest(ctx, "wr_pulse_gate_cancel", pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, cancelled, job.StateQueued, job.StateCancelled, nil); err != nil {
		t.Fatal(err)
	}
	members := append(append([]string(nil), nonterminal...), ready[0], ready[1], cancelled)
	if err := js.UpsertHumanGateObservation(ctx, job.HumanGateObservation{
		ID: "pulse-gate-terminal-members", GateType: job.HumanGateLogin,
		ScopeClass: string(job.HumanGateScopeInstitutionProfile), ScopeKey: "profile-term",
		DependentJobIDs: members[1:], ClaimMemberJobIDs: members,
		ObservationRevision: 1, Status: job.HumanGateOpen, DetailJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	snap, err := (&Service{Jobs: js, Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if snap.ProjectionComplete == nil || !*snap.ProjectionComplete {
		t.Fatalf("projection_complete = %v", snap.ProjectionComplete)
	}
	if snap.WaitingRequired == nil || *snap.WaitingRequired != 1 || snap.NonterminalTotal == nil || *snap.NonterminalTotal != 1 {
		t.Fatalf("typed gate with terminal members = waiting %v total %v", snap.WaitingRequired, snap.NonterminalTotal)
	}
	if got := *snap.InFlight + *snap.Scheduled + *snap.Continuing + *snap.WaitingRequired + *snap.Stalled; got != *snap.NonterminalTotal {
		t.Fatalf("pulse buckets sum to %d, want nonterminal_total %d", got, *snap.NonterminalTotal)
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

type pulseCohortMember struct{ key, jobID, outcome string }

// insertPulseCohort seeds one acquisition batch exactly as the cohort tables
// hold it. The projection reads those tables directly, and only raw rows can
// express the two shapes under test here: a label longer than the wire bound,
// and a settlement stamp chosen relative to a job outcome.
func insertPulseCohort(t *testing.T, js *job.Store, id, label, membership string, created, updated time.Time, closed string, members []pulseCohortMember) {
	t.Helper()
	ctx := context.Background()
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO acquisition_batches
			(id, cohort_id, source_kind, source_label, expected_total, created_at, updated_at, closed_at, membership_state)
		VALUES (?, ?, 'cli', ?, ?, ?, ?, NULLIF(?, ''), ?)`,
		id, "cohort_"+id, label, len(members),
		created.UTC().Format(time.RFC3339Nano), updated.UTC().Format(time.RFC3339Nano), closed, membership); err != nil {
		t.Fatal(err)
	}
	for i, m := range members {
		if _, err := js.S.DB().ExecContext(ctx, `
			INSERT INTO acquisition_batch_members
				(batch_id, ordinal, canonical_key, job_id, submission_outcome, created_at)
			VALUES (?, ?, ?, NULLIF(?, ''), ?, ?)`,
			id, i, m.key, m.jobID, m.outcome, created.UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
}

func pulseCohortJob(t *testing.T, js *job.Store, suffix string) string {
	t.Helper()
	id, err := js.CreateRequest(context.Background(), "wr_pulse_cohort_member_"+suffix, pulseWork(), "", "", pulsePolicy(), nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func wantBatchCount(t *testing.T, field string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("latest_batch.%s = nil, want %d", field, want)
	}
	if *got != want {
		t.Fatalf("latest_batch.%s = %d, want %d", field, *got, want)
	}
}

// TestReadLatestBatchCompleteProjectsCohortCounts pins the cohort denominator
// the `papio pulse` and status surfaces print beside a batch. Every bucket
// carries a distinct count, so a member filed under the wrong column fails
// here even though the buckets still sum to nonterminal_total.
func TestReadLatestBatchCompleteProjectsCohortCounts(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	var members []pulseCohortMember
	seed := func(bucket string, count int, prepare func(id string)) {
		for i := range count {
			id := pulseCohortJob(t, js, fmt.Sprintf("%s_%d", bucket, i))
			prepare(id)
			members = append(members, pulseCohortMember{
				key: fmt.Sprintf("doi:10.1000/%s-%d", bucket, i), jobID: id, outcome: "submitted",
			})
		}
	}
	lease := func(id string, expires time.Time) {
		if err := js.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := js.S.DB().ExecContext(ctx,
			`UPDATE jobs SET lease_owner = 'worker-1', lease_expires_at = ? WHERE id = ?`,
			expires.Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
	}
	seed("inflight", 1, func(id string) { lease(id, now.Add(time.Minute)) })
	seed("scheduled", 2, func(id string) {
		if err := js.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
			t.Fatal(err)
		}
		if err := js.Transition(ctx, id, job.StateResolving, job.StateRetryWait, nil, job.WithRetryAt(now.Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	})
	seed("continuing", 3, func(string) {})
	seed("waiting", 4, func(id string) {
		if _, err := js.S.DB().ExecContext(ctx, `UPDATE jobs SET state = 'awaiting_human' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := js.OpenHumanAction(ctx, id, "manual_download", "download", job.Access(false, "landing_page")); err != nil {
			t.Fatal(err)
		}
	})
	// An expired lease is a wedged worker holding the job: stalled, never
	// continuing.
	seed("stalled", 5, func(id string) { lease(id, now.Add(-time.Minute)) })
	// Six members were already owned, so the cohort settled them without ever
	// creating a job and none of them is unavailable.
	for i := range 6 {
		members = append(members, pulseCohortMember{key: fmt.Sprintf("doi:10.1000/owned-%d", i), outcome: "already_owned"})
	}

	started := now.Add(-time.Hour)
	insertPulseCohort(t, js, "batch_pulse_complete", "August sweep", "complete", started, now.Add(-time.Minute), "", members)

	snap, err := (&Service{Jobs: js, Cohorts: batch.New(js.S), Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := snap.LatestBatch
	if b == nil {
		t.Fatal("latest_batch = nil, want the seeded cohort")
	}
	if b.BatchID != "batch_pulse_complete" || b.Label != "August sweep" || b.Membership != "complete" {
		t.Fatalf("latest_batch identity = %+v", b)
	}
	if b.StartedAt != stamp(started) {
		t.Fatalf("latest_batch.started_at = %q, want %q", b.StartedAt, stamp(started))
	}
	if b.ProjectionComplete == nil || !*b.ProjectionComplete {
		t.Fatalf("latest_batch.projection_complete = %v, want true", b.ProjectionComplete)
	}
	if b.SettledAt != "" {
		t.Fatalf("latest_batch.settled_at = %q, want empty while fifteen members are live", b.SettledAt)
	}
	wantBatchCount(t, "total", b.Total, 21)
	wantBatchCount(t, "settled", b.Settled, 6)
	wantBatchCount(t, "nonterminal_total", b.NonterminalTotal, 15)
	wantBatchCount(t, "in_flight", b.InFlight, 1)
	wantBatchCount(t, "scheduled", b.Scheduled, 2)
	wantBatchCount(t, "continuing", b.Continuing, 3)
	wantBatchCount(t, "waiting_required", b.WaitingRequired, 4)
	wantBatchCount(t, "stalled", b.Stalled, 5)
	wantBatchCount(t, "unavailable", b.Unavailable, 0)
	if got := *b.InFlight + *b.Scheduled + *b.Continuing + *b.WaitingRequired + *b.Stalled; got != *b.NonterminalTotal {
		t.Fatalf("cohort buckets sum to %d, want nonterminal_total %d", got, *b.NonterminalTotal)
	}
}

// TestReadLatestBatchSettledAtAdvancesLastFinishedAt pins the precedence
// between the two settlement authorities. A cohort that settled after the last
// job transition moves last_finished_at forward; a cohort that settled earlier
// must never drag it backwards.
func TestReadLatestBatchSettledAtAdvancesLastFinishedAt(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	outside := pulseCohortJob(t, js, "outside")
	if err := js.Transition(ctx, outside, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, outside, job.StateResolving, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT MAX(at) FROM events WHERE kind = 'job.transition'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	finished, ok := parseTime(raw)
	if !ok {
		t.Fatalf("job transition stamp %q is unparseable", raw)
	}

	// Every member is already_owned, so the projection keeps the stored
	// closed_at instead of deriving settlement from a terminal job event.
	late := finished.Add(time.Hour)
	insertPulseCohort(t, js, "batch_pulse_settled", "settled sweep", "complete",
		finished.Add(-time.Hour), late, stamp(late), []pulseCohortMember{
			{key: "doi:10.1000/owned-a", outcome: "already_owned"},
			{key: "doi:10.1000/owned-b", outcome: "already_owned"},
		})

	now := late.Add(time.Minute)
	svc := &Service{Jobs: js, Cohorts: batch.New(js.S), Now: func() time.Time { return now }}
	snap, err := svc.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.LatestBatch == nil || snap.LatestBatch.SettledAt != stamp(late) {
		t.Fatalf("latest_batch = %+v, want settled_at %q", snap.LatestBatch, stamp(late))
	}
	if snap.LastFinishedAt != stamp(late) {
		t.Fatalf("last_finished_at = %q, want the later cohort settlement %q", snap.LastFinishedAt, stamp(late))
	}

	early := finished.Add(-time.Minute)
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE acquisition_batches SET closed_at = ? WHERE id = ?`, stamp(early), "batch_pulse_settled"); err != nil {
		t.Fatal(err)
	}
	snap, err = svc.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.LatestBatch == nil || snap.LatestBatch.SettledAt != stamp(early) {
		t.Fatalf("latest_batch = %+v, want settled_at %q", snap.LatestBatch, stamp(early))
	}
	if snap.LastFinishedAt != stamp(finished) {
		t.Fatalf("last_finished_at = %q, want the job authority %q", snap.LastFinishedAt, stamp(finished))
	}
}

// TestReadLatestBatchPartialMembershipWithholdsCounts covers the cohort whose
// membership never closed. Its members are individually projectable, so a
// regression that counted them anyway would publish a denominator the cohort
// cannot support.
func TestReadLatestBatchPartialMembershipWithholdsCounts(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	done := pulseCohortJob(t, js, "partial_ready")
	if err := js.Transition(ctx, done, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, done, job.StateResolving, job.StateReady, nil); err != nil {
		t.Fatal(err)
	}
	live := pulseCohortJob(t, js, "partial_queued")

	// The cohort is still open, but its last chunk landed 20 minutes ago: the
	// projection must give up on the denominator rather than wait forever.
	insertPulseCohort(t, js, "batch_pulse_open", "interrupted sweep", "open",
		now.Add(-time.Hour), now.Add(-20*time.Minute), "", []pulseCohortMember{
			{key: "doi:10.1000/partial-ready", jobID: done, outcome: "submitted"},
			{key: "doi:10.1000/partial-queued", jobID: live, outcome: "submitted"},
		})

	snap, err := (&Service{Jobs: js, Cohorts: batch.New(js.S), Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := snap.LatestBatch
	if b == nil {
		t.Fatal("latest_batch = nil, want the partial cohort named")
	}
	if b.BatchID != "batch_pulse_open" || b.Membership != "partial" {
		t.Fatalf("latest_batch = %+v, want batch_pulse_open reported as partial", b)
	}
	if b.StartedAt != stamp(now.Add(-time.Hour)) {
		t.Fatalf("latest_batch.started_at = %q, want %q", b.StartedAt, stamp(now.Add(-time.Hour)))
	}
	if b.ProjectionComplete == nil || *b.ProjectionComplete {
		t.Fatalf("latest_batch.projection_complete = %v, want false", b.ProjectionComplete)
	}
	for field, got := range map[string]*int64{
		"total": b.Total, "settled": b.Settled, "nonterminal_total": b.NonterminalTotal,
		"in_flight": b.InFlight, "scheduled": b.Scheduled, "continuing": b.Continuing,
		"waiting_required": b.WaitingRequired, "stalled": b.Stalled, "unavailable": b.Unavailable,
	} {
		if got != nil {
			t.Fatalf("latest_batch.%s = %d, want omitted for partial membership", field, *got)
		}
	}
	var membership string
	if err := js.S.DB().QueryRowContext(ctx,
		`SELECT membership_state FROM acquisition_batches WHERE id = ?`, "batch_pulse_open").Scan(&membership); err != nil {
		t.Fatal(err)
	}
	if membership != "partial" {
		t.Fatalf("stored membership_state = %q, want the durable partial verdict", membership)
	}
}

// TestReadLatestBatchLabelTruncatesToWireByteBound holds the wire bound on a
// caller-supplied cohort label. protocol.WorkPulseLatestBatch.validate bounds
// latest_batch.label at 256 UTF-8 BYTES, and the daemon self-validates its own
// outbound frames, so a label that only satisfies a 256-RUNE bound builds a
// frame the daemon then rejects — a fatal transport condition that drops the
// whole browser session. The truncation must also land on a rune boundary,
// because invalid UTF-8 would survive the control-character check.
func TestReadLatestBatchLabelTruncatesToWireByteBound(t *testing.T) {
	ctx := context.Background()
	js := pulseJobs(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	// 300 two-byte runes: 600 bytes, well over the cap, and no cut at the
	// 256-byte mark falls on a rune boundary, so a naive slice would split one.
	label := strings.Repeat("é", 300)
	insertPulseCohort(t, js, "batch_pulse_label", label, "complete",
		now.Add(-time.Hour), now.Add(-time.Minute), "", []pulseCohortMember{
			{key: "doi:10.1000/label-owned", outcome: "already_owned"},
		})
	snap, err := (&Service{Jobs: js, Cohorts: batch.New(js.S), Now: func() time.Time { return now }}).Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.LatestBatch == nil {
		t.Fatal("latest_batch = nil, want the seeded cohort")
	}
	got := snap.LatestBatch.Label
	if len(got) > 256 {
		t.Fatalf("label = %d bytes, want at most 256: the wire validator counts bytes, not runes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("label %q is not valid UTF-8: truncation split a rune", got)
	}
	// Two-byte runes divide 256 exactly, so the cap is reachable: 128 whole
	// runes are 256 bytes and all of them must survive. An implementation that
	// truncated one rune further would pass the cap check above while
	// needlessly shortening the operator's label.
	if want := strings.Repeat("é", 128); got != want {
		t.Fatalf("label = %d bytes, want the 128 whole runes that fill the cap exactly (%d bytes)", len(got), len(want))
	}
}
