// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/resolver"
)

// A measured 27% of one real 112-item missing-PDF backlog was books, chapters,
// reports, and theses with no DOI. Every one of them used to park as an
// institutional OpenURL handoff, because exhaustedCandidates asked only whether
// a resolver base was configured. That told the user to spend an SSO round trip
// on a printed monograph the library can only hand back as a catalogue record,
// left the job parked forever, and — now that open human actions are re-notified
// on an escalating schedule — turned each one into a recurring reminder about
// work no login can finish.
//
// The gate is a fetchable identifier, NOT the item type: a book chapter with a
// Springer DOI resolves normally and must keep its handoff.
func exhaustionService(t *testing.T) (*Service, *job.Store) {
	t.Helper()
	svc, jobs := newTestService(t)
	svc.Config.AccessMode = config.ModeDelegated
	svc.Config.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	// Resolve produces nothing, which is the exhaustion boundary under test.
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{}},
		Policy: config.Source{Enabled: true}}}
	fetches := 0
	svc.Fetch = fakeDownload(&fetches)
	svc.Validate = passValidation()
	return svc, jobs
}

func processOnce(t *testing.T, svc *Service, jobs *job.Store, request protocol.WorkRequest) *job.Row {
	t.Helper()
	ctx := context.Background()
	id, err := svc.Submit(ctx, request)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatalf("process: %v", err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return got
}

func TestTitleOnlyWorkIsUnavailableRatherThanAnInstitutionalHandoff(t *testing.T) {
	svc, jobs := exhaustionService(t)
	got := processOnce(t, svc, jobs, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_no_identifier",
		Title: "Evaluating training programs: the four levels", Authors: []string{"Donald L. Kirkpatrick"},
		Year: 2012, DesiredVersion: "any",
	})

	if got.State != job.StateUnavailable {
		t.Fatalf("state = %q, want %q — a bare title must not be routed to an institutional sign-in", got.State, job.StateUnavailable)
	}
	if got.TerminalReason != "no_identifier" {
		t.Fatalf("terminal reason = %q, want no_identifier", got.TerminalReason)
	}
	// The whole point: no action row, so no handoff queue entry, no SSO tab
	// offered over the bridge, and nothing for the reminder pass to escalate.
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none for an unfetchable work", actions)
	}
}

func TestISBNAloneDoesNotEarnAnInstitutionalHandoff(t *testing.T) {
	// ISBN satisfies work.HasIdentifier, so gating on that would have quietly
	// undone this fix the moment papio started carrying ISBNs out of Zotero.
	svc, jobs := exhaustionService(t)
	got := processOnce(t, svc, jobs, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_isbn_only",
		Identifiers: &protocol.Identifiers{ISBN: "9781576753484"},
		Title:       "Evaluating training programs: the four levels", DesiredVersion: "any",
	})

	if got.State != job.StateUnavailable || got.TerminalReason != "no_identifier" {
		t.Fatalf("isbn-only result = state:%q reason:%q, want unavailable/no_identifier", got.State, got.TerminalReason)
	}
	actions, _ := jobs.ListHumanActions(context.Background(), true)
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none — no resolver consumes an ISBN", actions)
	}
}

func TestDOIWorkKeepsItsInstitutionalHandoff(t *testing.T) {
	// The regression guard for the gate itself: a chapter or article with a real
	// identifier is exactly the case an institutional sign-in does resolve.
	svc, jobs := exhaustionService(t)
	got := processOnce(t, svc, jobs, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_doi_handoff",
		Identifiers: &protocol.Identifiers{DOI: "10.1007/978-1-4613-3087-5_2"},
		Title:       "What should be done with equity theory?", DesiredVersion: "any",
	})

	if got.State != job.StateAwaitingHuman {
		t.Fatalf("state = %q, want %q for a DOI-anchored work", got.State, job.StateAwaitingHuman)
	}
	actions, _ := jobs.ListHumanActions(context.Background(), true)
	if len(actions) != 1 || actions[0].Kind != "openurl_handoff" || !actions[0].RequiresAuth {
		t.Fatalf("actions = %+v, want one auth-requiring openurl_handoff", actions)
	}
}

func TestConservativeModeAlsoWithholdsAnUnusableOpenURL(t *testing.T) {
	// Conservative mode records an informational openurl_available action. An
	// OpenURL built from a bare title is not worth recording either.
	svc, jobs := exhaustionService(t)
	svc.Config.AccessMode = config.ModeConservative
	got := processOnce(t, svc, jobs, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_conservative_no_id",
		Title: "A printed monograph", Authors: []string{"A. Author"}, Year: 1999, DesiredVersion: "any",
	})

	if got.TerminalReason != "no_identifier" {
		t.Fatalf("terminal reason = %q, want no_identifier", got.TerminalReason)
	}
	actions, _ := jobs.ListHumanActions(context.Background(), true)
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none", actions)
	}
}

// Parks created before the identifier gate existed sit in awaiting_human with
// an open institutional handoff forever, and the reminder pass now escalates
// each one on a schedule. Upgrading must heal them, not leave the user to find
// and cancel each by hand.
func TestRepairHealsAPreExistingHandoffForAnUnfetchableWork(t *testing.T) {
	svc, jobs := exhaustionService(t)
	ctx := context.Background()
	id, err := svc.Submit(ctx, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_legacy_park",
		Title: "A printed monograph", Authors: []string{"A. Author"}, Year: 1999, DesiredVersion: "any",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the pre-fix state: parked awaiting_human on an institutional
	// handoff that no sign-in can complete.
	if _, err := jobs.OpenHumanAction(ctx, id, "openurl_handoff", InstitutionalOpenURLHandoffDetail,
		job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving,
		map[string]any{"reason": "scheduler_dispatch"}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, id, job.StateResolving, job.StateAwaitingHuman,
		map[string]any{"reason": "institutional_handoff"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
		t.Fatalf("repair: %v", err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateResolving {
		t.Fatalf("state = %q, want %q — repair returns the job to the one gate that classifies it", got.State, job.StateResolving)
	}
	open, _ := jobs.ListHumanActions(ctx, true)
	if len(open) != 0 {
		t.Fatalf("open actions = %+v, want the dead handoff resolved", open)
	}

	// And the reclaimed job settles as unavailable, never back into a handoff.
	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	settled, _ := jobs.Get(ctx, id)
	if settled.State != job.StateUnavailable || settled.TerminalReason != "no_identifier" {
		t.Fatalf("settled = state:%q reason:%q, want unavailable/no_identifier", settled.State, settled.TerminalReason)
	}
}

// The repair must not touch a park a sign-in can still finish.
func TestRepairLeavesAFetchableHandoffAlone(t *testing.T) {
	svc, jobs := exhaustionService(t)
	ctx := context.Background()
	got := processOnce(t, svc, jobs, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_keep_park",
		Identifiers: &protocol.Identifiers{DOI: "10.1007/978-1-4613-3087-5_2"}, DesiredVersion: "any",
	})
	if got.State != job.StateAwaitingHuman {
		t.Fatalf("precondition: state = %q", got.State)
	}
	if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := jobs.Get(ctx, got.ID)
	if after.State != job.StateAwaitingHuman {
		t.Fatalf("state = %q, want the handoff preserved", after.State)
	}
	open, _ := jobs.ListHumanActions(ctx, true)
	if len(open) != 1 {
		t.Fatalf("open actions = %+v, want the handoff preserved", open)
	}
}
