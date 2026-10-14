// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package browser

import (
	"context"
	"slices"
	"strings"
	"testing"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

// An institution with an OpenAthens domain gets the DOI offered through its
// redirector first. The offer, the safety domain and the candidate agree on one
// DOI-route fence, and a failed redirector drive falls back to the
// institution's OpenURL route in a new attempt, exactly like the plain DOI.
func TestOpenAthensRedirectorOfferFallsBackToOpenURL(t *testing.T) {
	for _, outcome := range []protocol.ProviderOutcomePayload{
		{Outcome: "ui_changed", Detail: "no adapter claimed this page"},
		{Outcome: "ui_changed", AdapterID: "sciencedirect", AdapterVersion: "0.1.0", Detail: "Article agent fallback stopped [identity_missing]"},
		{Outcome: "no_entitlement", AdapterID: "sciencedirect", AdapterVersion: "0.1.0"},
	} {
		t.Run(outcome.Outcome+"/"+outcome.AdapterID, func(t *testing.T) {
			b, jobs, cfg, _ := newBridgeWithHoldingsAndZotio(t, nil, nil, func(c *config.Config) {
				c.Browser.OpenAthensDomain = "example.edu"
			})
			ctx := context.Background()
			w := work.Work{DOI: "10.1016/j.jval.2022.04.1227", Title: "A health technology assessment", Authors: []string{"Example, A."}, Year: 2022}
			id := parkInstitutional(t, jobs, "openathens-first", w, "")
			if err := jobs.RecordEvent(ctx, id, app.PublisherFirstEventKind, map[string]any{"adapter_id": "sciencedirect"}); err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, job.PublisherFirstHandoffDetail, job.Access(true, "paywall")); err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			runSync(t, b, helloWithFeatures(t, "0.21.0", institutionalMaterializationFeature, effectPermitFeature, "provider_drive_epoch_v1"))

			first, err := b.openHandoffForJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			const wantURL = "https://go.openathens.net/redirector/example.edu?url=https%3A%2F%2Fdoi.org%2F10.1016%2Fj.jval.2022.04.1227"
			raw, err := b.offer(*row, *first, config.ModeDelegated)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			offer := msg.Payload.(*protocol.JobOfferPayload)
			if offer.OpenURL != wantURL || offer.DriveAttemptID == "" {
				t.Fatalf("first offer = %+v, want a DOI drive at %s", offer, wantURL)
			}
			if !slices.Contains(offer.ProviderHosts, "go.openathens.net") {
				t.Fatalf("offer provider hosts = %v, want the redirector host", offer.ProviderHosts)
			}
			if got := actionSafetyDomain(b.cfg, *row, *first); got != "publisher:doi.org" {
				t.Fatalf("redirector offer safety domain = %q, want the shared DOI-route fence", got)
			}
			// The candidate minted for this action carries the same fence, so
			// the scheduler's sibling anti-join and the effect permit agree.
			if _, err := b.prepareMaterializationCandidate(ctx, *row); err != nil {
				t.Fatal(err)
			}
			if candidate, err := jobs.CurrentBrowserCandidateForJob(ctx, id, 1); err != nil || candidate == nil || candidate.SafetyDomainID != "publisher:doi.org" {
				t.Fatalf("redirector candidate = %+v err=%v, want safety domain publisher:doi.org", candidate, err)
			}

			payload := outcome
			if err := b.outcome(ctx, id, "openathens-first-"+outcome.Outcome, &payload); err != nil {
				t.Fatal(err)
			}
			next, err := b.openHandoffForJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if next.Kind != handoffActionKind || next.Detail != app.InstitutionalOpenURLHandoffDetail {
				t.Fatalf("after the redirector %s the open action = %+v, want the institutional handoff", outcome.Outcome, next)
			}
			if got := b.handoffURL(*row, *next); !strings.HasPrefix(got, cfg.Browser.OpenURLBase+"?") {
				t.Fatalf("fallback offer URL = %q, want the OpenURL resolver", got)
			}
			if attempt, err := jobs.MaterializationAttemptRevision(ctx, id); err != nil || attempt != 2 {
				t.Fatalf("attempt = %d err=%v, want the OpenURL route in a new attempt", attempt, err)
			}
			if state, err := jobs.Get(ctx, id); err != nil || state.State != job.StateAwaitingHuman {
				t.Fatalf("state = %+v err=%v, want awaiting_human", state, err)
			}
		})
	}
}

// The OpenAthens domain is routing authority: changing the tenant must bump the
// profile revision, which retires evidence and route suppressions earned under
// the old value. A profile that does not set the field keeps the digest it had
// before the field existed, so upgrading bumps no revision.
func TestOpenAthensDomainIsProfileAuthorityWithoutChurningOtherProfiles(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")
	profile := func() job.InstitutionProfile {
		t.Helper()
		if err := b.reconcileMaterializationProfiles(ctx, key); err != nil {
			t.Fatal(err)
		}
		profiles, err := jobs.ListInstitutionProfiles(ctx, false)
		if err != nil || len(profiles) != 1 {
			t.Fatalf("profiles = %+v err=%v, want exactly the default profile", profiles, err)
		}
		return profiles[0]
	}
	base := b.cfg.Browser.OpenURLBase
	// The digest every profile had before the field existed.
	legacy := materializationMAC(key, "profile_authority", "default", base, "", "", "", "0")

	first := profile()
	if first.AuthorityDigest != legacy {
		t.Fatalf("a profile without openathens_domain changed digest: %q, want the pre-field %q", first.AuthorityDigest, legacy)
	}
	if again := profile(); again.Revision != first.Revision {
		t.Fatalf("an unchanged profile bumped revision %d -> %d", first.Revision, again.Revision)
	}

	b.cfg.Browser.OpenAthensDomain = "example.edu"
	withDomain := profile()
	if withDomain.Revision != first.Revision+1 || withDomain.AuthorityDigest == legacy {
		t.Fatalf("setting the domain: revision %d -> %d, digest changed=%v; want a bump", first.Revision, withDomain.Revision, withDomain.AuthorityDigest != legacy)
	}
	b.cfg.Browser.OpenAthensDomain = "other.example.edu"
	if moved := profile(); moved.Revision != withDomain.Revision+1 || moved.AuthorityDigest == withDomain.AuthorityDigest {
		t.Fatalf("changing the tenant: revision %d -> %d; want a bump", withDomain.Revision, moved.Revision)
	}
	b.cfg.Browser.OpenAthensDomain = ""
	if cleared := profile(); cleared.AuthorityDigest != legacy {
		t.Fatalf("clearing the domain left digest %q, want the pre-field %q", cleared.AuthorityDigest, legacy)
	}
}
