// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"testing"

	"papio/internal/job"
	"papio/internal/protocol"
)

const (
	vacancyHolderSession  = "sess-vacancy-holder-00000000000000001"
	vacancyPendingSession = "sess-vacancy-pending-0000000000000001"
	vacancyGrabRequestID  = "vacancy-grab-request-0001"
	vacancyGrabHost       = "vacancy.example.org"
)

type vacancyGenerationFixture struct {
	bridge           *Bridge
	jobs             *job.Store
	holderGeneration int64
}

func newVacancyGenerationFixture(t *testing.T) vacancyGenerationFixture {
	t.Helper()
	b, jobs, _, _ := newBridge(t)
	hello := func(sessionID string) {
		runSyncAs(t, b, sessionID, helloWithFeatures(t, "0.15.0", pdfGrabV1Feature, effectPermitFeature))
	}
	hello(vacancyHolderSession)
	holderGeneration := b.arbitration.generation()
	if holderGeneration < 1 {
		t.Fatalf("holder generation = %d, want a durable positive generation", holderGeneration)
	}
	hello(vacancyPendingSession)
	if b.arbitration.holderSession() == nil || b.arbitration.holderSession().ID != vacancyHolderSession {
		t.Fatalf("holder = %+v, want %s before departure", b.arbitration.holderSession(), vacancyHolderSession)
	}

	if _, _, err := b.RequestDevReload(""); err != nil {
		t.Fatalf("request dev reload: %v", err)
	}
	if messages, _ := runSyncAs(t, b, vacancyHolderSession); firstOfType(messages, protocol.MsgDevReload) == nil {
		t.Fatalf("holder poll did not emit dev_reload: %v", messages)
	}
	if _, err := b.Sync(context.Background(), vacancyHolderSession, true, nil); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	if b.arbitration.holderSession() != nil {
		t.Fatalf("holder after release = %+v, want vacancy", b.arbitration.holderSession())
	}
	if generation := b.arbitration.generation(); generation != 0 {
		t.Fatalf("vacancy generation = %d, want sentinel 0", generation)
	}

	return vacancyGenerationFixture{bridge: b, jobs: jobs, holderGeneration: holderGeneration}
}

func TestVacancyGenerationPermitIsNotResumedByNextHolder(t *testing.T) {
	fixture := newVacancyGenerationFixture(t)
	initial := pdfGrabVia(t, fixture.bridge, vacancyPendingSession, vacancyGrabRequestID, vacancyGrabHost)
	if initial.Outcome != "steering" {
		t.Fatalf("vacancy allocation outcome = %q, want steering", initial.Outcome)
	}
	if fixture.bridge.arbitration.holderSession() != nil {
		t.Fatalf("vacancy request promoted a holder: %+v", fixture.bridge.arbitration.holderSession())
	}
	var permitGeneration int64
	if err := fixture.jobs.S.DB().QueryRowContext(context.Background(),
		`SELECT browser_holder_generation FROM effect_permits WHERE grab_id=?`, initial.GrabID).
		Scan(&permitGeneration); err != nil {
		t.Fatalf("vacancy permit generation: %v", err)
	}
	if permitGeneration != 0 {
		t.Fatalf("vacancy permit generation = %d, want non-holder sentinel 0", permitGeneration)
	}

	runSyncAs(t, fixture.bridge, vacancyPendingSession,
		helloWithFeatures(t, "0.15.0", pdfGrabV1Feature, effectPermitFeature))
	if got := fixture.bridge.arbitration.generation(); got != fixture.holderGeneration+1 {
		t.Fatalf("next holder generation = %d, want %d", got, fixture.holderGeneration+1)
	}

	resumed := pdfGrabVia(t, fixture.bridge, vacancyPendingSession, vacancyGrabRequestID, vacancyGrabHost)
	if resumed.Outcome != "existing" {
		t.Fatalf("vacancy permit resumed with outcome %q, want existing", resumed.Outcome)
	}
}

func TestVacancyPdfGrabRemainsAdmitted(t *testing.T) {
	fixture := newVacancyGenerationFixture(t)
	result := pdfGrabVia(t, fixture.bridge, vacancyPendingSession, "vacancy-behaviour-0001", "vacancy-behaviour.example.org")
	if result.Outcome != "steering" || result.GrabID == "" || result.Reason != "" {
		t.Fatalf("vacancy pdf_grab_request = %+v, want success with steering outcome and no refusal reason", result)
	}
	if fixture.bridge.arbitration.holderSession() != nil {
		t.Fatalf("vacancy request promoted a holder: %+v", fixture.bridge.arbitration.holderSession())
	}
}
