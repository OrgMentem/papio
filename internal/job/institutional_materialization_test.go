package job

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func institutionalProfile(t *testing.T, js *Store, name, digest, claim string) InstitutionProfile {
	t.Helper()
	rows, err := js.ReconcileInstitutionProfiles(context.Background(), []InstitutionProfileSpec{{
		ConfiguredName: name, AuthorityDigest: digest, AuthenticationClaimID: claim,
	}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("reconcile profile: rows=%+v err=%v", rows, err)
	}
	return rows[0]
}

func institutionalCandidate(t *testing.T, js *Store, profile InstitutionProfile, id, jobID string) *BrowserCandidate {
	t.Helper()
	c, err := js.CreateBrowserCandidate(context.Background(), BrowserCandidateInput{
		ID: id, JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 7, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety-key", SafetyDomainID: "domain-key",
		AdapterRevision: "adapter-1", EffectContractID: "effect-1", Status: "eligible",
	})
	if err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	return c
}

func TestInstitutionProfileReconcilePreservesIDsAndTombstones(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	first := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	second, err := js.ReconcileInstitutionProfiles(ctx, []InstitutionProfileSpec{{
		ConfiguredName: "library", AuthorityDigest: "digest-b", AuthenticationClaimID: "auth-b",
	}})
	if err != nil || len(second) != 1 {
		t.Fatalf("authority update: %+v %v", second, err)
	}
	if second[0].ID != first.ID || second[0].Revision != first.Revision+1 || second[0].AuthenticationClaimID != "auth-b" {
		t.Fatalf("authority update = %+v, want same ID and revision+1", second[0])
	}
	if _, err := js.ReconcileInstitutionProfiles(ctx, nil); err != nil {
		t.Fatalf("remove profile: %v", err)
	}
	tombstone, err := js.GetInstitutionProfile(ctx, first.ID)
	if err != nil || tombstone == nil || tombstone.TombstonedAt == "" {
		t.Fatalf("tombstone = %+v err=%v", tombstone, err)
	}
	third := institutionalProfile(t, js, "library", "digest-c", "auth-c")
	if third.ID == first.ID || third.Revision != 1 {
		t.Fatalf("re-added profile = %+v, ID must never be reused", third)
	}
}
func TestMaterializationClaimRejectsProfileDriftAndTombstones(t *testing.T) {
	ctx := context.Background()

	t.Run("revision drift", func(t *testing.T) {
		js := testStore(t)
		jobID, err := js.CreateRequest(ctx, "materialization-revision-drift", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
		if err != nil {
			t.Fatal(err)
		}
		profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
		candidate := institutionalCandidate(t, js, profile, "candidate-revision-drift", jobID)
		if _, err := js.ReconcileInstitutionProfiles(ctx, []InstitutionProfileSpec{{
			ConfiguredName: "library", AuthorityDigest: "digest-b", AuthenticationClaimID: "auth-b",
		}}); err != nil {
			t.Fatal(err)
		}
		_, err = js.ClaimMaterialization(ctx, MaterializationClaimInput{
			CandidateID: candidate.ID, BrowserHolderGeneration: 1, JobAttemptRevision: 1,
			InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
			MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
		})
		if !errors.Is(err, ErrMaterializationStale) {
			t.Fatalf("revision-drift claim = %v, want stale", err)
		}
	})

	t.Run("tombstone", func(t *testing.T) {
		js := testStore(t)
		jobID, err := js.CreateRequest(ctx, "materialization-tombstone", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
		if err != nil {
			t.Fatal(err)
		}
		profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
		candidate := institutionalCandidate(t, js, profile, "candidate-tombstone", jobID)
		if _, err := js.ReconcileInstitutionProfiles(ctx, nil); err != nil {
			t.Fatal(err)
		}
		_, err = js.ClaimMaterialization(ctx, MaterializationClaimInput{
			CandidateID: candidate.ID, BrowserHolderGeneration: 1, JobAttemptRevision: 1,
			InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
			MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
		})
		if !errors.Is(err, ErrMaterializationStale) {
			t.Fatalf("tombstoned claim = %v, want stale", err)
		}
	})
}

func TestMaterializationClaimExactRetryRecoversBinding(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-retry", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	candidate := institutionalCandidate(t, js, profile, "candidate-retry", jobID)
	input := MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: 9, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
	}
	first, err := js.ClaimMaterialization(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := js.ClaimMaterialization(ctx, input)
	if err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if retry.ID != first.ID || retry.BindingID != first.BindingID || retry.Phase != first.Phase {
		t.Fatalf("retry = %+v, first = %+v", retry, first)
	}
	// A live lease is never evictable, whoever asks: the holder of record was
	// recently active, so a request from another generation is a race.
	takeover := input
	takeover.BrowserHolderGeneration++
	if _, err := js.ClaimMaterialization(ctx, takeover); !errors.Is(err, ErrMaterializationBusy) {
		t.Fatalf("takeover across a live lease = %v, want busy", err)
	}
	// Once the lease lapses, a STRICTLY NEWER holder asking for this candidate
	// is the browser telling papio it has no surface for the paper any more, so
	// it takes the candidate over and the stranded claim is retired. Chosen
	// 2026-08-21: a settled effect whose page is gone was otherwise unretirable
	// in principle, and one job re-asked 925 times against its own corpse.
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET lease_until=?
		WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), first.ID); err != nil {
		t.Fatal(err)
	}
	next, err := js.ClaimMaterialization(ctx, takeover)
	if err != nil {
		t.Fatalf("newer holder takeover: %v", err)
	}
	if next.ID == first.ID {
		t.Fatalf("takeover reused the stranded claim %s", first.ID)
	}
	stranded, err := js.GetMaterializationClaim(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stranded.Phase != "abandoned" {
		t.Fatalf("stranded claim = %q, want abandoned", stranded.Phase)
	}
}

// The eviction's safety guard. `settled` means the provider effect is over, so
// retiring the claim loses nothing; a permit still HELD means something may be
// happening at the provider right now, and papio must never let a second
// attempt start across an irreversible effect. A newer holder's request is
// evidence about a surface, never permission to interrupt one.
func TestNewerHolderCannotEvictAClaimWithAnEffectInFlight(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, candidateID := permitInstitutionalJob(t, js, "permit-inflight-claim")
	input := MaterializationClaimInput{
		CandidateID: candidateID, JobAttemptRevision: 1,
		InstitutionProfileRevision: 1, RouteRevision: 1,
		MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
		LeaseUntil: time.Now().Add(time.Minute),
	}
	claim, err := js.ClaimMaterialization(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
		JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
		SafetyDomainID: "domain", InstitutionalRequestID: "request-inflight-claim",
		JobAttemptRevision: 1, BrowserHolderGeneration: 3,
		ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().Add(time.Minute),
		Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
	}); err != nil || outcome != EffectPermitAcquired {
		t.Fatalf("acquire outcome=%v err=%v", outcome, err)
	}
	// Lapse the lease so the ONLY thing standing between the newer holder and
	// this claim is the in-flight permit.
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET lease_until=?
		WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claim.ID); err != nil {
		t.Fatal(err)
	}
	takeover := input
	takeover.BrowserHolderGeneration++
	if _, err := js.ClaimMaterialization(ctx, takeover); !errors.Is(err, ErrMaterializationBusy) {
		t.Fatalf("takeover across a held permit = %v, want busy", err)
	}
	held, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Phase == "abandoned" {
		t.Fatal("a claim with an effect in flight must never be evicted")
	}
}

