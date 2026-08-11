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
		ID: id, JobID: jobID, JobAttemptRevision: 3,
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
			CandidateID: candidate.ID, BrowserHolderGeneration: 1, JobAttemptRevision: 3,
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
			CandidateID: candidate.ID, BrowserHolderGeneration: 1, JobAttemptRevision: 3,
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
		CandidateID: candidate.ID, BrowserHolderGeneration: 9, JobAttemptRevision: 3,
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
	nonmatching := input
	nonmatching.BrowserHolderGeneration++
	if _, err := js.ClaimMaterialization(ctx, nonmatching); !errors.Is(err, ErrMaterializationBusy) {
		t.Fatalf("nonmatching holder retry = %v, want busy", err)
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
	expires := time.Now().UTC().Add(20 * time.Millisecond)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{
		CandidateID: candidate.ID, BrowserHolderGeneration: 3, JobAttemptRevision: 3,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, profile.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := js.ReconcileMaterializationClaims(ctx, expires.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 3, profile.Revision); !errors.Is(err, ErrMaterializationStale) {
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

func TestMaterializationClaimCASAndOrdinals(t *testing.T) {
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
		CandidateID: candidate.ID, BrowserHolderGeneration: 11, JobAttemptRevision: 3,
		InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
		MaterializationKind: "browser_tab", LeaseUntil: lease,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 11, profile.Revision); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 11, profile.Revision); err != nil {
		t.Fatalf("binding replay: %v", err)
	}
	if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 10, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("stale holder route = %v, want stale", err)
	}
	routeOrdinal, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 11, 0)
	if err != nil || routeOrdinal != 1 {
		t.Fatalf("route ordinal = %d err=%v, want 1", routeOrdinal, err)
	}
	if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 11, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("non-monotonic route = %v, want stale", err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 11, 1); err != nil {
		t.Fatalf("navigation ack: %v", err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 11, 1); err != nil {
		t.Fatalf("navigation replay: %v", err)
	}
	effect, err := js.AdvanceMaterializationEffect(ctx, claim.ID, claim.BindingID, 11, 0)
	if err != nil || effect != 1 {
		t.Fatalf("effect ordinal = %d err=%v, want 1", effect, err)
	}
	if _, err := js.AdvanceMaterializationEffect(ctx, claim.ID, claim.BindingID, 11, 0); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("non-monotonic effect = %v, want stale", err)
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
	input := MaterializationClaimInput{CandidateID: candidate.ID, JobAttemptRevision: 3, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute)}
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
	expires := time.Now().UTC().Add(20 * time.Millisecond)
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 4, JobAttemptRevision: 3, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "direct_download", LeaseUntil: expires})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := js.ReconcileMaterializationClaims(ctx, expires.Add(time.Millisecond))
	if err != nil || len(expired) != 1 || expired[0].ID != claim.ID {
		t.Fatalf("expired = %+v err=%v", expired, err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, "stale-binding", 4, profile.Revision); !errors.Is(err, ErrMaterializationStale) {
		t.Fatalf("expired bind = %v, want stale", err)
	}
	replacement, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 5, JobAttemptRevision: 3, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "direct_download", LeaseUntil: time.Now().UTC().Add(time.Minute)})
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
	claim, err := js.ClaimMaterialization(ctx, MaterializationClaimInput{CandidateID: candidate.ID, BrowserHolderGeneration: 22, JobAttemptRevision: 3, InstitutionProfileRevision: profile.Revision, RouteRevision: 7, MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 22, profile.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 22, 0); err != nil {
		t.Fatal(err)
	}
	if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 22, 1); err != nil {
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
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO artifact_winners (job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at) VALUES (?, ?, ?, ?, ?, ?)`, jobID, 3, candidate.ID, 21, "sha-generation-wrong", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := js.SettleMaterialization(ctx, claim.ID, claim.BindingID, 22, profile.Revision); !errors.Is(err, ErrMaterializationConflict) {
		t.Fatalf("mismatched generation settle = %v, want conflict", err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `DELETE FROM artifact_winners WHERE job_id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO artifact_winners (job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at) VALUES (?, ?, ?, ?, ?, ?)`, jobID, 3, candidate.ID, 22, "sha-ok", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
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
		{name: "effect"},
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
				CandidateID: candidate.ID, BrowserHolderGeneration: 31, JobAttemptRevision: 3,
				InstitutionProfileRevision: profile.Revision, RouteRevision: 7,
				MaterializationKind: "browser_tab", LeaseUntil: time.Now().UTC().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "route" || tc.name == "navigation" || tc.name == "effect" || tc.name == "settle" {
				if err := js.BindMaterialization(ctx, claim.ID, claim.BindingID, 31, profile.Revision); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "navigation" || tc.name == "effect" || tc.name == "settle" {
				if _, err := js.IssueMaterializationRoute(ctx, claim.ID, claim.BindingID, 31, 0); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "effect" || tc.name == "settle" {
				if err := js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 31, 1); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "settle" {
				if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO artifact_winners
					(job_id, job_attempt_revision, candidate_id, browser_holder_generation, sha256, created_at)
					VALUES (?, ?, ?, ?, ?, ?)`, jobID, 3, candidate.ID, 31, "sha-drift", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
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
				mutationErr = js.AcknowledgeMaterializationNavigation(ctx, claim.ID, claim.BindingID, 31, 1)
			case "renew":
				mutationErr = js.RenewMaterializationClaim(ctx, claim.ID, 31, time.Now().UTC().Add(2*time.Minute))
			case "effect":
				_, mutationErr = js.AdvanceMaterializationEffect(ctx, claim.ID, claim.BindingID, 31, 0)
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
			if tc.name == "route" {
				wantPhase = "bound"
			} else if tc.name == "navigation" || tc.name == "effect" || tc.name == "settle" {
				wantPhase = "route_issued"
				if tc.name == "effect" || tc.name == "settle" {
					wantPhase = "navigated"
				}
			}
			if got.Phase != wantPhase {
				t.Fatalf("%s mutated phase to %q, want %q", tc.name, got.Phase, wantPhase)
			}
		})
	}
}
