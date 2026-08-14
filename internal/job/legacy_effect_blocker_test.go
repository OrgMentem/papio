package job

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"papio/internal/store"
)

func legacyBlockerJob(t *testing.T, js *Store, requestID string) string {
	t.Helper()
	id, err := js.CreateRequest(context.Background(), requestID, testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func legacyStarted(t *testing.T, js *Store, jobID, attempt string, ordinal int64, strategy, revision, domain string) {
	t.Helper()
	if err := js.RecordEvent(context.Background(), jobID, "browser.provider_drive_epoch_started", map[string]any{
		"drive_attempt_id": attempt, "ordinal": ordinal, "strategy": strategy,
		"revision": revision, "safety_domain": domain,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestImportLegacyStartedEpochsRetainsSameAndDifferentDomains(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	first := legacyBlockerJob(t, js, "legacy-same-domain-1")
	second := legacyBlockerJob(t, js, "legacy-same-domain-2")
	third := legacyBlockerJob(t, js, "legacy-different-domain")
	legacyStarted(t, js, first, "epoch-a", 0, "generic", "1", "institution:shared")
	legacyStarted(t, js, second, "epoch-b", 1, "generic", "1", "institution:shared")
	legacyStarted(t, js, third, "epoch-c", 0, "generic", "1", "institution:other")

	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	blockers, err := js.UnresolvedLegacyEffectBlockers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 3 {
		t.Fatalf("unresolved blockers = %d, want 3", len(blockers))
	}
	wantDomains := map[string]string{first: "institution:shared", second: "institution:shared", third: "institution:other"}
	for _, blocker := range blockers {
		if blocker.SafetyDomainID != wantDomains[blocker.JobID] {
			t.Fatalf("job %s imported domain = %q, want %q", blocker.JobID, blocker.SafetyDomainID, wantDomains[blocker.JobID])
		}
		if !blocker.CleanupOnly || blocker.ReconstructedAttempt != nil || blocker.ReconstructedHolder != nil {
			t.Fatalf("legacy blocker fences = %+v, want cleanup-only NULL fences", blocker)
		}
	}
}

func TestImportLegacyStartedEpochsIsIdempotentAndSkipsResolvedTuples(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := legacyBlockerJob(t, js, "legacy-idempotent")
	legacyStarted(t, js, jobID, "epoch-live", 0, "generic", "1", "institution:one")
	legacyStarted(t, js, jobID, "epoch-result", 1, "generic", "1", "institution:one")
	if err := js.RecordEvent(ctx, jobID, "browser.provider_drive_epoch_result", map[string]any{
		"drive_attempt_id": "epoch-result", "ordinal": int64(1), "strategy": "generic", "revision": "1",
	}); err != nil {
		t.Fatal(err)
	}
	legacyStarted(t, js, jobID, "epoch-superseded", 2, "generic", "1", "institution:one")
	if err := js.RecordEvent(ctx, jobID, "browser.provider_drive_epoch_superseded", map[string]any{
		"drive_attempt_id": "epoch-superseded", "ordinal": int64(2), "strategy": "generic", "revision": "1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	count, err := js.UnresolvedLegacyEffectBlockerCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("unresolved blocker count = %d, want two (live and superseded started epochs)", count)
	}
}

func TestImportLegacyStartedEpochsSkipsCurrentPermitRows(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := permitJob(t, js, "current-permit-not-legacy")
	identity := EffectPermitIdentity{
		JobID: jobID, Kind: EffectKindGenericDrive,
		DriveAttemptID: "current-attempt", Ordinal: 0,
		Strategy: "generic", Revision: "1",
	}
	permit, outcome, err := js.AcquireEffectPermit(ctx, EffectPermitAcquireInput{
		Identity: identity, JobAttemptRevision: 1, BrowserHolderGeneration: 1,
		SafetyDomainID: "institution:current", LeaseUntil: time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{
			Kind: "browser.provider_drive_epoch_started",
			Detail: map[string]any{
				"drive_attempt_id": "current-attempt", "ordinal": int64(0),
				"strategy": "generic", "revision": "1",
				"safety_domain": "institution:current",
			},
		},
	})
	if err != nil || outcome != EffectPermitAcquired || permit == nil {
		t.Fatalf("current permit=%+v outcome=%q err=%v", permit, outcome, err)
	}
	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	count, err := js.UnresolvedLegacyEffectBlockerCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("current held permit was also imported as %d legacy blocker(s)", count)
	}
}
func TestImportLegacyStartedEpochsMatchesResultsAndSupersedesExactly(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := legacyBlockerJob(t, js, "legacy-exact-tuple")

	// A terminal event may precede the started event in a repaired/imported
	// history. It still answers that exact tuple and must prevent a blocker.
	if err := js.RecordEvent(ctx, jobID, "browser.provider_drive_epoch_result", map[string]any{
		"drive_attempt_id": "epoch-before-start", "ordinal": int64(0), "strategy": "generic", "revision": "1",
	}); err != nil {
		t.Fatal(err)
	}
	legacyStarted(t, js, jobID, "epoch-before-start", 0, "generic", "1", "institution:exact")

	// A result for a neighboring tuple is not evidence about this epoch.
	legacyStarted(t, js, jobID, "epoch-neighbor", 1, "generic", "1", "institution:exact")
	if err := js.RecordEvent(ctx, jobID, "browser.provider_drive_epoch_result", map[string]any{
		"drive_attempt_id": "epoch-neighbor", "ordinal": int64(1), "strategy": "generic", "revision": "2",
	}); err != nil {
		t.Fatal(err)
	}

	// Supersession is not completion evidence: this tuple remains a blocker.
	legacyStarted(t, js, jobID, "epoch-superseded", 2, "generic", "1", "institution:exact")
	if err := js.RecordEvent(ctx, jobID, "browser.provider_drive_epoch_superseded", map[string]any{
		"drive_attempt_id": "epoch-superseded", "ordinal": int64(2), "strategy": "generic", "revision": "1",
	}); err != nil {
		t.Fatal(err)
	}

	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	blockers, err := js.UnresolvedLegacyEffectBlockers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 2 {
		t.Fatalf("exact tuple filtering = %+v, want neighbor and superseded unresolved tuples", blockers)
	}
	foundNeighbor, foundSuperseded := false, false
	for _, blocker := range blockers {
		foundNeighbor = foundNeighbor || blocker.DriveAttemptID == "epoch-neighbor"
		foundSuperseded = foundSuperseded || blocker.DriveAttemptID == "epoch-superseded"
	}
	if !foundNeighbor || !foundSuperseded {
		t.Fatalf("exact tuple filtering = %+v, want neighbor and superseded unresolved tuples", blockers)
	}
}

func TestSettleLegacyEffectBlockerSettlesExactIndependentRows(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	first := legacyBlockerJob(t, js, "legacy-settle-first")
	second := legacyBlockerJob(t, js, "legacy-settle-second")
	legacyStarted(t, js, first, "epoch-first", 0, "generic", "1", "institution:one")
	legacyStarted(t, js, second, "epoch-second", 0, "generic", "1", "institution:two")
	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := js.RecordEvent(ctx, first, "browser.provider_drive_epoch_result", map[string]any{
		"drive_attempt_id": "epoch-first", "ordinal": int64(0), "strategy": "generic", "revision": "1",
		"outcome": "not_pdf", "safety_domain": "institution:one",
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleLegacyEffectBlocker(ctx, LegacyEffectBlockerInput{Kind: GenericDrive, JobID: first, DriveAttemptID: "epoch-first", Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil {
		t.Fatal(err)
	}
	blockers, err := js.UnresolvedLegacyEffectBlockers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 1 || blockers[0].JobID != second {
		t.Fatalf("unresolved after exact settlement = %+v, want second only", blockers)
	}
	// The exact settlement is idempotent.
	if err := js.SettleLegacyEffectBlocker(ctx, LegacyEffectBlockerInput{Kind: GenericDrive, JobID: first, DriveAttemptID: "epoch-first", Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleLegacyEffectBlocker(ctx, LegacyEffectBlockerInput{Kind: GenericDrive, JobID: "missing", DriveAttemptID: "epoch-first", Ordinal: 0, Strategy: "generic", Revision: "1"}); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("missing blocker settlement = %v, want stale", err)
	}
}
func TestImportLegacyEffectKindsAndExactSettlement(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	genericJob := legacyBlockerJob(t, js, "legacy-kind-generic")
	directJob := legacyBlockerJob(t, js, "legacy-kind-direct")
	institutionalJob := legacyBlockerJob(t, js, "legacy-kind-institutional")
	legacyStarted(t, js, genericJob, "generic-attempt", 0, "generic", "1", "domain:generic")
	if err := js.RecordEvent(ctx, directJob, "browser.direct_route", map[string]any{
		"phase": "offered", "drive_attempt_id": "direct-attempt", "ordinal": int64(1),
		"route_revision": "route-1", "safety_domain": "domain:direct",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at)
		VALUES ('grab-legacy', 'example.test', 'legacy PDF', 'awaiting_file', ?, ?)`,
		now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO institution_profiles
		  (id, configured_name, revision, authority_digest, authentication_claim_id, created_at, updated_at)
		VALUES ('profile-legacy', 'legacy profile', 1, 'digest', 'auth-claim', ?, ?)`,
		now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO browser_candidates
		  (id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		   route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id,
		   adapter_revision, effect_contract_id, status, created_at, updated_at)
		VALUES ('candidate-legacy', ?, 1, 'profile-legacy', 1, 1, 'institutional', 'doi',
		        'pre-route', 'domain:institutional', 'adapter-1', 'effect-1', 'claimed', ?, ?)`,
		institutionalJob, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO materialization_claims
		  (id, candidate_id, browser_holder_generation, materialization_kind, binding_id,
		   phase, route_issuance_ordinal, effect_ordinal, created_at, updated_at)
		VALUES ('claim-legacy', 'candidate-legacy', 1, 'browser_tab', 'binding-legacy',
		        'route_issued', 2, 3, ?, ?)`,
		now, now); err != nil {
		t.Fatal(err)
	}
	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	blockers, err := js.UnresolvedLegacyEffectBlockers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 4 {
		t.Fatalf("imported blockers = %+v, want generic/direct/pdf/institutional", blockers)
	}
	for _, blocker := range blockers {
		if !blocker.CleanupOnly {
			t.Fatalf("imported blocker is not cleanup-only: %+v", blocker)
		}
	}
	identities := []LegacyEffectBlockerInput{
		{Kind: GenericDrive, JobID: genericJob, DriveAttemptID: "generic-attempt", Ordinal: 0, Strategy: "generic", Revision: "1"},
		{Kind: DirectGet, JobID: directJob, DriveAttemptID: "direct-attempt", Ordinal: 1, Strategy: "direct_get", Revision: "route-1"},
		{Kind: PDFGrab, GrabID: "grab-legacy"},
		{Kind: Institutional, JobID: institutionalJob, ClaimID: "claim-legacy", BindingID: "binding-legacy", EffectOrdinal: 3},
	}
	for _, identity := range identities {
		if err := js.SettleLegacyEffectBlocker(ctx, identity); err != nil {
			t.Fatalf("settle exact %s identity: %v", identity.Kind, err)
		}
	}
	if count, err := js.UnresolvedLegacyEffectBlockerCount(ctx); err != nil || count != 0 {
		t.Fatalf("remaining blockers = %d, err=%v, want zero", count, err)
	}
	// A same-shaped tuple of another kind cannot settle a blocker.
	if err := js.SettleLegacyEffectBlocker(ctx, LegacyEffectBlockerInput{
		Kind: GenericDrive, JobID: directJob, DriveAttemptID: "direct-attempt",
		Ordinal: 1, Strategy: "generic", Revision: "route-1",
	}); !errors.Is(err, ErrEffectPermitStale) {
		t.Fatalf("cross-kind settlement = %v, want stale", err)
	}
}
func TestImportLegacyIdentityIndexesPreserveDistinctRows(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	firstJob := legacyBlockerJob(t, js, "legacy-identity-first")
	secondJob := legacyBlockerJob(t, js, "legacy-identity-second")
	now := time.Now().UTC().Format(time.RFC3339Nano)

	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO pdf_grabs(id, url_host, title, state, created_at, updated_at)
		VALUES ('grab-identity-first', 'example.test', 'legacy PDF first', 'awaiting_file', ?, ?),
		       ('grab-identity-second', 'example.test', 'legacy PDF second', 'awaiting_file', ?, ?)`,
		now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO institution_profiles
		  (id, configured_name, revision, authority_digest, authentication_claim_id, created_at, updated_at)
		VALUES ('profile-identities', 'legacy identities', 1, 'digest', 'auth-claim-identities', ?, ?)`,
		now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO browser_candidates
		  (id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
		   route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id,
		   adapter_revision, effect_contract_id, status, created_at, updated_at)
		VALUES ('candidate-identity-first', ?, 1, 'profile-identities', 1, 1, 'institutional', 'doi',
		        'pre-route-first', 'domain:institutional-first', 'adapter-1', 'effect-identity-first', 'claimed', ?, ?),
		       ('candidate-identity-second', ?, 1, 'profile-identities', 1, 1, 'institutional', 'doi',
		        'pre-route-second', 'domain:institutional-second', 'adapter-1', 'effect-identity-second', 'claimed', ?, ?)`,
		firstJob, now, now, secondJob, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO materialization_claims
		  (id, candidate_id, browser_holder_generation, materialization_kind, binding_id,
		   phase, route_issuance_ordinal, effect_ordinal, created_at, updated_at)
		VALUES ('claim-identity-first', 'candidate-identity-first', 1, 'browser_tab', 'binding-identity-first',
		        'route_issued', 1, 1, ?, ?),
		       ('claim-identity-second', 'candidate-identity-second', 1, 'browser_tab', 'binding-identity-second',
		        'route_issued', 2, 2, ?, ?)`,
		now, now, now, now); err != nil {
		t.Fatal(err)
	}

	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}

	var pdfCount, pdfIdentities, institutionalCount, institutionalIdentities int
	if err := js.S.DB().QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT grab_id)
		  FROM legacy_effect_blockers
		 WHERE effect_kind='pdf_grab' AND status='unresolved'`).Scan(&pdfCount, &pdfIdentities); err != nil {
		t.Fatal(err)
	}
	if pdfCount != 2 || pdfIdentities != 2 {
		t.Fatalf("PDF blockers = %d rows across %d identities, want two distinct rows", pdfCount, pdfIdentities)
	}
	if err := js.S.DB().QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT claim_id || '|' || binding_id || '|' || effect_ordinal)
		  FROM legacy_effect_blockers
		 WHERE effect_kind='institutional' AND status='unresolved'`).Scan(&institutionalCount, &institutionalIdentities); err != nil {
		t.Fatal(err)
	}
	if institutionalCount != 2 || institutionalIdentities != 2 {
		t.Fatalf("institutional blockers = %d rows across %d identities, want two distinct rows", institutionalCount, institutionalIdentities)
	}
}

func TestImportLegacyEffectsRejectsMalformedStartAuthority(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := legacyBlockerJob(t, js, "legacy-malformed-start")
	if err := js.RecordEvent(ctx, jobID, "browser.provider_drive_epoch_started", map[string]any{
		"drive_attempt_id": "missing-domain", "ordinal": int64(0),
		"strategy": "generic", "revision": "1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := js.ImportLegacyStartedEpochs(ctx); err == nil ||
		!strings.Contains(err.Error(), "unclassifiable legacy provider drive effect") {
		t.Fatalf("malformed start import error = %v, want precise refusal", err)
	}
	count, err := js.UnresolvedLegacyEffectBlockerCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("malformed import left %d blocker(s), want transaction rollback", count)
	}
}
func TestImportLegacyDriveEffectsRejectsNullAndDanglingJobsAtomically(t *testing.T) {
	tests := []struct {
		name, jobID, kind, detail string
	}{
		{"null generic", "", "browser.provider_drive_epoch_started", `{"drive_attempt_id":"null-generic","ordinal":0,"strategy":"generic","revision":"1","safety_domain":"domain:null"}`},
		{"dangling generic", "dangling-generic", "browser.provider_drive_epoch_started", `{"drive_attempt_id":"dangling-generic","ordinal":0,"strategy":"generic","revision":"1","safety_domain":"domain:dangling"}`},
		{"null direct", "", "browser.direct_route", `{"phase":"offered","drive_attempt_id":"null-direct","ordinal":0,"route_revision":"1","safety_domain":"domain:null"}`},
		{"dangling direct", "dangling-direct", "browser.direct_route", `{"phase":"offered","drive_attempt_id":"dangling-direct","ordinal":0,"route_revision":"1","safety_domain":"domain:dangling"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			validJob := legacyBlockerJob(t, js, "legacy-null-dangling-valid")
			legacyStarted(t, js, validJob, "valid-attempt", 0, "generic", "1", "domain:valid")
			now := time.Now().UTC().Format(time.RFC3339Nano)
			var jobArg any = tt.jobID
			if tt.jobID == "" {
				jobArg = nil
			}
			if _, err := js.S.DB().ExecContext(ctx,
				`INSERT INTO events(job_id, at, kind, detail_json) VALUES (?, ?, ?, ?)`,
				jobArg, now, tt.kind, tt.detail); err != nil {
				t.Fatal(err)
			}
			if err := js.ImportLegacyStartedEpochs(ctx); err == nil ||
				!strings.Contains(err.Error(), "unclassifiable legacy") {
				t.Fatalf("null/dangling import error = %v, want precise refusal", err)
			}
			count, err := js.UnresolvedLegacyEffectBlockerCount(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("null/dangling import left %d blocker(s), want transaction rollback", count)
			}
		})
	}
}

func TestImportLegacyDriveEffectsRejectsReversedConflictingDomainsAtomically(t *testing.T) {
	tests := []struct {
		name, eventKind, firstDetail, secondDetail string
	}{
		{
			name:         "generic",
			eventKind:    "browser.provider_drive_epoch_started",
			firstDetail:  `{"drive_attempt_id":"conflict","ordinal":0,"strategy":"generic","revision":"1","safety_domain":"domain:second"}`,
			secondDetail: `{"drive_attempt_id":"conflict","ordinal":0,"strategy":"generic","revision":"1","safety_domain":"domain:first"}`,
		},
		{
			name:         "direct",
			eventKind:    "browser.direct_route",
			firstDetail:  `{"phase":"offered","drive_attempt_id":"conflict","ordinal":0,"route_revision":"1","safety_domain":"domain:second"}`,
			secondDetail: `{"phase":"offered","drive_attempt_id":"conflict","ordinal":0,"route_revision":"1","safety_domain":"domain:first"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			jobID := legacyBlockerJob(t, js, "legacy-conflicting-domain-"+tt.name)
			now := time.Now().UTC().Format(time.RFC3339Nano)
			for _, detail := range []string{tt.firstDetail, tt.secondDetail} {
				if _, err := js.S.DB().ExecContext(ctx,
					`INSERT INTO events(job_id, at, kind, detail_json) VALUES (?, ?, ?, ?)`,
					jobID, now, tt.eventKind, detail); err != nil {
					t.Fatal(err)
				}
			}
			if err := js.ImportLegacyStartedEpochs(ctx); err == nil ||
				!strings.Contains(err.Error(), "conflicting safety domains") {
				t.Fatalf("conflicting import error = %v, want domain conflict refusal", err)
			}
			count, err := js.UnresolvedLegacyEffectBlockerCount(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("conflicting import left %d blocker(s), want transaction rollback", count)
			}
		})
	}
}

func TestSettleLegacyEffectBlockerCleanupOnlyDoesNotMutateCurrentJob(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := legacyBlockerJob(t, js, "legacy-cleanup-only")
	before, err := js.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := js.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	legacyStarted(t, js, jobID, "epoch-cleanup", 0, "generic", "1", "institution:cleanup")
	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleLegacyEffectBlocker(ctx, LegacyEffectBlockerInput{Kind: GenericDrive, JobID: jobID, DriveAttemptID: "epoch-cleanup", Ordinal: 0, Strategy: "generic", Revision: "1"}); err != nil {
		t.Fatal(err)
	}
	after, err := js.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != after.State || before.ArtifactSHA256 != after.ArtifactSHA256 || before.SelectedCandidateID != after.SelectedCandidateID {
		t.Fatalf("cleanup-only settlement mutated current job: before=%+v after=%+v", before, after)
	}
	eventsAfter, err := js.Events(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore)+1 {
		t.Fatalf("cleanup-only settlement appended event(s): before=%d after=%d", len(eventsBefore), len(eventsAfter))
	}
	for _, event := range eventsAfter[len(eventsBefore):] {
		if event["kind"] == "browser.provider_drive_epoch_result" || event["kind"] == "browser.provider_drive_epoch_superseded" {
			t.Fatalf("cleanup-only settlement wrote current-job effect event: %+v", event)
		}
	}
}

func TestArtifactProducerSettlesOnlyExactLegacyBlocker(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID := legacyBlockerJob(t, js, "legacy-winner")
	legacyStarted(t, js, jobID, "epoch-a", 0, "generic", "1", "institution:one")
	legacyStarted(t, js, jobID, "epoch-b", 1, "generic", "1", "institution:one")
	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	ordinal := int64(0)
	settled, err := js.SettleArtifactProducer(ctx, jobID, ArtifactProducerIdentity{
		Kind: GenericDrive, DriveAttemptID: "epoch-a", Ordinal: &ordinal,
		Strategy: "generic", Revision: "1",
	})
	if err != nil || !settled {
		t.Fatalf("exact legacy artifact settlement settled=%v err=%v", settled, err)
	}
	blockers, err := js.UnresolvedLegacyEffectBlockers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 1 || blockers[0].DriveAttemptID != "epoch-b" {
		t.Fatalf("unresolved blockers=%+v, want only epoch-b", blockers)
	}
}
func TestImportLegacyAbandonedRowsConservatively(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	now := store.Now()

	abandonedGrab := "grab-legacy-abandoned"
	completedGrab := "grab-legacy-completed"
	jobWithCompletion := legacyBlockerJob(t, js, "legacy-pdf-completed")
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO pdf_grabs(id, url_host, title, state, job_id, outcome, created_at, updated_at)
		VALUES (?, 'example.test', 'abandoned PDF', 'abandoned', NULL, 'abandoned', ?, ?),
		       (?, 'example.test', 'completed PDF', 'abandoned', ?, 'job_created', ?, ?)`,
		abandonedGrab, now, now, completedGrab, jobWithCompletion, now, now); err != nil {
		t.Fatal(err)
	}

	profileID := "profile-legacy-abandoned"
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO institution_profiles
		  (id, configured_name, revision, authority_digest, authentication_claim_id, created_at, updated_at)
		VALUES (?, 'legacy profile', 1, 'digest-abandoned', 'auth-abandoned', ?, ?)`,
		profileID, now, now); err != nil {
		t.Fatal(err)
	}
	seedClaim := func(jobID, candidateID, claimID, bindingID string) {
		t.Helper()
		if _, err := js.S.DB().ExecContext(ctx, `
			INSERT INTO browser_candidates
			  (id, job_id, job_attempt_revision, institution_profile_id, institution_profile_revision,
			   route_revision, route_class, identifier_strategy, pre_route_safety_key, safety_domain_id,
			   adapter_revision, effect_contract_id, status, created_at, updated_at)
			VALUES (?, ?, 1, ?, 1, 1, 'institutional', 'doi', ?, ?,
			        'adapter-legacy', 'effect-legacy', 'claimed', ?, ?)`,
			candidateID, jobID, profileID, "pre-route-"+candidateID, "domain:"+candidateID, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := js.S.DB().ExecContext(ctx, `
			INSERT INTO materialization_claims
			  (id, candidate_id, browser_holder_generation, materialization_kind, binding_id,
			   phase, route_issuance_ordinal, effect_ordinal, created_at, updated_at)
			VALUES (?, ?, 1, 'browser_tab', ?, 'abandoned', 2, 3, ?, ?)`,
			claimID, candidateID, bindingID, now, now); err != nil {
			t.Fatal(err)
		}
	}

	blockedJob := legacyBlockerJob(t, js, "legacy-institutional-abandoned-blocked")
	seedClaim(blockedJob, "candidate-abandoned-blocked", "claim-abandoned-blocked", "binding-abandoned-blocked")

	winnerJob := legacyBlockerJob(t, js, "legacy-institutional-abandoned-winner")
	seedClaim(winnerJob, "candidate-abandoned-winner", "claim-abandoned-winner", "binding-abandoned-winner")
	if _, err := js.S.DB().ExecContext(ctx, `
		INSERT INTO artifact_winners
		  (job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at)
		VALUES (?, 1, ?, 1, ?, ?)`,
		winnerJob, "candidate-abandoned-winner", strings.Repeat("a", 64), now); err != nil {
		t.Fatal(err)
	}

	resultJob := legacyBlockerJob(t, js, "legacy-institutional-abandoned-result")
	seedClaim(resultJob, "candidate-abandoned-result", "claim-abandoned-result", "binding-abandoned-result")
	if err := js.RecordEvent(ctx, resultJob, "browser.institutional_effect_result", map[string]any{
		"claim_id": "claim-abandoned-result", "binding_id": "binding-abandoned-result",
		"effect_ordinal": int64(3), "outcome": "acknowledged",
	}); err != nil {
		t.Fatal(err)
	}

	if err := js.ImportLegacyStartedEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	blockers, err := js.UnresolvedLegacyEffectBlockers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 2 {
		t.Fatalf("conservative abandoned import = %+v, want PDF and institutional blockers", blockers)
	}
	var sawPDF, sawInstitutional bool
	for _, blocker := range blockers {
		sawPDF = sawPDF || blocker.Kind == PDFGrab && blocker.GrabID == abandonedGrab
		sawInstitutional = sawInstitutional || blocker.Kind == Institutional &&
			blocker.ClaimID == "claim-abandoned-blocked" && blocker.EffectOrdinal == 3
	}
	if !sawPDF || !sawInstitutional {
		t.Fatalf("conservative abandoned import = %+v, want exact PDF and institutional blockers", blockers)
	}
}