// The stale sweep is the only mechanism that can reach a claim whose browser
// session is gone: its candidate reads `claimed`, so the scheduler never offers
// it and the browser never asks, and its lease has lapsed, so the
// generation-blind expiry path is its only other hope — and there a settled
// permit vetoes. Five claims sat stranded in exactly that gap on the operator's
// machine, generations 155-206 against a holder at 366.
func TestStaleSweepRetiresALapsedSettledClaimButNotAnInFlightOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settle   bool
		wantGone bool
	}{
		{name: "settled effect is over", settle: true, wantGone: true},
		{name: "effect still in flight", settle: false, wantGone: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			ctx := context.Background()
			jobID, candidateID := permitInstitutionalJob(t, js, "stale-sweep-"+tc.name)
			claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
				CandidateID: candidateID, JobAttemptRevision: 1,
				InstitutionProfileRevision: 1, RouteRevision: 1,
				MaterializationKind: "browser_tab", BrowserHolderGeneration: 3,
				LeaseUntil: time.Now().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, 1, 9); err != nil {
				t.Fatal(err)
			}
			const requestID = "request-stale-sweep"
			if _, outcome, err := js.AcquireInstitutionalEffectPermit(ctx, InstitutionalEffectPermitAcquireInput{
				JobID: jobID, ClaimID: claim.ID, BindingID: claim.BindingID,
				SafetyDomainID: "domain", InstitutionalRequestID: requestID,
				JobAttemptRevision: 1, BrowserHolderGeneration: 3,
				ExpectedEffectOrdinal: 0, LeaseUntil: time.Now().Add(time.Minute),
				Authorization: EffectPermitEvent{Kind: "institutional.authorized"},
			}); err != nil || outcome != EffectPermitAcquired {
				t.Fatalf("acquire outcome=%v err=%v", outcome, err)
			}
			if tc.settle {
				if _, outcome, err := js.SettleEffectPermit(ctx, EffectPermitSettleInput{
					Identity: EffectPermitIdentity{
						JobID: jobID, Kind: Institutional, ClaimID: claim.ID, BindingID: claim.BindingID,
						EffectOrdinal: 1, InstitutionalRequestID: requestID,
					},
					RequiredEvents: []EffectPermitEvent{{Kind: "browser.institutional_effect_result", Detail: map[string]any{
						"claim_id": claim.ID, "binding_id": claim.BindingID, "effect_ordinal": 1,
						"institutional_request_id": requestID,
					}}},
				}); err != nil || outcome != EffectPermitApplied {
					t.Fatalf("settle outcome=%v err=%v", outcome, err)
				}
			}
			// Lapse the lease: invisible to the sweep's old live-lease-only
			// predicate, and to nothing else.
			if _, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET lease_until=?
				WHERE id=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), claim.ID); err != nil {
				t.Fatal(err)
			}

			if _, err := js.AbandonStaleMaterializations(ctx, 4); err != nil {
				t.Fatal(err)
			}
			swept, err := js.GetMaterializationClaim(ctx, claim.ID)
			if err != nil {
				t.Fatal(err)
			}
			if gone := swept.Phase == "abandoned"; gone != tc.wantGone {
				t.Fatalf("claim abandoned = %t, want %t (phase %q)", gone, tc.wantGone, swept.Phase)
			}
		})
	}
}

