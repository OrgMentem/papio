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

// parkPublisherFirst parks a job exactly as app.exhaustedCandidates does for a
// DOI whose packaged adapter is offered before the resolver.
func parkPublisherFirst(t *testing.T, jobs *job.Store, reqID string) (string, work.Work) {
	t.Helper()
	ctx := context.Background()
	w := work.Work{DOI: "10.1176/appi.ajp.2010.09111680", Title: "Personalized Medicine for Depression", Authors: []string{"Simon, Gregory E."}, Year: 2010}
	id := parkInstitutional(t, jobs, reqID, w, "")
	// An earlier pass of this job may already hold a resolver drive epoch.
	if err := jobs.RecordEvent(ctx, id, "browser.provider_drive_epoch_offered", map[string]any{
		"drive_attempt_id": "earlier-resolver-attempt", "ordinal": int64(0),
		"strategy": "generic", "revision": "1", "safety_domain": "institution:openurl.example.edu",
	}); err != nil {
		t.Fatal(err)
	}
	if err := jobs.RecordEvent(ctx, id, app.PublisherFirstEventKind, map[string]any{"adapter_id": "psychiatryonline"}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, job.PublisherFirstHandoffDetail, job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	return id, w
}

// The DOI route is offered first, under its own safety domain and a fresh
// drive epoch; when it fails, the institutional resolver is the next offer in
// a new attempt, not a manual download and not a second DOI attempt.
func TestPublisherFirstFailureFallsBackToInstitutionalRoute(t *testing.T) {
	for _, outcome := range []protocol.ProviderOutcomePayload{
		{Outcome: "ui_changed", AdapterID: "psychiatryonline", AdapterVersion: "0.1.1", Detail: "Article agent fallback stopped [identity_missing]"},
		{Outcome: "wrong_work", AdapterID: "psychiatryonline", AdapterVersion: "0.1.1"},
		{Outcome: "no_entitlement", AdapterID: "psychiatryonline", AdapterVersion: "0.1.1"},
	} {
		t.Run(outcome.Outcome, func(t *testing.T) {
			b, jobs, cfg, _ := newBridge(t)
			ctx := context.Background()
			id, w := parkPublisherFirst(t, jobs, "publisher-first-"+outcome.Outcome)
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			runSync(t, b, helloWithFeatures(t, "0.21.0", institutionalMaterializationFeature, effectPermitFeature, "provider_drive_epoch_v1"))

			first, err := b.openHandoffForJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			doiURL := "https://doi.org/" + w.DOI
			raw, err := b.offer(*row, *first, config.ModeDelegated)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			offer := msg.Payload.(*protocol.JobOfferPayload)
			if offer.OpenURL != doiURL || offer.DriveAttemptID == "" || offer.DriveAttemptID == "earlier-resolver-attempt" {
				t.Fatalf("first offer = %+v, want a fresh DOI drive at %s", offer, doiURL)
			}
			if got := actionSafetyDomain(b.cfg, *row, *first); got != "publisher:doi.org" {
				t.Fatalf("first offer domain = %q", got)
			}
			doiAttempt := offer.DriveAttemptID

			payload := outcome
			if err := b.outcome(ctx, id, "publisher-first-"+outcome.Outcome, &payload); err != nil {
				t.Fatal(err)
			}
			next, err := b.openHandoffForJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if next.Kind != handoffActionKind || next.Detail != app.InstitutionalOpenURLHandoffDetail {
				t.Fatalf("after DOI %s the open action = %+v, want the institutional handoff", outcome.Outcome, next)
			}
			if got := b.handoffURL(*row, *next); !strings.HasPrefix(got, cfg.Browser.OpenURLBase+"?") {
				t.Fatalf("second offer URL = %q, want the resolver", got)
			}
			if attempt, err := jobs.MaterializationAttemptRevision(ctx, id); err != nil || attempt != 2 {
				t.Fatalf("attempt = %d err=%v, want the resolver in a new attempt", attempt, err)
			}
			if state, err := jobs.Get(ctx, id); err != nil || state.State != job.StateAwaitingHuman {
				t.Fatalf("state = %+v err=%v, want awaiting_human", state, err)
			}
			if latched, err := b.browserOfferLatched(ctx, *row, *next); err != nil || latched {
				t.Fatalf("DOI failure latched the resolver route=%v err=%v", latched, err)
			}
			if _, err := b.prepareMaterializationCandidate(ctx, *row); err != nil {
				t.Fatal(err)
			}
			candidate, err := jobs.CurrentBrowserCandidateForJob(ctx, id, 2)
			if err != nil || candidate == nil || candidate.SafetyDomainID == "publisher:doi.org" {
				t.Fatalf("resolver candidate=%+v err=%v", candidate, err)
			}
			raw, err = b.offer(*row, *next, config.ModeDelegated)
			if err != nil {
				t.Fatal(err)
			}
			msg, err = protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			second := msg.Payload.(*protocol.JobOfferPayload)
			if !strings.HasPrefix(second.OpenURL, cfg.Browser.OpenURLBase+"?") || second.DriveAttemptID == "" || second.DriveAttemptID == doiAttempt {
				t.Fatalf("resolver drive offer = %+v, want a fresh epoch", second)
			}

			// The resolver is the last candidate: its failure is an ordinary
			// manual download, never a return to the DOI.
			if outcome.Outcome == "no_entitlement" {
				return
			}
			if err := b.outcome(ctx, id, "resolver-"+outcome.Outcome, &payload); err != nil {
				t.Fatal(err)
			}
			last, err := b.openHandoffForJob(ctx, id)
			if err != nil || last.Kind != "manual_download" || job.IsPublisherHandoff(*last) {
				t.Fatalf("resolver failure action=%+v err=%v, want an institutional manual download", last, err)
			}
		})
	}
}

// A publisher-route offer sits on a publisher or redirector host and names no
// institution, so the extension could not bind it to the institution whose
// session evidence may release it, and parked a fresh publisher-first offer
// queued with no tab although the session was live (papio-d5e6cb6750e74b8f).
// The offer keeps its job's resolver host beside the route, last: the first
// host names the route and is where the extension attributes an unattributed
// challenge. A resolver-route offer already names its own host and gains no
// second copy.
func TestPublisherRouteOfferKeepsItsJobsResolverHostLast(t *testing.T) {
	const otherResolverHost = "other-openurl.example.edu"
	for _, tc := range []struct {
		name       string
		openAthens string
		profile    string
		publisher  bool
		wantFirst  string
		wantLast   string
	}{
		{name: "plain DOI route", publisher: true, wantFirst: "doi.org", wantLast: "openurl.example.edu"},
		{name: "OpenAthens redirector route", openAthens: "example.edu", publisher: true, wantFirst: "go.openathens.net", wantLast: "openurl.example.edu"},
		{name: "named resolver profile", profile: "other", publisher: true, wantFirst: "doi.org", wantLast: otherResolverHost},
		{name: "resolver route", publisher: false, wantFirst: "openurl.example.edu", wantLast: "openurl.example.edu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, _, _ := newBridgeWithHoldingsAndZotio(t, nil, nil, func(c *config.Config) {
				c.Browser.OpenAthensDomain = tc.openAthens
				c.Browser.Resolvers = map[string]config.Institution{
					"other": {OpenURLBase: "https://" + otherResolverHost + "/resolve"},
				}
			})
			ctx := context.Background()
			w := work.Work{DOI: "10.1176/appi.ajp.2010.09111680", Title: "Personalized Medicine for Depression", Authors: []string{"Simon, Gregory E."}, Year: 2010}
			id := parkInstitutional(t, jobs, "publisher-host-"+strings.ReplaceAll(tc.name, " ", "-"), w, tc.profile)
			if tc.publisher {
				if err := jobs.RecordEvent(ctx, id, app.PublisherFirstEventKind, map[string]any{"adapter_id": "psychiatryonline"}); err != nil {
					t.Fatal(err)
				}
				if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, job.PublisherFirstHandoffDetail, job.Access(true, "paywall")); err != nil {
					t.Fatal(err)
				}
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			runSync(t, b, helloWithFeatures(t, "0.21.0", institutionalMaterializationFeature, effectPermitFeature, "provider_drive_epoch_v1"))
			action, err := b.openHandoffForJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got := job.IsPublisherHandoff(*action); got != tc.publisher {
				t.Fatalf("action %+v publisher route = %v, want %v", action, got, tc.publisher)
			}
			raw, err := b.offer(*row, *action, config.ModeDelegated)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := protocol.DecodeBrowserMessage(raw)
			if err != nil {
				t.Fatal(err)
			}
			hosts := msg.Payload.(*protocol.JobOfferPayload).ProviderHosts
			if len(hosts) == 0 || hosts[0] != tc.wantFirst || hosts[len(hosts)-1] != tc.wantLast {
				t.Fatalf("offer provider hosts = %v, want route host %q first and resolver host %q last", hosts, tc.wantFirst, tc.wantLast)
			}
			count := 0
			for _, host := range hosts {
				if host == tc.wantLast {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("offer provider hosts = %v carry the resolver host %d times, want once", hosts, count)
			}
		})
	}
}

