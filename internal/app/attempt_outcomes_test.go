// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/resolver"
	"papio/internal/store"
	"papio/internal/work"
)

type attemptOutcomePMID struct {
	fakeResolver
	metadata fakeEnricher
}

func (r *attemptOutcomePMID) LookupPMID(ctx context.Context, _ string) (work.Work, bool, error) {
	return r.metadata.Enrich(ctx, work.Work{})
}

type attemptOutcomeSecondary struct {
	fakeResolver
}

func (r *attemptOutcomeSecondary) Resolve(ctx context.Context, w work.Work) ([]resolver.Candidate, error) {
	if w.DOI == "10.1002/example" {
		return nil, nil
	}
	return r.fakeResolver.Resolve(ctx, w)
}

// Configure a real Process pass whose selected provider boundary has the given
// outcome. The ordinal distinguishes sibling attempts from primary attempts.
func configureAttemptOutcome(svc *Service, phase string, providerErr error, matched bool) (protocol.WorkRequest, string, int) {
	request := doiRequest("wr_attempt_outcome")
	candidate := typedSiblingCandidate("fixture")
	candidate.ResolvedWork = work.Work{}
	adapter := &fakeResolver{name: "fixture", cands: []resolver.Candidate{candidate}, err: providerErr}
	svc.Config.Sources[config.SourceCrossrefMetadata] = config.Source{Enabled: true}
	svc.Resolvers = []ResolverEntry{{Adapter: adapter, Policy: config.Source{Enabled: true}}}
	source, ordinal := "fixture", 1
	switch phase {
	case "fuzzy":
		svc.Resolvers[0].Adapter = &fakeSiblingResolver{
			fakeResolver: fakeResolver{name: "fixture"},
			siblings:     []resolver.Candidate{candidate},
			siblingErr:   providerErr,
		}
		ordinal = 2
	case "relations":
		svc.Resolvers = nil
		svc.Enricher = &fakeRelations{siblings: []string{"10.1234/sibling"}, err: providerErr}
		source = config.SourceCrossrefMetadata
	case "typed":
		svc.Enricher = &fakeRelations{siblings: []string{"10.1234/sibling"}}
		svc.Resolvers[0].Adapter = &attemptOutcomeSecondary{fakeResolver: *adapter}
		ordinal = 2
	case "pmid":
		request.Identifiers = &protocol.Identifiers{PMID: "12345678"}
		svc.Resolvers[0].Adapter = &attemptOutcomePMID{
			fakeResolver: *adapter,
			metadata: fakeEnricher{
				result:  work.Work{PMID: "12345678", DOI: "10.1002/example", Title: "Example Paper"},
				matched: matched, err: providerErr,
			},
		}
	case "enrichment":
		request.Identifiers = nil
		request.Title = "Example Paper"
		request.Authors = []string{"Ada Lovelace"}
		request.Year = 2024
		svc.MetadataEnrichers = []MetadataEnricherEntry{{Name: "fixture", Enricher: &fakeEnricher{
			result: work.Work{Title: "Example Paper", Authors: []string{"Ada Lovelace"}, Year: 2024, DOI: "10.1002/example"}, matched: matched, err: providerErr,
		}}}
	}
	return request, source, ordinal
}