func TestMaterializationExpiredBindReplayIsStaleAndCandidateEligible(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-expired-bind", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	candidate := institutionalCandidate(t, js, profile, "candidate-expired-bind", jobID)
	// The lease must outlive the bind below by a margin no scheduler can eat:
	// BindMaterialization compares lease_until against store.Now(), so a 20ms
	// lease made the *valid* bind fail as stale on a loaded -race runner. Expiry
	// is driven by ReconcileMaterializationClaims' explicit cutoff instead, which
	// is what this test is actually about.
	expires := time.Now().UTC().Add(time.Hour)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: 3, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, profile.Revision, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReconcileMaterializationClaims(ctx, expires.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, profile.Revision, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("expired binding replay = %v, want stale", err)
	}
	got, err := js.GetBrowserCandidate(ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "eligible" {
		t.Fatalf("candidate status after expiry = %q, want eligible", got.Status)
	}
}

// BindMaterialization's own lease_until predicate, which the two reconcile tests
// above do NOT reach: they expire the claim through ReconcileMaterializationClaims,
// so bind refuses on phase and passes with the lease compare removed. A holder that
// sat past its lease and binds before any reconciler runs must be refused by bind
// itself, or a tab is driven under a lease someone else may already have taken.
func TestBindRefusesAClaimWhoseLeaseHasPassedWithoutReconciliation(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-lease-lapsed", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	candidate := institutionalCandidate(t, js, profile, "candidate-lease-lapsed", jobID)
	// ClaimMaterialization requires a future lease, so the lapse has to happen
	// after the claim. Ageing the row is time passing with no scheduler involved;
	// crucially no reconciler runs, so the claim stays phase='claimed' and the
	// lease is the only thing left that can refuse the bind.
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: 3, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	lapsed := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET lease_until=? WHERE id=?`, lapsed, claim.ID); err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, profile.Revision, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("bind on a lapsed lease = %v, want stale", err)
	}
}

func TestInstitutionProfileReconcileRollsBackOnDuplicate(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	before := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	if _, err := js.ReconcileInstitutionProfiles(ctx, []InstitutionProfileSpec{
		{ConfiguredName: "library", AuthorityDigest: "digest-b", AuthenticationClaimID: "auth-b"},
		{ConfiguredName: "library", AuthorityDigest: "digest-c", AuthenticationClaimID: "auth-c"},
	}); err == nil {
		t.Fatal("duplicate profile reconcile unexpectedly committed")
	}
	after, err := js.GetInstitutionProfile(ctx, before.ID)
	if err != nil || after == nil {
		t.Fatalf("read after rollback: %+v %v", after, err)
	}
	if after.Revision != before.Revision || after.AuthorityDigest != before.AuthorityDigest || after.AuthenticationClaimID != before.AuthenticationClaimID {
		t.Fatalf("rollback lost old authority: before=%+v after=%+v", before, after)
	}
}

func TestMaterializationClaimCASAndRouteOrdinals(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-claim-job", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	candidate := institutionalCandidate(t, js, profile, "candidate-materialization", jobID)
	lease := time.Now().UTC().Add(time.Minute)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: 11, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: lease,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 11, profile.Revision, 0); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 11, profile.Revision, 0); err != nil {
		t.Fatalf("binding replay: %v", err)
	}
	if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 10, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("stale holder route = %v, want stale", err)
	}
	routeOrdinal, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 11, 0)
	if err != nil || routeOrdinal != 1 {
		t.Fatalf("route ordinal = %d err=%v, want 1", routeOrdinal, err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 11, profile.Revision, 7); err != nil {
		t.Fatalf("route-issued replacement bind: %v", err)
	}
	if replay, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 11, 1); err != nil || replay != 1 {
		t.Fatalf("route replay after replacement = %d err=%v, want ordinal 1", replay, err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 11, 1, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("old tab navigation ack = %v, want stale", err)
	}
	if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 11, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("non-monotonic route = %v, want stale", err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 11, 1, 7); err != nil {
		t.Fatalf("navigation ack: %v", err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 11, 1, 7); err != nil {
		t.Fatalf("navigation replay: %v", err)
	}
}
func TestMaterializationRouteReplayRequiresCurrentProfileRevision(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []string{"bound", "route_issued", "navigated"} {
		t.Run(phase, func(t *testing.T) {
			js := testStore(t)
			jobID, err := js.CreateRequest(ctx, "materialization-route-fence-"+phase, testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
			if err != nil {
				t.Fatal(err)
			}
			profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
			candidate := institutionalCandidate(t, js, profile, "candidate-route-fence-"+phase, jobID)
			claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
				CandidateID: candidate.ID, BrowserHolderGeneration: 41,
				JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
				RouteRevision: 7, MaterializationKind: "browser_tab",
				LeaseUntil: time.Now().UTC().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 41, profile.Revision, 0); err != nil {
				t.Fatal(err)
			}
			if phase != "bound" {
				ordinal, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 41, 0)
				if err != nil || ordinal != 1 {
					t.Fatalf("initial route ordinal=%d err=%v", ordinal, err)
				}
			}
			if phase == "navigated" {
				if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 41, 1, 0); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "route_issued" {
				ordinal, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 41, 1)
				if err != nil || ordinal != 1 {
					t.Fatalf("lost-response replay ordinal=%d err=%v", ordinal, err)
				}
			}

			if _, err := js.ReconcileInstitutionProfiles(ctx, []InstitutionProfileSpec{{
				ConfiguredName: "library", AuthorityDigest: "digest-b", AuthenticationClaimID: "auth-b",
			}}); err != nil {
				t.Fatal(err)
			}
			expectedOrdinal := int64(0)
			if phase != "bound" {
				expectedOrdinal = 1
			}
			if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 41, expectedOrdinal); !errors.Is(err, ErrMaterializationStale) {
				t.Fatalf("profile-drift %s replay=%v, want stale", phase, err)
			}
			got, err := js.GetMaterializationClaim(ctx, claim.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Phase != phase || got.RouteIssuanceOrdinal != expectedOrdinal {
				t.Fatalf("profile-drift claim=%+v, want phase=%s ordinal=%d", got, phase, expectedOrdinal)
			}
		})
	}
}
func TestMaterializationBindingAndNavigationRequireExactTab(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-tab-fence", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	candidate := institutionalCandidate(t, js, profile, "candidate-tab-fence", jobID)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: 52, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 52, profile.Revision, 0); err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 52, profile.Revision, 1); err != nil {
		t.Fatalf("replacement binding: %v", err)
	}
	if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 52, 0); err != nil {
		t.Fatal(err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 52, 1, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("navigation on old tab = %v, want stale", err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 52, 1, 1); err != nil {
		t.Fatalf("navigation on replacement tab: %v", err)
	}
}
func TestMaterializationRetryCreatesNewAttemptAndFencesOldRoute(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-retry-epoch", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	makeCandidate := func(id, safety string, attempt int64) *BrowserCandidate {
		t.Helper()
		candidate, err := js.CreateBrowserCandidate(ctx, BrowserCandidateInput{
			ID: id, JobID: jobID, JobAttemptRevision: attempt,
			InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
			RouteRevision: 7, RouteClass: "institutional", IdentifierStrategy: "doi",
			PreRouteSafetyKey: safety, SafetyDomainID: "domain-" + id,
			AdapterRevision: "adapter-1", EffectContractID: "effect-1", Status: "eligible",
		})
		if err != nil {
			t.Fatal(err)
		}
		return candidate
	}
	oldCandidate := makeCandidate("candidate-retry-old", "safety-old", 1)
	oldClaim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: oldCandidate.ID, BrowserHolderGeneration: 61,
		JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision,
		RouteRevision: 7, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, oldClaim.ID, oldClaim.BindingID, 61, profile.Revision, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := js.IssueMaterializationRoute(ctx, oldClaim.ID, oldClaim.BindingID, 61, 0); err != nil {
		t.Fatal(err)
	}
	if err := js.RecordEvent(ctx, jobID, "job.retry_requested", map[string]any{"reason": "explicit_retry"}); err != nil {
		t.Fatal(err)
	}
	attempt, err := js.MaterializationAttemptRevision(ctx, jobID)
	if err != nil || attempt != 2 {
		t.Fatalf("attempt revision=%d err=%v, want 2", attempt, err)
	}
	newCandidate := makeCandidate("candidate-retry-new", "safety-new", attempt)
	if newCandidate.ID == oldCandidate.ID || newCandidate.PreRouteSafetyKey == oldCandidate.PreRouteSafetyKey {
		t.Fatalf("retry candidate did not get distinct identity: old=%+v new=%+v", oldCandidate, newCandidate)
	}
	if _, err := js.IssueMaterializationRoute(ctx, oldClaim.ID, oldClaim.BindingID, 61, 1); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("old route callback after retry=%v, want stale", err)
	}
	if err := js.RenewMaterializationClaim(ctx, oldClaim.ID, 61, time.Now().UTC().Add(2*time.Minute)); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("old lease renewal after retry=%v, want stale", err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, oldClaim.ID, oldClaim.BindingID, 61, 1, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("old navigation after retry=%v, want stale", err)
	}
}

func TestMaterializationConcurrentClaimHasOneWinner(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-race-job", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	candidate := institutionalCandidate(t, js, profile, "candidate-race", jobID)
	input := MaterializationClaimInput{CandidateID: candidate.ID, JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute)}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners, busy int
	for generation := int64(1); generation <= 2; generation++ {
		wg.Add(1)
		go func(g int64) {
			defer wg.Done()
			in := input
			in.BrowserHolderGeneration = g
			_, err := js.ClaimMaterialization(ctx, in)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
			} else if errors.Is(err, ErrMaterializationBusy) {
				busy++
			} else {
				t.Errorf("claim generation %d: %v", g, err)
			}
		}(generation)
	}
	wg.Wait()
	if winners != 1 || busy != 1 {
		t.Fatalf("claim race winners=%d busy=%d, want one each", winners, busy)
	}
}

func TestMaterializationExpiryReconcilesAndRejectsStaleHolder(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-expiry-job", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	candidate := institutionalCandidate(t, js, profile, "candidate-expiry", jobID)
	// Far future, expired by the cutoff below rather than by elapsed wall time.
	expires := time.Now().UTC().Add(time.Hour)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 4, JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "direct_download", LeaseUntil: expires})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := js.ReconcileMaterializationClaims(ctx, expires.Add(time.Millisecond))
	if err != nil || len(expired) != 1 || expired[0].ID != claim.ID {
		t.Fatalf("expired = %+v err=%v", expired, err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, "stale-binding", 4, profile.Revision, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("expired bind = %v, want stale", err)
	}
	replacement, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 5, JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "direct_download", LeaseUntil: time.Now().UTC().Add(time.Minute)})
	if err != nil || replacement.ID == claim.ID {
		t.Fatalf("replacement claim = %+v err=%v", replacement, err)
	}
}

func TestSettleMaterializationRequiresMatchingWinnerAndIsAtomic(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-settle-job", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	candidate := institutionalCandidate(t, js, profile, "candidate-settle", jobID)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 22, JobAttemptRevision: 1, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 22, profile.Revision, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 22, 0); err != nil {
		t.Fatal(err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 22, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleMaterialization(ctx, claim.ID, claim.BindingID, 22, profile.Revision); !errors.Is(err, ErrMaterializationConflict) {
		t.Fatalf("missing winner settle = %v, want conflict", err)
	}
	var phase string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT phase FROM materialization_claims WHERE id=?`, claim.ID).Scan(&phase); err != nil || phase != "navigated" {
		t.Fatalf("failed settle changed phase=%q err=%v", phase, err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO artifact_winners (job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at) VALUES (?, ?, ?, ?, ?, ?)`, jobID, 99, candidate.ID, 22, "sha-wrong", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleMaterialization(ctx, claim.ID, claim.BindingID, 22, profile.Revision); !errors.Is(err, ErrMaterializationConflict) {
		t.Fatalf("mismatched winner settle = %v, want conflict", err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `DELETE FROM artifact_winners WHERE job_id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO artifact_winners (job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at) VALUES (?, ?, ?, ?, ?, ?)`, jobID, 1, candidate.ID, 21, "sha-generation-wrong", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleMaterialization(ctx, claim.ID, claim.BindingID, 22, profile.Revision); !errors.Is(err, ErrMaterializationConflict) {
		t.Fatalf("mismatched generation settle = %v, want conflict", err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `DELETE FROM artifact_winners WHERE job_id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO artifact_winners (job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at) VALUES (?, ?, ?, ?, ?, ?)`, jobID, 1, candidate.ID, 22, "sha-ok", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleMaterialization(ctx, claim.ID, claim.BindingID, 22, profile.Revision); err != nil {
		t.Fatalf("matching winner settle: %v", err)
	}
	settled, err := js.GetMaterializationClaim(ctx, claim.ID)
	if err != nil || settled.Phase != "settled" {
		t.Fatalf("settled claim=%+v err=%v", settled, err)
	}
	got, err := js.GetBrowserCandidate(ctx, candidate.ID)
	if err != nil || got.Status != "succeeded" {
		t.Fatalf("candidate after settle=%+v err=%v", got, err)
	}
	if err := js.SettleMaterialization(ctx, claim.ID, claim.BindingID, 22, profile.Revision); err != nil {
		t.Fatalf("settlement replay: %v", err)
	}
}
func TestMaterializationMutationsRejectProfileDrift(t *testing.T) {
	cases := []struct{ name string }{
		{name: "route"},
		{name: "navigation"},
		{name: "renew"},
		{name: "settle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			js := testStore(t)
			jobID, err := js.CreateRequest(ctx, "materialization-profile-drift-"+tc.name, testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
			if err != nil {
				t.Fatal(err)
			}
			profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
			candidate := institutionalCandidate(t, js, profile, "candidate-profile-drift-"+tc.name, jobID)
			claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
				CandidateID: candidate.ID, BrowserHolderGeneration: 31, JobAttemptRevision: 1,
				InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
				MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "route" || tc.name == "navigation" || tc.name == "settle" {
				if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 31, profile.Revision, 0); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "navigation" || tc.name == "settle" {
				if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 31, 0); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "settle" {
				if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 31, 1, 0); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "settle" {
				if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO artifact_winners
					(job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at)
					VALUES (?, ?, ?, ?, ?, ?)`, jobID, 1, candidate.ID, 31, "sha-drift", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := js.ReconcileInstitutionProfiles(ctx, []InstitutionProfileSpec{{
				ConfiguredName: "library", AuthorityDigest: "digest-b", AuthenticationClaimID: "auth-b",
			}}); err != nil {
				t.Fatal(err)
			}

			var mutationErr error
			switch tc.name {
			case "route":
				_, mutationErr = js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 31, 0)
			case "navigation":
				mutationErr = js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 31, 1, 0)
			case "renew":
				mutationErr = js.RenewMaterializationClaim(ctx, claim.ID, 31, time.Now().UTC().Add(2*time.Minute))
			case "settle":
				mutationErr = js.SettleMaterialization(ctx, claim.ID, claim.BindingID, 31, profile.Revision)
			}
			if !errors.Is(mutationErr, ErrMaterializationStale) {
				t.Fatalf("%s after profile drift = %v, want stale", tc.name, mutationErr)
			}
			got, err := js.GetMaterializationClaim(ctx, claim.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantPhase := "claimed"
			switch tc.name {
			case "route":
				wantPhase = "bound"
			case "navigation", "settle":
				wantPhase = "route_issued"
				if tc.name == "settle" {
					wantPhase = "navigated"
				}
			}
			if got.Phase != wantPhase {
				t.Fatalf("%s mutated phase to %q, want %q", tc.name, got.Phase, wantPhase)
			}
		})
	}
}

