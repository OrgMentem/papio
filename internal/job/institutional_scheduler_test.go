package job

import (
	"context"
	"fmt"
	"testing"
	"time"

	"papio/internal/store"
)

func insertSchedulerCandidate(t *testing.T, js *Store, id, jobID string, profile InstitutionProfile, safety string, status string, createdAt string) {
	t.Helper()
	insertSchedulerCandidateKeys(t, js, id, jobID, profile, safety, safety, status, createdAt)
}

func insertSchedulerCandidateKeys(t *testing.T, js *Store, id, jobID string, profile InstitutionProfile, preRouteSafety, safetyDomain, status string, createdAt string) {
	t.Helper()
	if status == "" {
		status = "eligible"
	}
	_, err := js.S.DB().ExecContext(context.Background(), `
		INSERT INTO browser_candidates
		 (id, job_id, job_attempt_revision, institution_profile_id,
		  institution_profile_revision, route_revision, route_class,
		  identifier_strategy, pre_route_safety_key, safety_domain_id,
		  adapter_revision, effect_contract_id, status, created_at, updated_at)
		VALUES (?, ?, 1, ?, ?, 1, 'institutional', 'doi', ?, ?,
		        'adapter-1', 'effect-1', ?, ?, ?)`,
		id, jobID, profile.ID, profile.Revision, preRouteSafety, safetyDomain, status, createdAt, createdAt)
	if err != nil {
		t.Fatalf("insert scheduler candidate %s: %v", id, err)
	}
}

func schedulerJob(t *testing.T, js *Store, name string) string {
	t.Helper()
	id, err := js.CreateRequest(context.Background(), name, testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create scheduler job: %v", err)
	}
	return id
}

func TestScheduleEligibleBrowserCandidatesTraversesBeyondFixedPages(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	profile := institutionalProfile(t, js, "scheduler-large", "digest-large", "auth-large")
	jobID := schedulerJob(t, js, "scheduler-large-job")
	for i := range 501 {
		id := fmt.Sprintf("scheduler-candidate-%03d", i)
		domain := fmt.Sprintf("domain-%03d", i)
		insertSchedulerCandidate(t, js, id, jobID, profile, domain, "eligible", "2026-08-01T00:00:00Z")
	}
	first, err := js.ScheduleEligibleBrowserCandidates(ctx, 201, CandidateScheduleCursor{})
	if err != nil {
		t.Fatalf("schedule first large backlog page: %v", err)
	}
	if len(first.Candidates) != 201 {
		t.Fatalf("first large schedule count=%d, want 201", len(first.Candidates))
	}
	seen200 := false
	for _, candidate := range first.Candidates {
		if candidate.CandidateID == "scheduler-candidate-200" {
			seen200 = true
		}
		if candidate.JobID != jobID || candidate.Status != "eligible" || candidate.CreatedAt == "" {
			t.Fatalf("descriptor leaked or lost durable fields: %+v", candidate)
		}
	}
	if !seen200 {
		t.Fatal("candidate beyond position 200 was not scheduled")
	}
	second, err := js.ScheduleEligibleBrowserCandidates(ctx, 300, first.Cursor)
	if err != nil {
		t.Fatalf("schedule second large backlog page: %v", err)
	}
	if len(second.Candidates) != 300 || second.HasMore {
		t.Fatalf("second large schedule count=%d has_more=%v, want 300 false", len(second.Candidates), second.HasMore)
	}
	seen500 := false
	for _, candidate := range second.Candidates {
		if candidate.CandidateID == "scheduler-candidate-500" {
			seen500 = true
		}
	}
	if !seen500 {
		t.Fatal("candidate beyond position 500 was not scheduled")
	}
}