func TestProcessSurfacesFinishAttemptFailure(t *testing.T) {
	for _, phase := range []string{"resolve", "fuzzy", "relations", "typed", "pmid", "enrichment"} {
		outcomes := []struct {
			name string
			err  error
		}{
			{"success", nil},
			{"failed", errors.New("provider refused")},
			{"retryable", &resolver.TemporaryError{Err: errors.New("provider unavailable"), RetryAfter: time.Hour}},
			{"wire_refused", &budget.ErrExceeded{Kind: budget.KindCredits, Window: budget.WindowUTCDay, Until: time.Now().Add(time.Hour)}},
		}
		if phase == "fuzzy" {
			outcomes = append(outcomes, struct {
				name string
				err  error
			}{"no_search_basis", resolver.ErrNoSearchBasis})
		}
		if phase == "pmid" || phase == "enrichment" {
			outcomes = append(outcomes, struct {
				name string
				err  error
			}{"not_applicable", resolver.ErrNotApplicable}, struct {
				name string
				err  error
			}{"no_match", nil})
		}
		for _, outcome := range outcomes {
			t.Run(phase+"/"+outcome.name, func(t *testing.T) {
				svc, jobs := newTestService(t)
				ctx := context.Background()
				request, source, ordinal := configureAttemptOutcome(svc, phase, outcome.err, outcome.name != "no_match")
				fetches := 0
				svc.Fetch = fakeDownload(&fetches)
				svc.Validate = passValidation()
				id, err := svc.Submit(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				row, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				trigger := fmt.Sprintf(`CREATE TRIGGER refuse_attempt_finish BEFORE UPDATE OF ended_at ON attempts
					WHEN NEW.source = '%s' AND (SELECT COUNT(*) FROM attempts WHERE job_id = NEW.job_id AND source = NEW.source) = %d
					BEGIN SELECT RAISE(ABORT, 'injected attempt finish failure'); END`, source, ordinal)
				if _, err := jobs.S.DB().ExecContext(ctx, trigger); err != nil {
					t.Fatal(err)
				}
				if err := svc.Process(ctx, row); err == nil || !strings.Contains(err.Error(), "injected attempt finish failure") {
					t.Fatalf("Process error = %v, want the failed outcome write", err)
				}
				got, err := jobs.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if got.State != job.StateResolving || fetches != 0 {
					t.Fatalf("state = %s, fetches = %d; failed persistence must stop the pass", got.State, fetches)
				}
				if got.Work.DOI != row.Work.DOI {
					t.Fatalf("DOI changed from %q to %q before the attempt settled", row.Work.DOI, got.Work.DOI)
				}
				var candidates, open int
				if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM candidates WHERE job_id = ?`, id).Scan(&candidates); err != nil {
					t.Fatal(err)
				}
				if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE job_id = ? AND ended_at IS NULL`, id).Scan(&open); err != nil {
					t.Fatal(err)
				}
				if candidates != 0 || open != 1 {
					t.Fatalf("candidates = %d, open attempts = %d; want no published candidates and one surfaced failure", candidates, open)
				}
			})
		}
	}
}