func TestInstitutionAuthorityKeyStableAndConcurrent(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	first, err := js.InstitutionAuthorityKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("authority key length = %d, want 32", len(first))
	}
	second, err := js.InstitutionAuthorityKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("authority key changed between reads")
	}

	const calls = 12
	keys := make(chan []byte, calls)
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := js.InstitutionAuthorityKey(ctx)
			if err != nil {
				errs <- err
				return
			}
			keys <- key
		}()
	}
	wg.Wait()
	close(keys)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for key := range keys {
		if len(key) != 32 || string(key) != string(first) {
			t.Fatalf("concurrent key differs or has wrong length: %x", key)
		}
	}
}

func TestInstitutionProfileAndCandidateLookupsAreCurrentAndOrdered(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-lookups", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "configured-library", "authz_opaque", "auth_opaque")
	gotProfile, err := js.InstitutionProfileByConfiguredName(ctx, "configured-library")
	if err != nil || gotProfile == nil {
		t.Fatalf("profile lookup = %+v, %v", gotProfile, err)
	}
	if gotProfile.ID != profile.ID || gotProfile.ConfiguredName != "configured-library" {
		t.Fatalf("profile lookup = %+v, want %q", gotProfile, profile.ID)
	}

	first := institutionalCandidate(t, js, profile, "candidate-first", jobID)
	second := institutionalCandidate(t, js, profile, "candidate-second", jobID)
	if _, err := js.S.DB().ExecContext(ctx, `
		UPDATE browser_candidates SET created_at=?, updated_at=? WHERE id=?`,
		"2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `
		UPDATE browser_candidates SET created_at=?, updated_at=? WHERE id=?`,
		"2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z", second.ID); err != nil {
		t.Fatal(err)
	}
	gotCandidate, err := js.EligibleBrowserCandidateForJob(ctx, jobID, first.JobAttemptRevision)
	if err != nil || gotCandidate == nil {
		t.Fatalf("candidate lookup = %+v, %v", gotCandidate, err)
	}
	if gotCandidate.ID != first.ID {
		t.Fatalf("candidate lookup = %q, want oldest %q", gotCandidate.ID, first.ID)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE browser_candidates SET status='claimed' WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	current, err := js.CurrentBrowserCandidateForJob(ctx, jobID, first.JobAttemptRevision)
	if err != nil || current == nil || current.ID != first.ID {
		t.Fatalf("current candidate = %+v, %v; want claimed first", current, err)
	}
}

func TestMaterializationClaimLookupByBindingID(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, "materialization-binding-lookup", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "binding-library", "authz_opaque", "auth_opaque")
	candidate := institutionalCandidate(t, js, profile, "candidate-binding-lookup", jobID)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: 4, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := js.MaterializationClaimByBindingID(ctx, claim.BindingID)
	if err != nil || got == nil {
		t.Fatalf("binding lookup = %+v, %v", got, err)
	}
	if got.ID != claim.ID || got.BindingID != claim.BindingID {
		t.Fatalf("binding lookup = %+v, want claim %q", got, claim.ID)
	}
	missing, err := js.MaterializationClaimByBindingID(ctx, "missing-binding")
	if err != nil || missing != nil {
		t.Fatalf("missing binding lookup = %+v, %v; want nil", missing, err)
	}
}

func TestAbandonStaleMaterializationsReleasesOnlyOlderLiveClaims(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, err := js.CreateRequest(ctx, "materialization-abandon", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-a", "auth-a")
	stale := institutionalCandidate(t, js, profile, "candidate-stale", jobID)
	current := institutionalCandidate(t, js, profile, "candidate-current", jobID)
	materializing := institutionalCandidate(t, js, profile, "candidate-materializing", jobID)
	terminal := institutionalCandidate(t, js, profile, "candidate-terminal", jobID)
	claimStale, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: stale.ID, BrowserHolderGeneration: 4, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimCurrent, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: current.ID, BrowserHolderGeneration: 5, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimMaterializing, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: materializing.ID, BrowserHolderGeneration: 4, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE browser_candidates SET status='materializing' WHERE id=?`, materializing.ID); err != nil {
		t.Fatal(err)
	}
	claimTerminal, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: terminal.ID, BrowserHolderGeneration: 4, JobAttemptRevision: 1,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE materialization_claims SET phase='settled' WHERE id=?`, claimTerminal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE browser_candidates SET status='succeeded' WHERE id=?`, terminal.ID); err != nil {
		t.Fatal(err)
	}

	count, err := js.AbandonStaleMaterializations(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("abandoned count = %d, want 2", count)
	}
	for _, tc := range []struct {
		id     string
		status string
	}{
		{stale.ID, "eligible"},
		{current.ID, "claimed"},
		{materializing.ID, "eligible"},
		{terminal.ID, "succeeded"},
	} {
		got, getErr := js.GetBrowserCandidate(ctx, tc.id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status != tc.status {
			t.Fatalf("candidate %s status = %q, want %q", tc.id, got.Status, tc.status)
		}
	}
	for _, tc := range []struct {
		id    string
		phase string
	}{
		{claimStale.ID, "abandoned"},
		{claimCurrent.ID, "claimed"},
		{claimMaterializing.ID, "abandoned"},
		{claimTerminal.ID, "settled"},
	} {
		got, getErr := js.GetMaterializationClaim(ctx, tc.id)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Phase != tc.phase {
			t.Fatalf("claim %s phase = %q, want %q", tc.id, got.Phase, tc.phase)
		}
	}
}

