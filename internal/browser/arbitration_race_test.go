// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/job"
)

// TestAdoptionFenceReadsHolderStateAgainstHolderTransitions pins the one
// place the bridge reads arbitration state without b.mu. Adoption ingestion
// (ingestAdoptedFile and everything under it) runs outside the session lock by
// design, so weighArtifact and commitArtifact cannot take it, yet both fence
// the artifact winner on the current holder generation. Holder transitions
// rewrite that generation under b.mu on every hello, promotion and release.
// This drives both sides at once; it is a data-race proof, so it only has
// teeth under -race (the repo's standard test mode). The generation source is
// stubbed so the writer shares no store lock with the reader: a shared lock
// would order the two sides by accident and hide the race.
func TestAdoptionFenceReadsHolderStateAgainstHolderTransitions(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	runSync(t, b, hello())
	jobID := parkInstitutional(t, jobs, "wr_holder_race", handoffWork(), "")
	profiles, err := jobs.ReconcileInstitutionProfiles(ctx, []job.InstitutionProfileSpec{{
		ConfiguredName: "default", AuthorityDigest: "digest-holder-race", AuthenticationClaimID: "auth-holder-race",
	}})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profile reconcile: %+v %v", profiles, err)
	}
	if _, err := jobs.CreateBrowserCandidate(ctx, job.BrowserCandidateInput{
		ID: "candidate-holder-race", JobID: jobID, JobAttemptRevision: 1,
		InstitutionProfileID: profiles[0].ID, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, RouteClass: "institutional", IdentifierStrategy: "doi",
		PreRouteSafetyKey: "safety", SafetyDomainID: "domain",
		AdapterRevision: "adapter", EffectContractID: "effect", Status: "eligible",
	}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	generation := b.arbitration.generation()
	b.mu.Unlock()
	if _, err := jobs.ClaimMaterialization(ctx, job.MaterializationClaimInput{
		CandidateID: "candidate-holder-race", BrowserHolderGeneration: generation,
		JobAttemptRevision: 1, InstitutionProfileRevision: profiles[0].Revision,
		RouteRevision: 1, MaterializationKind: "browser_tab",
		LeaseUntil: time.Now().UTC().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), jobID, "paper.pdf"))

	// Every third allocation fails, so applyPromotion also flips
	// materializationGenerationUnavailable, the flag weighArtifact reads next
	// to the generation.
	var allocations atomic.Int64
	b.mu.Lock()
	b.arbitration.nextEpoch = func() (int64, error) {
		n := allocations.Add(1)
		if n%3 == 0 {
			return 0, errors.New("generation store unavailable")
		}
		return n, nil
	}
	b.mu.Unlock()

	const rounds = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range rounds {
			fence, err := b.weighArtifact(ctx, jobID, "paper.pdf")
			if err != nil {
				t.Errorf("weigh: %v", err)
				return
			}
			if err := b.commitArtifact(ctx, jobID, "paper.pdf", fence, nil); err != nil {
				t.Errorf("commit: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range rounds {
			session := &browserSession{ID: sessA, ExtensionVersion: "1.2.3"}
			if i%2 == 1 {
				session.ID = sessB
			}
			b.mu.Lock()
			promoteForTest(b, session, "holder churn")
			b.mu.Unlock()
			b.mu.Lock()
			b.release(session.ID)
			b.mu.Unlock()
		}
	}()
	wg.Wait()
}
