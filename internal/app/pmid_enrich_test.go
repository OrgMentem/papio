// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/resolvers/europepmc"
)

const (
	pmidOnlyPMID  = "42380380"
	pmidOnlyDOI   = "10.1007/s10072-026-09204-z"
	pmidOnlyTitle = "Evaluating LLM diagnostic reasoning: a new frontier for clinical AI assessment."
)

// europePMCRecord is the live shape of the PMID's own record: indexed, with a
// DOI, and NOT open access — exactly the record the resolver discards.
func europePMCRecord(doi string) string {
	doiField := ""
	if doi != "" {
		doiField = `"doi":"` + doi + `",`
	}
	return `{"hitCount":1,"resultList":{"result":[{"id":"` + pmidOnlyPMID + `","source":"MED","pmid":"` + pmidOnlyPMID + `",` +
		doiField + `"title":"` + pmidOnlyTitle + `","authorString":"Brigo F, Zaboli A, Maida E, Lavorgna L.",` +
		`"pubYear":"2026","isOpenAccess":"N"}]}}`
}

// europePMCService is exhaustionService with the real Europe PMC adapter
// pointed at an httptest fixture serving body, recording every query.
func europePMCService(t *testing.T, body string) (*Service, *job.Store, func() []string) {
	t.Helper()
	svc, jobs := exhaustionService(t)
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("query"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	svc.Config.Sources[config.SourceEuropePMC] = config.Source{Enabled: true}
	svc.Resolvers = append(svc.Resolvers, ResolverEntry{
		Adapter: europepmc.NewWithOptions(europepmc.Options{Client: srv.Client(), BaseURL: srv.URL}),
		Policy:  config.Source{Enabled: true},
	})
	return svc, jobs, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), queries...)
	}
}

func pmidOnlyRequest(id string) protocol.WorkRequest {
	return protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: id,
		Identifiers: &protocol.Identifiers{PMID: pmidOnlyPMID}, DesiredVersion: "any",
	}
}