// The resolver host rides only where the frame stays valid: the protocol
// accepts 1..20 provider hosts of [a-z0-9.-], and an offer that cannot take the
// binding goes out without it rather than failing validation.
func TestPublisherRouteInstitutionHostNeverBreaksTheFrame(t *testing.T) {
	publisher := job.HumanAction{Kind: handoffActionKind, Detail: job.PublisherFirstHandoffDetail}
	resolver := job.HumanAction{Kind: handoffActionKind, Detail: app.InstitutionalOpenURLHandoffDetail}
	inst := config.Institution{OpenURLBase: "https://Openurl.Example.edu/resolve"}
	full := make([]string, maxOfferProviderHosts)
	for i := range full {
		full[i] = "host" + string(rune('a'+i)) + ".example.org"
	}
	for _, tc := range []struct {
		name   string
		hosts  []string
		action job.HumanAction
		inst   config.Institution
		want   []string
	}{
		{"binds last, lowercased", []string{"doi.org", "www.jstor.org"}, publisher, inst, []string{"doi.org", "www.jstor.org", "openurl.example.edu"}},
		{"already present", []string{"doi.org", "openurl.example.edu"}, publisher, inst, []string{"doi.org", "openurl.example.edu"}},
		{"no room", full, publisher, inst, full},
		{"host outside the protocol grammar", []string{"doi.org"}, publisher, config.Institution{OpenURLBase: "https://open_url.example.edu/resolve"}, []string{"doi.org"}},
		{"no resolver configured", []string{"doi.org"}, publisher, config.Institution{}, []string{"doi.org"}},
		{"resolver route unchanged", []string{"openurl.example.edu"}, resolver, inst, []string{"openurl.example.edu"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withPublisherRouteInstitutionHost(tc.hosts, tc.action, tc.inst)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("hosts = %v, want %v", got, tc.want)
			}
		})
	}

	// The offer's host list is shared with the control-suppression check, so
	// the helper must never write through spare capacity in the caller's slice.
	spare := append(make([]string, 0, 8), "doi.org", "www.jstor.org")
	if got := withPublisherRouteInstitutionHost(spare, publisher, inst); len(got) != 3 {
		t.Fatalf("hosts = %v, want the resolver host appended", got)
	}
	if leaked := spare[:3][2]; leaked != "" {
		t.Fatalf("the caller's slice gained %q in its spare capacity", leaked)
	}
}
