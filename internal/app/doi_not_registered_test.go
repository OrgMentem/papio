// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"testing"

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
	svc.DOIs = registry

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
	svc.DOIs = &fakeDOIRegistry{registered: true}

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
	svc.DOIs = &fakeDOIRegistry{err: errors.New("dial tcp: connection refused")}

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
	// A PMID is its own acquisition route, so the DOI's registration is not the
	// deciding fact and the probe must not spend a request on it.
	svc, jobs := exhaustionService(t)
	registry := &fakeDOIRegistry{}
	svc.DOIs = registry

	got := processOnce(t, svc, jobs, protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_doi_plus_pmid",
		Identifiers:    &protocol.Identifiers{DOI: "10.1016/j.cedpsych.2020.101816", PMID: "32001929"},
		Title:          "Intrinsic and extrinsic motivation from a self-determination theory perspective",
		DesiredVersion: "any",
	})

	if got.State != job.StateAwaitingHuman {
		t.Fatalf("state = %q, want %q — the PMID still gives a login something to resolve", got.State, job.StateAwaitingHuman)
	}
	if len(registry.calls) != 0 {
		t.Fatalf("registry calls = %v, want none when another fetchable identifier is present", registry.calls)
	}
}
