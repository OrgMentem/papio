// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

func TestOpenAthensRedirectorURLEncodesTheDestinationAndRefusesBadDomains(t *testing.T) {
	got, ok := OpenAthensRedirectorURL("example.edu", "https://doi.org/10.1002/(SICI)1097-0258(19980815/30)17:15/16<1661::AID-SIM968>3.0.CO;2-2")
	want := "https://go.openathens.net/redirector/example.edu?url=https%3A%2F%2Fdoi.org%2F10.1002%2F%28SICI%291097-0258%2819980815%2F30%2917%3A15%2F16%3C1661%3A%3AAID-SIM968%3E3.0.CO%3B2-2"
	if !ok || got != want {
		t.Fatalf("redirector URL = %q, %v; want %q", got, ok, want)
	}
	// The decoded url parameter is exactly the destination, with nothing
	// folded into the redirector's own query.
	parsed, err := url.Parse(got)
	if err != nil || parsed.Host != "go.openathens.net" || parsed.Path != "/redirector/example.edu" || len(parsed.Query()) != 1 ||
		!strings.HasPrefix(parsed.Query().Get("url"), "https://doi.org/10.1002/(SICI)") {
		t.Fatalf("parsed redirector URL = %+v err=%v", parsed, err)
	}
	for _, domain := range []string{"", "Example.edu", "example", "https://example.edu", "example.edu/evil", "example.edu:8443", "example.edu?x=1", "a b.edu", "-example.edu", "example..edu", "example.edu."} {
		if got, ok := OpenAthensRedirectorURL(domain, "https://doi.org/10.1000/x"); ok || got != "" {
			t.Errorf("OpenAthensRedirectorURL(%q) = %q, %v; want refusal", domain, got, ok)
		}
	}
}

func TestPublisherHandoffURLUsesTheRedirectorOnlyWithAValidDomain(t *testing.T) {
	action := job.HumanAction{Kind: "openurl_handoff", Detail: job.PublisherFirstHandoffDetail}
	row := job.Row{Work: work.Work{DOI: "10.1016/j.jval.2022.04.1227"}}
	const redirected = "https://go.openathens.net/redirector/example.edu?url=https%3A%2F%2Fdoi.org%2F10.1016%2Fj.jval.2022.04.1227"
	if got, ok := PublisherHandoffURL(action, row, config.Institution{OpenAthensDomain: "example.edu"}); !ok || got != redirected {
		t.Fatalf("publisher-first URL with a domain = %q, %v; want %q", got, ok, redirected)
	}
	// An explicit publisher retry and a diagnosed manual download of the DOI
	// route share the one derivation.
	for _, detail := range []string{job.PublisherHandoffDetail, job.PublisherHandoffDetail + "\nhttps://evil.example"} {
		if got, ok := PublisherHandoffURL(job.HumanAction{Detail: detail}, row, config.Institution{OpenAthensDomain: "example.edu"}); !ok || got != redirected {
			t.Fatalf("publisher handoff %q = %q, %v; want the redirector", detail, got, ok)
		}
	}
	// No domain, or a domain that bypassed config validation, never reaches a
	// URL: the route stays the plain DOI.
	for _, domain := range []string{"", "example.edu/evil", "https://example.edu"} {
		if got, ok := PublisherHandoffURL(action, row, config.Institution{OpenAthensDomain: domain}); !ok || got != "https://doi.org/10.1016/j.jval.2022.04.1227" {
			t.Fatalf("publisher-first URL with domain %q = %q, %v; want the plain DOI", domain, got, ok)
		}
	}
	// A job with no valid DOI has no publisher route at all, domain or not.
	if got, ok := PublisherHandoffURL(action, job.Row{Work: work.Work{PMID: "123"}}, config.Institution{OpenAthensDomain: "example.edu"}); ok || got != "" {
		t.Fatalf("DOI-less job route = %q, %v; want none", got, ok)
	}
	// The page cannot supply the destination: only an action that names the
	// DOI route reaches the redirector.
	if got, ok := PublisherHandoffURL(job.HumanAction{Detail: "https://evil.example/x"}, row, config.Institution{OpenAthensDomain: "example.edu"}); ok || got != "" {
		t.Fatalf("non-publisher action produced %q, %v", got, ok)
	}
	// ResolveHumanActionURL reads the domain from the job's own profile.
	got, ok := ResolveHumanActionURL(action, job.Row{Work: row.Work, Policy: job.Policy{Resolver: "example"}}, func(name string) (config.Institution, bool) {
		if name != "example" {
			t.Errorf("profile looked up = %q, want the job's resolver", name)
		}
		return config.Institution{OpenURLBase: "https://example.primo.exlibrisgroup.com/nde/openurl", OpenAthensDomain: "example.edu"}, true
	})
	if !ok || got != redirected {
		t.Fatalf("ResolveHumanActionURL = %q, %v; want %q", got, ok, redirected)
	}
}