// handoffRFTID returns the rft_id of the job's one open institutional handoff.
func handoffRFTID(t *testing.T, svc *Service, jobs *job.Store, row *job.Row) string {
	t.Helper()
	actions, err := jobs.ListOpenHumanActionsForJobs(context.Background(), []string{row.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != "openurl_handoff" {
		t.Fatalf("open actions = %+v, want one openurl_handoff", actions)
	}
	target, ok := ResolveHumanActionURL(actions[0], *row, svc.Config.InstitutionFor)
	if !ok {
		t.Fatal("handoff did not resolve to an institutional URL")
	}
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query().Get("rft_id")
}

// Defect 34: a PMID-only job was never enriched, so its institutional OpenURL
// carried only info:pmid while the PMID's own record named the DOI.
func TestPMIDOnlyWorkAdoptsItsRecordDOIBeforeTheHandoff(t *testing.T) {
	svc, jobs, queries := europePMCService(t, europePMCRecord(pmidOnlyDOI))
	got := processOnce(t, svc, jobs, pmidOnlyRequest("wr_pmid_only_doi"))

	if got.State != job.StateAwaitingHuman {
		t.Fatalf("state = %q, want awaiting_human", got.State)
	}
	if got.Work.DOI != pmidOnlyDOI || got.Work.Title != pmidOnlyTitle || got.Work.Year != 2026 || got.Work.PMID != pmidOnlyPMID {
		t.Fatalf("persisted work = %+v, want the PMID's own DOI, title, and year beside the PMID", got.Work)
	}
	if q := queries(); len(q) == 0 || q[0] != "EXT_ID:"+pmidOnlyPMID+" AND SRC:MED" {
		t.Fatalf("europe pmc queries = %q, want the PMID lookup first", q)
	}
	anchor, err := jobs.SubmittedIdentity(context.Background(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if anchor.Work.PMID != pmidOnlyPMID || anchor.Work.DOI != "" {
		t.Fatalf("anchor = %+v, want the submitted PMID only", anchor.Work)
	}
	if rftID := handoffRFTID(t, svc, jobs, got); rftID != "info:doi/"+pmidOnlyDOI {
		t.Fatalf("handoff rft_id = %q, want info:doi/%s", rftID, pmidOnlyDOI)
	}
}

func TestPMIDRecordWithoutDOIStaysPMIDOnly(t *testing.T) {
	svc, jobs, _ := europePMCService(t, europePMCRecord(""))
	got := processOnce(t, svc, jobs, pmidOnlyRequest("wr_pmid_no_doi"))

	if got.State != job.StateAwaitingHuman {
		t.Fatalf("state = %q, want awaiting_human", got.State)
	}
	if got.Work.DOI != "" || got.Work.PMID != pmidOnlyPMID || got.Work.Title != pmidOnlyTitle {
		t.Fatalf("persisted work = %+v, want PMID kept, bibliography filled, no DOI", got.Work)
	}
	if rftID := handoffRFTID(t, svc, jobs, got); rftID != "info:pmid/"+pmidOnlyPMID {
		t.Fatalf("handoff rft_id = %q, want info:pmid/%s", rftID, pmidOnlyPMID)
	}
}

// A PMID-only park from before enrichment existed re-enters resolving once and
// comes back with the DOI; a record without one re-parks and is not repaired
// again.
func TestRepairReResolvesAnUnenrichedPMIDPark(t *testing.T) {
	for _, tc := range []struct {
		name, doi, wantRFTID string
	}{
		{"record names a DOI", pmidOnlyDOI, "info:doi/" + pmidOnlyDOI},
		{"record has no DOI", "", "info:pmid/" + pmidOnlyPMID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, jobs, _ := europePMCService(t, europePMCRecord(tc.doi))
			lookup := svc.Resolvers
			svc.Resolvers = lookup[:1] // the pre-fix pass: no PMID lookup source
			parked := processOnce(t, svc, jobs, pmidOnlyRequest("wr_pmid_legacy_park"))
			if parked.State != job.StateAwaitingHuman || parked.Work.DOI != "" {
				t.Fatalf("precondition: parked = state:%q work:%+v", parked.State, parked.Work)
			}
			svc.Resolvers = lookup

			if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
				t.Fatal(err)
			}
			if got, _ := jobs.Get(ctx, parked.ID); got.State != job.StateResolving {
				t.Fatalf("state after repair = %q, want resolving", got.State)
			}
			row, err := jobs.ClaimNext(ctx, "w", time.Minute)
			if err != nil || row == nil {
				t.Fatalf("claim: %v", err)
			}
			if err := svc.Process(ctx, row); err != nil {
				t.Fatal(err)
			}
			reparked, _ := jobs.Get(ctx, parked.ID)
			if reparked.State != job.StateAwaitingHuman {
				t.Fatalf("state after re-resolve = %q, want awaiting_human", reparked.State)
			}
			if rftID := handoffRFTID(t, svc, jobs, reparked); rftID != tc.wantRFTID {
				t.Fatalf("handoff rft_id = %q, want %q", rftID, tc.wantRFTID)
			}

			if err := svc.HandoffRepairer().RunDue(ctx); err != nil {
				t.Fatal(err)
			}
			if again, _ := jobs.Get(ctx, parked.ID); again.State != job.StateAwaitingHuman {
				t.Fatalf("state after second repair pass = %q, want the park kept (one-shot)", again.State)
			}
			events, _ := jobs.Events(ctx, parked.ID)
			repairs := 0
			for _, event := range events {
				detail, _ := event["detail"].(map[string]any)
				if reason, _ := detail["reason"].(string); reason == pmidEnrichmentRepairReason {
					repairs++
				}
			}
			if repairs != 1 {
				t.Fatalf("pmid enrichment repairs = %d, want exactly 1", repairs)
			}
		})
	}
}