func TestScheduleEligibleBrowserCandidatesFairProfilesAndDomains(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	profiles, err := js.ReconcileInstitutionProfiles(ctx, []InstitutionProfileSpec{
		{ConfiguredName: "scheduler-fair-a", AuthorityDigest: "digest-a", AuthenticationClaimID: "auth-a"},
		{ConfiguredName: "scheduler-fair-b", AuthorityDigest: "digest-b", AuthenticationClaimID: "auth-b"},
		{ConfiguredName: "scheduler-fair-c", AuthorityDigest: "digest-c", AuthenticationClaimID: "auth-c"},
	})
	if err != nil || len(profiles) != 3 {
		t.Fatalf("reconcile fair profiles: profiles=%d err=%v", len(profiles), err)
	}
	for profileIndex, profile := range profiles {
		jobID := schedulerJob(t, js, fmt.Sprintf("scheduler-fair-job-%d", profileIndex))
		for domainIndex := range 3 {
			domain := fmt.Sprintf("fair-domain-%d-%d", profileIndex, domainIndex)
			insertSchedulerCandidate(t, js, fmt.Sprintf("fair-candidate-%d-%d", profileIndex, domainIndex), jobID, profile, domain, "eligible", "2026-08-01T00:00:00Z")
		}
	}
	page, err := js.ScheduleEligibleBrowserCandidates(ctx, 9, CandidateScheduleCursor{})
	if err != nil {
		t.Fatalf("schedule fair rotation: %v", err)
	}
	if len(page.Candidates) != 9 {
		t.Fatalf("fair schedule count=%d, want 9", len(page.Candidates))
	}
	profileCount := map[string]int{}
	domainCount := map[string]int{}
	for _, candidate := range page.Candidates {
		profileCount[candidate.InstitutionProfileID]++
		domainCount[candidate.SafetyDomainID]++
	}
	for _, profile := range profiles {
		if profileCount[profile.ID] != 3 {
			t.Fatalf("profile %s count=%d, want 3: %+v", profile.ID, profileCount[profile.ID], profileCount)
		}
	}
	for domain, count := range domainCount {
		if count != 1 {
			t.Fatalf("safety domain %s selected %d times", domain, count)
		}
	}
}