// With an OpenAthens domain, a DOI whose destination has a packaged adapter is
// offered through the redirector before the institution's OpenURL (Primo)
// route; the OpenURL route stays the one-shot fallback. Without a domain, with
// no DOI, or for a destination with no packaged adapter, nothing changes.
func TestExhaustedCandidatesOffersOpenAthensRedirectorBeforeOpenURL(t *testing.T) {
	const primo = "https://example.primo.exlibrisgroup.com/nde/openurl?institution=61EXU_INST&vid=61EXU_INST:61EXU_NDE"
	const redirector = "https://go.openathens.net/redirector/example.edu?url="
	for _, tc := range []struct {
		name         string
		ids          *protocol.Identifiers
		domain       string
		otherProfile bool
		want         string // wanted first offer; empty means the Primo resolver
	}{
		{"elsevier through the redirector", &protocol.Identifiers{DOI: "10.1016/j.jval.2022.04.1227"}, "example.edu", false,
			redirector + "https%3A%2F%2Fdoi.org%2F10.1016%2Fj.jval.2022.04.1227"},
		{"taylor and francis through the redirector", &protocol.Identifiers{DOI: "10.1080/0144929x.2019.1578828"}, "example.edu", false,
			redirector + "https%3A%2F%2Fdoi.org%2F10.1080%2F0144929x.2019.1578828"},
		{"existing publisher-first prefix also uses it", &protocol.Identifiers{DOI: "10.1021/acs.jcim.6c00481"}, "example.edu", false,
			redirector + "https%3A%2F%2Fdoi.org%2F10.1021%2Facs.jcim.6c00481"},
		{"no domain keeps the resolver first", &protocol.Identifiers{DOI: "10.1016/j.jval.2022.04.1227"}, "", false, ""},
		{"no packaged adapter keeps the resolver first", &protocol.Identifiers{DOI: "10.1287/isre.2024.0857"}, "example.edu", false, ""},
		{"no DOI keeps the resolver first", &protocol.Identifiers{PMID: "31234567"}, "example.edu", false, ""},
		{"another profile's domain does not apply", &protocol.Identifiers{DOI: "10.1016/j.jval.2022.04.1227"}, "example.edu", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, jobs := newTestService(t)
			svc.Config.AccessMode = config.ModeDelegated
			profile := config.Institution{OpenURLBase: primo, OpenAthensDomain: tc.domain}
			svc.Config.Browser.Resolvers = map[string]config.Institution{"example": profile}
			resolver := "example"
			if tc.otherProfile {
				svc.Config.Browser.Resolvers = map[string]config.Institution{
					"example": {OpenURLBase: primo},
					"other":   {OpenURLBase: "https://other.example.edu/openurl", OpenAthensDomain: tc.domain},
				}
			}
			id, err := svc.Submit(ctx, protocol.WorkRequest{
				SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_openathens_first",
				Identifiers: tc.ids, DesiredVersion: "any", Resolver: resolver,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.exhaustedCandidates(ctx, row, job.StateResolving, "no_legal_candidates", "no legal candidates", ""); err != nil {
				t.Fatal(err)
			}
			actions := handoffActionsForJob(t, jobs, id)
			if len(actions) != 1 {
				t.Fatalf("handoff actions = %+v, want one", actions)
			}
			got, ok := ResolveHumanActionURL(actions[0], *row, svc.Config.InstitutionFor)
			if !ok {
				t.Fatalf("handoff %+v has no URL", actions[0])
			}
			if tc.want == "" {
				if actions[0].Detail != InstitutionalOpenURLHandoffDetail || !strings.HasPrefix(got, primo) {
					t.Fatalf("first offer = %q (%q), want the Primo resolver", got, actions[0].Detail)
				}
				return
			}
			if actions[0].Detail != job.PublisherFirstHandoffDetail || got != tc.want {
				t.Fatalf("first offer = %q (%q), want the redirector %q", got, actions[0].Detail, tc.want)
			}
			if !actions[0].RequiresAuth {
				t.Fatalf("redirector handoff must keep the paywall classification: %+v", actions[0])
			}

			// The redirector is one-shot: a second exhaustion pass for the same
			// job (rediscovery after the fallback) offers the OpenURL route.
			if err := jobs.Transition(ctx, id, job.StateAwaitingHuman, job.StateResolving, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE human_actions SET status='resolved' WHERE job_id=?`, id); err != nil {
				t.Fatal(err)
			}
			if err := svc.exhaustedCandidates(ctx, row, job.StateResolving, "no_legal_candidates", "no legal candidates", ""); err != nil {
				t.Fatal(err)
			}
			actions = handoffActionsForJob(t, jobs, id)
			if len(actions) != 1 || actions[0].Detail != InstitutionalOpenURLHandoffDetail {
				t.Fatalf("second pass handoffs = %+v, want only the institutional route", actions)
			}
			if got, ok := ResolveHumanActionURL(actions[0], *row, svc.Config.InstitutionFor); !ok || !strings.HasPrefix(got, primo) {
				t.Fatalf("fallback URL = %q, %v; want the Primo resolver", got, ok)
			}
		})
	}
}