// The answer must not depend on claim-id ordering. LiveMaterializationClaimForJob
// takes one row `ORDER BY m.id`, so with two non-terminal claims coexisting -
// the normal state after a re-drive - it names an arbitrary one; a close rule
// built on it authorized retiring the live drive's own surface whenever the
// older claim happened to sort first. Only a strictly newer sibling supersedes,
// so the newest claim is never superseded whichever way the ids fall.
func TestSupersededMaterializationClaimIsOrderIndependent(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, err := js.CreateRequest(ctx, "materialization-superseded", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-sup", "auth-sup")
	claimFor := func(candidateID string, createdAt string) *MaterializationClaim {
		t.Helper()
		candidate := institutionalCandidate(t, js, profile, candidateID, jobID)
		claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
			CandidateID: candidate.ID, BrowserHolderGeneration: 5, JobAttemptRevision: 1,
			InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
			MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		// ClaimMaterialization stamps its own created_at, and two claims in one
		// test can land on the same instant; the ordering under test is
		// (created_at, id), so the fixture states the times explicitly.
		if _, err := js.S.DB().ExecContext(ctx,
			`UPDATE materialization_claims SET created_at=?, phase='navigated' WHERE id=?`, createdAt, claim.ID); err != nil {
			t.Fatal(err)
		}
		claim.CreatedAt = createdAt
		return claim
	}
	older := claimFor("candidate-superseded-older", "2026-08-26T01:00:00Z")
	newer := claimFor("candidate-superseded-newer", "2026-08-26T02:00:00Z")

	for _, tc := range []struct {
		name  string
		claim *MaterializationClaim
		want  bool
	}{
		{name: "older claim is superseded", claim: older, want: true},
		{name: "newest claim is never superseded", claim: newer, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := js.SupersededMaterializationClaim(ctx, tc.claim.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("superseded(%s) = %v, want %v", tc.claim.ID, got, tc.want)
			}
		})
	}

	// A terminal sibling is not a drive, so retiring the survivor's surface must
	// not be authorized by it.
	if _, err := js.S.DB().ExecContext(ctx,
		`UPDATE materialization_claims SET phase='abandoned' WHERE id=?`, newer.ID); err != nil {
		t.Fatal(err)
	}
	got, err := js.SupersededMaterializationClaim(ctx, older.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("an abandoned sibling must not supersede a live claim")
	}
	if got, err := js.SupersededMaterializationClaim(ctx, "claim-that-never-existed"); err != nil || got {
		t.Fatalf("unknown claim = %v, %v; want false, nil", got, err)
	}
}