func TestProcessSurfacesBudgetBlockedAttemptFinishFailure(t *testing.T) {
	for _, phase := range []string{"resolve", "relations", "pmid", "enrichment"} {
		t.Run(phase, func(t *testing.T) {
			svc, jobs := newTestService(t)
			ctx := context.Background()
			request, source, _ := configureAttemptOutcome(svc, phase, nil, true)
			svc.Budgets = budget.New(jobs.S)
			if err := svc.Budgets.Defer(ctx, source, config.Source{Enabled: true}, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			fetches := 0
			svc.Fetch = fakeDownload(&fetches)
			svc.Validate = passValidation()
			id, err := svc.Submit(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.S.DB().ExecContext(ctx, `CREATE TRIGGER refuse_budget_finish BEFORE UPDATE ON attempts
				WHEN NEW.outcome = 'budget_blocked' BEGIN SELECT RAISE(ABORT, 'injected budget finish failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := svc.Process(ctx, row); err == nil || !strings.Contains(err.Error(), "injected budget finish failure") {
				t.Fatalf("Process error = %v, want the failed blocked outcome write", err)
			}
			got, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != job.StateResolving {
				t.Fatalf("state = %s, want resolving rather than a normal budget park", got.State)
			}
		})
	}
}

func TestProcessSurfacesBudgetDeferFailure(t *testing.T) {
	for _, phase := range []string{"resolve", "fuzzy", "relations", "typed", "pmid", "enrichment"} {
		t.Run(phase, func(t *testing.T) {
			svc, jobs := newTestService(t)
			ctx := context.Background()
			providerErr := &resolver.TemporaryError{Err: errors.New("provider unavailable"), RetryAfter: time.Hour}
			request, _, _ := configureAttemptOutcome(svc, phase, providerErr, true)
			svc.Budgets = budget.New(jobs.S)
			fetches := 0
			svc.Fetch = fakeDownload(&fetches)
			svc.Validate = passValidation()
			id, err := svc.Submit(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			trigger := fmt.Sprintf(`CREATE TRIGGER refuse_budget_defer BEFORE INSERT ON source_budgets
				WHEN NEW.next_allowed_at > '%s' BEGIN SELECT RAISE(ABORT, 'injected defer failure'); END`, store.FormatTime(time.Now().Add(time.Minute)))
			if _, err := jobs.S.DB().ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			if err := svc.Process(ctx, row); err == nil || !strings.Contains(err.Error(), "injected defer failure") {
				t.Fatalf("Process error = %v, want the failed source gate write", err)
			}
			got, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != job.StateResolving {
				t.Fatalf("state = %s, want resolving rather than a normal retry park", got.State)
			}
			var open int
			if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE job_id = ? AND ended_at IS NULL`, id).Scan(&open); err != nil {
				t.Fatal(err)
			}
			if open != 0 {
				t.Fatalf("open attempts = %d, want settled attempts despite the failed defer", open)
			}
		})
	}
}

func TestProcessSurfacesSiblingCandidateInsertFailure(t *testing.T) {
	for _, boundary := range []string{"resolve", "fetch"} {
		t.Run(boundary, func(t *testing.T) {
			svc, jobs := newTestService(t)
			ctx := context.Background()
			primary := typedSiblingCandidate("fixture")
			primary.URL = "https://example.test/dead.pdf"
			primary.ResolvedWork = work.Work{}
			sibling := primary
			sibling.URL = "https://example.test/sibling.pdf"
			adapter := &fakeSiblingResolver{
				fakeResolver: fakeResolver{name: "fixture"},
				siblings:     []resolver.Candidate{sibling},
			}
			wantState := job.StateResolving
			if boundary == "fetch" {
				adapter.cands = []resolver.Candidate{primary}
				wantState = job.StateFetching
			}
			svc.Config.Sources["empty"] = config.Source{Enabled: true}
			svc.Resolvers = []ResolverEntry{
				{Adapter: &fakeSiblingResolver{fakeResolver: fakeResolver{name: "empty"}}, Policy: config.Source{Enabled: true}},
				{Adapter: adapter, Policy: config.Source{Enabled: true}},
			}
			svc.Fetch = landingExpansionFetch(sibling.URL)
			svc.Validate = passValidation()
			id, err := svc.Submit(ctx, doiRequest("wr_sibling_insert_failure"))
			if err != nil {
				t.Fatal(err)
			}
			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jobs.S.DB().ExecContext(ctx, `CREATE TRIGGER refuse_sibling_candidate BEFORE INSERT ON candidates
				WHEN NEW.url_redacted LIKE '%/sibling.pdf' BEGIN SELECT RAISE(ABORT, 'injected sibling insert failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := svc.Process(ctx, row); err == nil || !strings.Contains(err.Error(), "injected sibling insert failure") {
				t.Fatalf("Process error = %v, want the failed sibling candidate write", err)
			}
			got, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != wantState {
				t.Fatalf("state = %s, want %s rather than exhaustion", got.State, wantState)
			}
			var markers int
			if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id = ? AND kind = 'job.sibling_search'`, id).Scan(&markers); err != nil {
				t.Fatal(err)
			}
			if markers != 0 {
				t.Fatalf("search markers = %d, want none before the candidates persist", markers)
			}
			if _, err := jobs.S.DB().ExecContext(ctx, `DROP TRIGGER refuse_sibling_candidate`); err != nil {
				t.Fatal(err)
			}
			if err := svc.Process(ctx, got); err != nil {
				t.Fatal(err)
			}
			got, err = jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != job.StateReady || len(adapter.hopRequests) != 2 {
				t.Fatalf("state = %s, sibling searches = %d; want ready after a second search", got.State, len(adapter.hopRequests))
			}
			if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id = ? AND kind = 'job.sibling_search'`, id).Scan(&markers); err != nil {
				t.Fatal(err)
			}
			if markers != 1 {
				t.Fatalf("search markers = %d, want one after the candidates persist", markers)
			}
		})
	}
}

func TestLandingDerivedCandidateAndEventCommitTogether(t *testing.T) {
	svc, jobs := newTestService(t)
	ctx := context.Background()
	const landingURL = "https://example.test/article"
	const pdfURL = "https://example.test/recovered.pdf"
	parent := typedSiblingCandidate("fixture")
	parent.URL = "https://example.test/dead.pdf"
	parent.Landing = landingURL
	parent.ResolvedWork = work.Work{}
	svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{parent}}, Policy: config.Source{Enabled: true}}}
	svc.Fetch = landingExpansionFetch(pdfURL)
	svc.Validate = passValidation()
	svc.LandingReader = &fakeLandingReader{pdfURLFor: map[string]string{landingURL: pdfURL}}
	id, err := svc.Submit(ctx, doiRequest("wr_landing_atomic"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `CREATE TRIGGER refuse_landing_event BEFORE INSERT ON events
		WHEN NEW.kind = 'job.landing_derived' BEGIN SELECT RAISE(ABORT, 'injected landing event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err == nil || !strings.Contains(err.Error(), "injected landing event failure") {
		t.Fatalf("Process error = %v, want the failed landing event write", err)
	}
	var derived, events int
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM candidates WHERE job_id = ? AND rank_evidence LIKE 'derived=citation_pdf_url%'`, id).Scan(&derived); err != nil {
		t.Fatal(err)
	}
	if err := jobs.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE job_id = ? AND kind = 'job.landing_derived'`, id).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if derived != 0 || events != 0 {
		t.Fatalf("derived candidates = %d, events = %d; want both rolled back", derived, events)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `DROP TRIGGER refuse_landing_event`); err != nil {
		t.Fatal(err)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatal(err)
	}
	got, err := jobs.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != job.StateReady {
		t.Fatalf("state = %s, want ready from the committed derived candidate", got.State)
	}
}