func TestScheduleEligibleBrowserCandidatesExcludesStaleAndParkedRows(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	profile := institutionalProfile(t, js, "scheduler-fences", "digest-fences", "auth-fences")
	jobID := schedulerJob(t, js, "scheduler-fences-job")
	insertSchedulerCandidate(t, js, "scheduler-valid", jobID, profile, "domain-valid", "eligible", "2026-08-01T00:00:00Z")
	insertSchedulerCandidate(t, js, "scheduler-suppressed", jobID, profile, "domain-suppressed", "eligible", "2026-08-01T00:00:01Z")
	insertSchedulerCandidate(t, js, "scheduler-ineligible-status", jobID, profile, "domain-status", "suppressed", "2026-08-01T00:00:02Z")
	staleJobID := schedulerJob(t, js, "scheduler-stale-job")
	insertSchedulerCandidate(t, js, "scheduler-stale-attempt", staleJobID, profile, "domain-stale", "eligible", "2026-08-01T00:00:03Z")
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO route_suppressions
		(id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		 route_revision, safety_domain_id, adapter_revision, identifier_strategy, evidence_observation_id, reason, active, created_at, updated_at)
		VALUES ('scheduler-suppression', ?, 1, ?, ?, 1, 'domain-suppressed', 'adapter-1', 'doi', NULL, 'provider_challenge', 1, ?, ?)`,
		jobID, profile.ID, profile.Revision, "2026-08-01T00:00:00Z", "2026-08-01T00:00:00Z"); err != nil {
		t.Fatalf("insert suppression: %v", err)
	}
	if err := js.RecordEvent(ctx, staleJobID, "job.retry_requested", map[string]any{"reason": "test"}); err != nil {
		t.Fatalf("record retry: %v", err)
	}
	parkedJob := schedulerJob(t, js, "scheduler-parked-job")
	parked := institutionalCandidate(t, js, profile, "scheduler-parked", parkedJob)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: parked.ID, BrowserHolderGeneration: 7, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("claim parked candidate: %v", err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 7, profile.Revision, 42); err != nil {
		t.Fatalf("bind parked candidate: %v", err)
	}
	page, err := js.ScheduleEligibleBrowserCandidates(ctx, 20, CandidateScheduleCursor{})
	if err != nil {
		t.Fatalf("schedule fenced candidates: %v", err)
	}
	foundValid := false
	for _, candidate := range page.Candidates {
		if candidate.CandidateID == "scheduler-valid" {
			foundValid = true
		}
		switch candidate.CandidateID {
		case "scheduler-suppressed", "scheduler-ineligible-status", "scheduler-stale-attempt", "scheduler-parked":
			t.Fatalf("fenced candidate selected: %s", candidate.CandidateID)
		}
	}
	if !foundValid {
		t.Fatal("eligible scheduler-valid candidate was not selected")
	}
}

// TestScheduleEligibleBrowserCandidatesExcludesTerminalJobs pins the corpse case.
// Nothing retires a candidate row when its job reaches a terminal state, and the
// bridge admits at most one candidate per safety domain per poll, so a cancelled
// paper's `eligible` candidate consumed a live paper's turns on that domain
// forever. Observed live 2026-08-19 on the operator's own institution domain.
func TestScheduleEligibleBrowserCandidatesExcludesTerminalJobs(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	profile := institutionalProfile(t, js, "scheduler-terminal", "digest-terminal", "auth-terminal")
	deadJob := schedulerJob(t, js, "scheduler-terminal-dead")
	liveJob := schedulerJob(t, js, "scheduler-terminal-live")
	// Same safety domain: the dead one is the only reason the live one waits.
	insertSchedulerCandidateKeys(t, js, "scheduler-terminal-dead-candidate", deadJob, profile,
		"pre-route-dead", "shared-domain", "eligible", "2026-08-01T00:00:00Z")
	insertSchedulerCandidateKeys(t, js, "scheduler-terminal-live-candidate", liveJob, profile,
		"pre-route-live", "shared-domain", "eligible", "2026-08-01T00:00:01Z")
	if err := js.Cancel(ctx, deadJob, TerminalReasonBrowserCancelled); err != nil {
		t.Fatalf("cancel the dead job: %v", err)
	}

	page, err := js.ScheduleEligibleBrowserCandidates(ctx, 20, CandidateScheduleCursor{})
	if err != nil {
		t.Fatalf("schedule after cancellation: %v", err)
	}
	if len(page.Candidates) != 1 {
		t.Fatalf("scheduled %+v, want only the live job's candidate", page.Candidates)
	}
	if page.Candidates[0].CandidateID != "scheduler-terminal-live-candidate" {
		t.Fatalf("scheduled %q, want the live job's candidate", page.Candidates[0].CandidateID)
	}
}

func TestScheduleEligibleBrowserCandidatesDedupesLandedDomainAcrossPreRoutes(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	profile := institutionalProfile(t, js, "scheduler-domain", "digest-domain", "auth-domain")
	jobID := schedulerJob(t, js, "scheduler-domain-job")
	insertSchedulerCandidateKeys(t, js, "scheduler-domain-a", jobID, profile, "pre-route-a", "landed-domain", "eligible", "2026-08-01T00:00:00Z")
	insertSchedulerCandidateKeys(t, js, "scheduler-domain-b", jobID, profile, "pre-route-b", "landed-domain", "eligible", "2026-08-01T00:00:01Z")
	page, err := js.ScheduleEligibleBrowserCandidates(ctx, 2, CandidateScheduleCursor{})
	if err != nil {
		t.Fatalf("schedule landed-domain siblings: %v", err)
	}
	if len(page.Candidates) != 1 || page.Candidates[0].SafetyDomainID != "landed-domain" {
		t.Fatalf("landed-domain siblings scheduled as %+v, want one domain descriptor", page.Candidates)
	}
}

func TestScheduleEligibleBrowserCandidatesKeysetContinuationIsDeterministic(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	profile := institutionalProfile(t, js, "scheduler-keyset", "digest-keyset", "auth-keyset")
	jobID := schedulerJob(t, js, "scheduler-keyset-job")
	for i := range 12 {
		insertSchedulerCandidate(t, js, fmt.Sprintf("keyset-%02d", i), jobID, profile, fmt.Sprintf("keyset-domain-%02d", i), "eligible", "2026-08-01T00:00:00Z")
	}
	first, err := js.ScheduleEligibleBrowserCandidates(ctx, 4, CandidateScheduleCursor{})
	if err != nil {
		t.Fatalf("first keyset page: %v", err)
	}
	second, err := js.ScheduleEligibleBrowserCandidates(ctx, 4, first.Cursor)
	if err != nil {
		t.Fatalf("second keyset page: %v", err)
	}
	seen := map[string]bool{}
	for _, candidate := range first.Candidates {
		seen[candidate.CandidateID] = true
	}
	for _, candidate := range second.Candidates {
		if seen[candidate.CandidateID] {
			t.Fatalf("keyset continuation repeated candidate %s", candidate.CandidateID)
		}
		seen[candidate.CandidateID] = true
	}
	if len(seen) != 8 {
		t.Fatalf("continued keyset selected %d unique candidates, want 8", len(seen))
	}
	encoded, err := second.Cursor.Encode()
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	decoded := DecodeCandidateScheduleCursor(encoded)
	third, err := js.ScheduleEligibleBrowserCandidates(ctx, 4, decoded)
	if err != nil {
		t.Fatalf("encoded keyset page: %v", err)
	}
	for _, candidate := range third.Candidates {
		if seen[candidate.CandidateID] {
			t.Fatalf("encoded continuation repeated candidate %s", candidate.CandidateID)
		}
	}
}
func TestScheduleEligibleBrowserCandidatesShadowMatchesAuthoritativeEligibility(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	profiles, err := js.ReconcileInstitutionProfiles(ctx, []InstitutionProfileSpec{
		{ConfiguredName: "scheduler-shadow-a", AuthorityDigest: "shadow-a", AuthenticationClaimID: "shadow-auth-a"},
		{ConfiguredName: "scheduler-shadow-b", AuthorityDigest: "shadow-b", AuthenticationClaimID: "shadow-auth-b"},
	})
	if err != nil || len(profiles) != 2 {
		t.Fatalf("reconcile shadow profiles: profiles=%d err=%v", len(profiles), err)
	}
	profileA, profileB := profiles[0], profiles[1]
	if _, err := js.ReconcileInstitutionProfiles(ctx, []InstitutionProfileSpec{
		{ConfiguredName: profileA.ConfiguredName, AuthorityDigest: "shadow-a-revised", AuthenticationClaimID: "shadow-auth-a"},
		{ConfiguredName: profileB.ConfiguredName, AuthorityDigest: profileB.AuthorityDigest, AuthenticationClaimID: profileB.AuthenticationClaimID},
	}); err != nil {
		t.Fatalf("revise shadow profile: %v", err)
	}
	revisedProfileA, err := js.GetInstitutionProfile(ctx, profileA.ID)
	if err != nil || revisedProfileA == nil {
		t.Fatalf("read revised shadow profile: profile=%+v err=%v", revisedProfileA, err)
	}
	profileA = *revisedProfileA
	validJob := schedulerJob(t, js, "scheduler-shadow-valid")
	insertSchedulerCandidate(t, js, "shadow-valid-a", validJob, profileA, "shadow-domain-a", "eligible", "2026-08-01T00:00:00Z")
	insertSchedulerCandidate(t, js, "shadow-valid-b", validJob, profileB, "shadow-domain-b", "eligible", "2026-08-01T00:00:01Z")
	insertSchedulerCandidateKeys(t, js, "shadow-valid-b-sibling", validJob, profileB, "shadow-pre-b", "shadow-domain-b", "eligible", "2026-08-01T00:00:02Z")

	staleAttemptJob := schedulerJob(t, js, "scheduler-shadow-stale-attempt")
	insertSchedulerCandidate(t, js, "shadow-stale-attempt", staleAttemptJob, profileB, "shadow-domain-stale-attempt", "eligible", "2026-08-01T00:00:03Z")
	if err := js.RecordEvent(ctx, staleAttemptJob, "job.retry_requested", map[string]any{"reason": "shadow"}); err != nil {
		t.Fatalf("record shadow retry: %v", err)
	}
	insertSchedulerCandidate(t, js, "shadow-terminal", validJob, profileB, "shadow-domain-terminal", "suppressed", "2026-08-01T00:00:04Z")
	insertSchedulerCandidate(t, js, "shadow-suppressed", validJob, profileB, "shadow-domain-suppressed", "eligible", "2026-08-01T00:00:05Z")
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO route_suppressions
		(id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		 route_revision, safety_domain_id, adapter_revision, identifier_strategy,
		 evidence_observation_id, reason, active, created_at, updated_at)
		VALUES ('shadow-suppression', ?, 1, ?, ?, 1, 'shadow-domain-suppressed',
		        'adapter-1', 'doi', NULL, 'provider_challenge', 1, ?, ?)`,
		validJob, profileB.ID, profileB.Revision, "2026-08-01T00:00:00Z", "2026-08-01T00:00:00Z"); err != nil {
		t.Fatalf("insert shadow suppression: %v", err)
	}
	staleProfileJob := schedulerJob(t, js, "scheduler-shadow-stale-profile")
	insertSchedulerCandidate(t, js, "shadow-stale-profile", staleProfileJob, InstitutionProfile{
		ID: profileA.ID, Revision: profileA.Revision - 1,
	}, "shadow-domain-stale-profile", "eligible", "2026-08-01T00:00:06Z")
	// The insert helper only needs the profile ID/revision; the FK and authority
	// query below establish that this row is stale after the profile revision.

	liveJob := schedulerJob(t, js, "scheduler-shadow-live")
	live := institutionalCandidate(t, js, profileB, "shadow-live", liveJob)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: live.ID, BrowserHolderGeneration: 41, JobAttemptRevision: 1,
		InstitutionProfileRevision: profileB.Revision, RouteRevision: live.RouteRevision,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("claim shadow live candidate: %v", err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 41, profileB.Revision, 99); err != nil {
		t.Fatalf("bind shadow live candidate: %v", err)
	}

	now := store.Now()
	rows, err := js.S.DB().QueryContext(ctx, `SELECT c.id
		FROM browser_candidates c
		JOIN institution_profiles p ON p.id=c.institution_profile_id
		WHERE c.status='eligible'
		  AND p.tombstoned_at IS NULL
		  AND p.revision=c.institution_profile_revision
		  AND c.job_attempt_revision = 1 + (
			SELECT COUNT(*) FROM events e
			WHERE e.job_id=c.job_id AND e.kind='job.retry_requested')
		  AND NOT EXISTS (
			SELECT 1 FROM route_suppressions s
			WHERE s.active=1 AND s.job_id=c.job_id
			  AND s.job_attempt_revision=c.job_attempt_revision
			  AND s.institution_profile_id=c.institution_profile_id
			  AND s.institution_profile_revision=c.institution_profile_revision
			  AND s.route_revision=c.route_revision
			  AND s.safety_domain_id=c.safety_domain_id
			  AND s.adapter_revision=c.adapter_revision
			  AND s.identifier_strategy=c.identifier_strategy)
		  AND NOT EXISTS (
			SELECT 1 FROM materialization_claims m
			WHERE m.candidate_id=c.id
			  AND m.phase IN ('claimed','bound','route_issued','navigated')
			  AND (m.lease_until IS NULL OR m.lease_until > ?))
		  AND NOT EXISTS (
			SELECT 1 FROM materialization_claims m
			JOIN browser_candidates sibling ON sibling.id=m.candidate_id
			WHERE sibling.safety_domain_id=c.safety_domain_id
			  AND m.phase IN ('bound','route_issued','navigated')
			  AND (m.lease_until IS NULL OR m.lease_until > ?))`, now, now)
	if err != nil {
		t.Fatalf("read authoritative shadow eligibility: %v", err)
	}
	defer rows.Close()
	expected := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan authoritative shadow eligibility: %v", err)
		}
		expected[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate authoritative shadow eligibility: %v", err)
	}

	got := map[string]bool{}
	cursor := CandidateScheduleCursor{}
	for {
		page, err := js.ScheduleEligibleBrowserCandidates(ctx, 1, cursor)
		if err != nil {
			t.Fatalf("schedule shadow page: %v", err)
		}
		for _, descriptor := range page.Candidates {
			if got[descriptor.CandidateID] {
				t.Fatalf("shadow scheduler repeated candidate %s", descriptor.CandidateID)
			}
			got[descriptor.CandidateID] = true
		}
		if !page.HasMore {
			break
		}
		cursor = page.Cursor
	}
	if len(got) != len(expected) {
		t.Fatalf("shadow scheduler count=%d authoritative=%d got=%v expected=%v", len(got), len(expected), got, expected)
	}
	for id := range expected {
		if !got[id] {
			t.Fatalf("shadow scheduler omitted authoritative candidate %s", id)
		}
	}
	for id := range got {
		if !expected[id] {
			t.Fatalf("shadow scheduler selected ineligible candidate %s", id)
		}
	}
}