// A requeue bumps the job attempt revision, so a paper that went round through
// document delivery has its newer drive on a LATER attempt. Scoping the
// comparison to one attempt found no newer sibling, refused every close, and
// left the superseded surface on screen (measured live 2026-08-26). An older
// attempt must never supersede anything.
func TestSupersededMaterializationClaimSpansJobAttempts(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	jobID, err := js.CreateRequest(ctx, "materialization-attempts", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	profile := institutionalProfile(t, js, "library", "digest-att", "auth-att")
	claimAt := func(candidateID string, attempt int64, createdAt string) *MaterializationClaim {
		t.Helper()
		// Claimed at attempt 1 through the real constructor, then re-dated: a
		// live requeue bumps the job's own attempt revision, and this test is
		// about the comparison, not about replaying a requeue.
		if _, err := js.CreateBrowserCandidate(ctx, BrowserCandidateInput{
			ID: candidateID, JobID: jobID, JobAttemptRevision: 1,
			InstitutionProfileID: profile.ID, InstitutionProfileRevision: profile.Revision,
			RouteRevision: 7, RouteClass: "institutional", IdentifierStrategy: "doi",
			PreRouteSafetyKey: "safety-" + candidateID, SafetyDomainID: "domain-" + candidateID,
			AdapterRevision: "adapter-1", EffectContractID: "effect-1", Status: "eligible",
		}); err != nil {
			t.Fatal(err)
		}
		claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
			CandidateID: candidateID, BrowserHolderGeneration: 5, JobAttemptRevision: 1,
			InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
			MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := js.S.DB().ExecContext(ctx,
			`UPDATE materialization_claims SET created_at=?, phase='navigated' WHERE id=?`,
			createdAt, claim.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := js.S.DB().ExecContext(ctx,
			`UPDATE browser_candidates SET job_attempt_revision=? WHERE id=?`, attempt, candidateID); err != nil {
			t.Fatal(err)
		}
		return claim
	}
	first := claimAt("candidate-attempt-1", 1, "2026-08-26T01:00:00Z")
	second := claimAt("candidate-attempt-2", 2, "2026-08-26T02:00:00Z")

	got, err := js.SupersededMaterializationClaim(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("a later attempt's live claim must supersede the earlier attempt's surface")
	}
	got, err = js.SupersededMaterializationClaim(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("an earlier attempt must never supersede the current one")
	}
}
