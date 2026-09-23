// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
)

// A packaged publisher adapter drives these DOIs from doi.org, while the UNE
// Primo route lands on the journal homepage (measured 2026-09-23 on AJP:
// identity_missing through Primo, 3/3 successes through retry-publisher).
// The first offer is therefore the DOI; the Primo route stays the fallback.
func TestExhaustedCandidatesOffersPublisherFirstForPackagedDOIPrefix(t *testing.T) {
	const primo = "https://une.primo.exlibrisgroup.com/nde/openurl?institution=61UNE_INST&vid=61UNE_INST:61UNE_NDE"
	for _, tc := range []struct {
		name, doi, want string
	}{
		{"psychiatryonline", "10.1176/appi.ajp.2010.09111680", "https://doi.org/10.1176/appi.ajp.2010.09111680"},
		{"acs", "10.1021/acs.jcim.6c00481", "https://doi.org/10.1021/acs.jcim.6c00481"},
		{"science", "10.1126/science.adz4433", "https://doi.org/10.1126/science.adz4433"},
		{"unmapped prefix keeps resolver first", "10.1002/example.pf", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, jobs := newTestService(t)
			svc.Config.AccessMode = config.ModeDelegated
			svc.Config.Browser.Resolvers = map[string]config.Institution{"une": {OpenURLBase: primo}}
			id, err := svc.Submit(ctx, protocol.WorkRequest{
				SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "wr_publisher_first",
				Identifiers: &protocol.Identifiers{DOI: tc.doi}, DesiredVersion: "any", Resolver: "une",
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
				if actions[0].Detail != InstitutionalOpenURLHandoffDetail || got[:len(primo)] != primo {
					t.Fatalf("unmapped DOI first offer = %q (%q), want the Primo resolver", got, actions[0].Detail)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("first offer = %q, want the DOI route %q before Primo", got, tc.want)
			}
			if !actions[0].RequiresAuth {
				t.Fatalf("publisher-first handoff must keep the paywall classification: %+v", actions[0])
			}

			// A second exhaustion pass for the same job (rediscovery after the
			// fallback) must not return to the DOI: it is one-shot per job.
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
		})
	}
}
