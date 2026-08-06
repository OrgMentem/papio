// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
)

// A DOI that parses is not a DOI that exists. `10.1016/j.cedpsych.2020.101816`
// was a real submission: one transposed digit off the article the user meant
// (…101860). Crossref, OpenAlex, EuropePMC, and Unpaywall each answered
// "candidates=0" — the same answer they give for a paywalled article — so the
// job reached the institutional handoff, the link resolver had nothing to
// match, and the user was dropped on doi.org's "DOI NOT FOUND" page. The action
// could never be completed, so it re-offered on every session-live tick for
// three days (≈60 tabs) and escalated seven reminders.
//
// The registry probe is the only signal that separates "nobody registered this"
// from "nobody will give it to you for free".
type fakeDOIRegistry struct {
	registered bool
	err        error
	calls      []string
}

func (f *fakeDOIRegistry) Registered(_ context.Context, doi string) (bool, error) {
	f.calls = append(f.calls, doi)
	return f.registered, f.err
}

func TestUnregisteredDOIIsUnavailableRatherThanAnInstitutionalHandoff(t *testing.T) {
	svc, jobs := exhaustionService(t)
	registry := &fakeDOIRegistry{}
	svc.DOIRegistry = registry

	got := processOnce(t, svc, jobs, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_doi_typo",
		Identifiers:    &protocol.Identifiers{DOI: "10.1016/j.cedpsych.2020.101816"},
		Title:          "Intrinsic and extrinsic motivation from a self-determination theory perspective",
		DesiredVersion: "any",
	})

	if got.State != job.StateUnavailable {
		t.Fatalf("state = %q, want %q — an unregistered DOI must not be routed to an institutional sign-in", got.State, job.StateUnavailable)
	}
	if got.TerminalReason != string(job.TerminalReasonDOINotRegistered) {
		t.Fatalf("terminal reason = %q, want %q", got.TerminalReason, job.TerminalReasonDOINotRegistered)
	}
	// The point of the whole fix: no action row, so nothing for the bridge to
	// re-offer and nothing for the reminder pass to escalate.
	actions, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %+v, want none for a DOI that resolves to nothing", actions)
	}
	if len(registry.calls) != 1 || registry.calls[0] != "10.1016/j.cedpsych.2020.101816" {
		t.Fatalf("registry calls = %v, want exactly the job's DOI", registry.calls)
	}
}

func TestRegisteredDOIKeepsItsInstitutionalHandoff(t *testing.T) {
	svc, jobs := exhaustionService(t)
	svc.DOIRegistry = &fakeDOIRegistry{registered: true}

	got := processOnce(t, svc, jobs, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_doi_registered",
		Identifiers:    &protocol.Identifiers{DOI: "10.1016/j.cedpsych.2020.101860"},
		Title:          "Intrinsic and extrinsic motivation from a self-determination theory perspective",
		DesiredVersion: "any",
	})

	if got.State != job.StateAwaitingHuman {
		t.Fatalf("state = %q, want %q — a registered DOI behind a paywall is exactly what a handoff is for", got.State, job.StateAwaitingHuman)
	}
}

func TestUnreachableDOIRegistryStillOffersTheHandoff(t *testing.T) {
	// Fail open. During a registry outage another handoff costs one tab; the
	// other direction terminates every paywalled job in the queue.
	svc, jobs := exhaustionService(t)
	svc.DOIRegistry = &fakeDOIRegistry{err: errors.New("dial tcp: connection refused")}

	got := processOnce(t, svc, jobs, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_doi_registry_down",
		Identifiers:    &protocol.Identifiers{DOI: "10.1016/j.cedpsych.2020.101860"},
		Title:          "Intrinsic and extrinsic motivation from a self-determination theory perspective",
		DesiredVersion: "any",
	})

	if got.State != job.StateAwaitingHuman {
		t.Fatalf("state = %q, want %q — a probe failure means unknown, not unregistered", got.State, job.StateAwaitingHuman)
	}
}

func TestUnregisteredDOIWithAnotherIdentifierIsNotProbed(t *testing.T) {
	// Each sibling identifier is its own acquisition route, so the DOI's
	// registration is not the deciding fact and the probe must not spend a
	// request on it. All three are exercised because handoffGate's guard is a
	// disjunction: covering only PMID would let a dropped ArXiv or OpenAlex
	// clause terminate a perfectly routable job as doi_not_registered.
	for name, ids := range map[string]*protocol.Identifiers{
		"pmid":     {DOI: "10.1016/j.cedpsych.2020.101816", PMID: "32001929"},
		"arxiv":    {DOI: "10.1016/j.cedpsych.2020.101816", ArXiv: "2401.00001"},
		"openalex": {DOI: "10.1016/j.cedpsych.2020.101816", OpenAlex: "W2741809807"},
	} {
		t.Run(name, func(t *testing.T) {
			svc, jobs := exhaustionService(t)
			registry := &fakeDOIRegistry{}
			svc.DOIRegistry = registry

			got := processOnce(t, svc, jobs, protocol.WorkRequest{
				SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_doi_plus_" + name,
				Identifiers:    ids,
				Title:          "Intrinsic and extrinsic motivation from a self-determination theory perspective",
				DesiredVersion: "any",
			})

			if got.State != job.StateAwaitingHuman {
				t.Fatalf("state = %q, want %q — the %s still gives a login something to resolve", got.State, job.StateAwaitingHuman, name)
			}
			if len(registry.calls) != 0 {
				t.Fatalf("registry calls = %v, want none when another fetchable identifier is present", registry.calls)
			}
		})
	}
}

func TestRepairHealsAPreExistingHandoffForAnUnregisteredDOI(t *testing.T) {
	// The reported incident's own shape: the job was ALREADY parked when the
	// gate shipped, so only the repair pass can reach it. Rule 2 never can —
	// it waits on browser.no_entitlement_requeue, which requires the browser
	// to have reached the institutional resolver, and a dead DOI never gets
	// that far.
	svc, jobs := exhaustionService(t)
	svc.DOIRegistry = &fakeDOIRegistry{}
	ctx := context.Background()
	id, err := svc.Submit(ctx, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_legacy_typo_park",
		Identifiers:    &protocol.Identifiers{DOI: "10.1016/j.cedpsych.2020.101816"},
		Title:          "Intrinsic and extrinsic motivation from a self-determination theory perspective",
		DesiredVersion: "any",
	})
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("state = %q, want %q — a park nothing can complete must be reclaimed", got.State, job.StateResolving)
	}
	open, _ := jobs.ListHumanActions(ctx, true)
	if len(open) != 0 {
		t.Fatalf("open actions = %+v, want the dead handoff resolved", open)
	}

	row, err := jobs.ClaimNext(ctx, "w", time.Minute)
	if err != nil || row == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	settled, _ := jobs.Get(ctx, id)
	if settled.State != job.StateUnavailable || settled.TerminalReason != string(job.TerminalReasonDOINotRegistered) {
		t.Fatalf("settled = state:%q reason:%q, want unavailable/%s", settled.State, settled.TerminalReason, job.TerminalReasonDOINotRegistered)
	}
}
