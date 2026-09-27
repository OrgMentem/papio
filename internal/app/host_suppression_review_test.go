// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/work"
)

// Regression tests for the ADR-0031 review findings. Each drives the real
// fetchCandidates path.

// A 429 that also carries a WAF challenge header is a rate limit, not a
// wall: three of them spread over more than a day must never suppress the
// host, and the next fetch after the backoff reaches it.
func TestRateLimitedChallengeNeverBecomesAWall(t *testing.T) {
	h := newSuppressionHarness(t)
	var reached int
	dl, err := fetch.New(fetch.DefaultPolicy(), publicHostResolver{}, handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.Header().Set("X-Amzn-Waf-Action", "challenge")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})})
	if err != nil {
		t.Fatal(err)
	}
	dl.SetHostGate(h.svc.HostGate)
	h.svc.Fetch = func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		return dl.DownloadWithHeaders(ctx, c.URL, c.RequestHeaders, path)
	}
	for i, id := range []string{"wr_429_waf_1", "wr_429_waf_2", "wr_429_waf_3", "wr_429_waf_4"} {
		row := h.run(id, h.candidate("https://"+walledHost+"/"+id+".pdf", resolver.AccessLicensedAPI))
		attempts := h.fetchAttempts(row.ID)
		if len(attempts) != 1 || attempts[0].outcome != fetch.ClassRetryable || attempts[0].status != http.StatusTooManyRequests ||
			!strings.HasPrefix(attempts[0].detail, "*fetch.Error(http-429)") {
			t.Fatalf("job %d attempts = %+v, want one retryable http-429 fetch", i+1, attempts)
		}
		h.now = h.now.Add(13 * time.Hour) // past the backoff cap, walls would span a day
	}
	if reached != 4 {
		t.Fatalf("host reached %d times, want 4: a rate limit must never suppress", reached)
	}
}

// Probe revival must pick a candidate this pass can actually fetch. A
// suppressed row the resolvers no longer return is not revivable; it must not
// block the live row on the same host.
func TestProbeRevivalSkipsStaleRows(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden, "busy.example.org": http.StatusServiceUnavailable})
	stale := h.candidate("https://"+walledHost+"/gone.pdf", resolver.AccessLicensedAPI)
	current := h.candidate("https://"+walledHost+"/current.pdf", resolver.AccessLicensedAPI)
	busy := h.candidate("https://busy.example.org/paper.pdf", resolver.AccessLicensedAPI)
	row := h.run("wr_stale_revival", stale, current, busy)
	if row.State != job.StateRetryWait {
		t.Fatalf("pass 1 state = %s, want retry_wait", row.State)
	}
	// The next pass resolves only the current URL (the stale one vanished).
	h.svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{current, busy}}, Policy: config.Source{Enabled: true}}}
	h.now = h.now.Add(8 * 24 * time.Hour)
	h.fetched = nil
	if _, err := h.jobs.S.DB().Exec(`UPDATE jobs SET retry_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), row.ID); err != nil {
		t.Fatal(err)
	}
	h.process(row.ID)
	if n := strings.Count(strings.Join(h.fetched, ","), walledHost); n != 1 {
		t.Fatalf("probe pass fetched %v, want the live row probed once", h.fetched)
	}
}

// A budget refusal between the probe verdict and the request must hand the
// probe back: the next job on the host gets it, marked.
func TestProbeReleasedWhenBudgetRefuses(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	h.now = h.now.Add(8 * 24 * time.Hour)
	ctx := context.Background()
	h.svc.Budgets = budget.New(h.jobs.S)
	h.svc.Config.Sources["other"] = config.Source{Enabled: true}
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	// The source gate closes after resolution, so the refusal lands on the
	// fetch-stage budget admission, after the probe verdict.
	h.svc.Resolvers = []ResolverEntry{{Adapter: &gatingResolver{
		fakeResolver: fakeResolver{name: "fixture", cands: []resolver.Candidate{h.candidate("https://"+walledHost+"/gated.pdf", resolver.AccessLicensedAPI)}},
		gate: func() {
			if err := h.svc.Budgets.Defer(ctx, "fixture", config.Source{Enabled: true}, time.Now().UTC().Add(48*time.Hour)); err != nil {
				t.Error(err)
			}
		},
	}, Policy: config.Source{Enabled: true}}}
	gatedID, err := h.svc.Submit(ctx, doiRequestFor("wr_probe_gated"))
	if err != nil {
		t.Fatal(err)
	}
	gated := h.process(gatedID)
	if len(h.fetched) != 0 || h.candidateStatus(gated.ID, walledHost) != "retryable" {
		t.Fatalf("gated job: fetched %v, candidate %s; want no request and a retryable row", h.fetched, h.candidateStatus(gated.ID, walledHost))
	}
	other := h.candidate("https://"+walledHost+"/other.pdf", resolver.AccessLicensedAPI)
	other.Source = "other"
	h.svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "other", cands: []resolver.Candidate{other}}, Policy: config.Source{Enabled: true}}}
	id, err := h.svc.Submit(ctx, doiRequestFor("wr_probe_after_gate"))
	if err != nil {
		t.Fatal(err)
	}
	row := h.process(id)
	attempts := h.fetchAttempts(row.ID)
	if len(h.fetched) != 1 || len(attempts) != 1 || !strings.HasSuffix(attempts[0].detail, " probe") {
		t.Fatalf("next job: fetched %v, attempts %+v; want the released probe, marked", h.fetched, attempts)
	}
}

// A probe admitted on a redirect target belongs to the pass: its attempt is
// marked, and the job's next candidate on that host joins the probe pass.
func TestRedirectProbeJoinsThePass(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	h.now = h.now.Add(8 * 24 * time.Hour)
	var walledHits int
	dl, err := fetch.New(fetch.DefaultPolicy(), publicHostResolver{}, handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "doi.example.org" {
			http.Redirect(w, r, "https://"+walledHost+"/via-doi.pdf", http.StatusFound)
			return
		}
		walledHits++
		w.WriteHeader(http.StatusForbidden)
	})})
	if err != nil {
		t.Fatal(err)
	}
	dl.SetHostGate(h.svc.HostGate)
	h.svc.Fetch = func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		return dl.DownloadWithHeaders(ctx, c.URL, c.RequestHeaders, path)
	}
	row := h.run("wr_redirect_probe",
		h.candidate("https://doi.example.org/10.1/x", resolver.AccessLicensedAPI),
		h.candidate("https://"+walledHost+"/direct.pdf", resolver.AccessLicensedAPI))
	if walledHits != 2 {
		t.Fatalf("walled host reached %d times, want 2 (redirect probe + same-pass candidate)", walledHits)
	}
	for _, attempt := range h.fetchAttempts(row.ID) {
		if !strings.HasSuffix(attempt.detail, " probe") {
			t.Fatalf("attempt %+v lacks the probe marker", attempt)
		}
	}
}

// Every park or exhaustion a pass with skips reaches carries the summary:
// the landing-page-only park and a document-delivery park included.
func TestSuppressionSummaryOnEveryPark(t *testing.T) {
	t.Run("landing_page_only", func(t *testing.T) {
		h := newSuppressionHarness(t)
		h.seedWalls(walledHost, 3)
		h.fetchWith(nil)
		landing := h.candidate("https://landing.example.org/article", resolver.AccessLicensedAPI)
		landing.Direct = false
		row := h.run("wr_summary_landing", h.candidate("https://"+walledHost+"/p.pdf", resolver.AccessLicensedAPI), landing)
		detail := h.lastTransitionDetail(row.ID)
		if detail["reason"] != "landing_page_only" || detail["suppressed_hosts"] == nil {
			t.Fatalf("landing park detail = %v, want landing_page_only with suppressed_hosts", detail)
		}
	})
	t.Run("document_delivery", func(t *testing.T) {
		h := newSuppressionHarness(t)
		h.svc.Config.AccessMode = config.ModeDelegated
		h.svc.Config.Browser.DocumentDelivery = &config.DocumentDelivery{Kind: "openurl", BaseURL: "https://ill.example.edu/request-form"}
		h.svc.Delivery = delivery.New(h.jobs.S, &h.svc.Config, nil)
		h.seedWalls(walledHost, 3)
		h.fetchWith(nil)
		h.svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{
			h.candidate("https://"+walledHost+"/p.pdf", resolver.AccessLicensedAPI)}}, Policy: config.Source{Enabled: true}}}
		id, err := h.svc.Submit(context.Background(), deliveryWorkRequest("wr_summary_delivery", "10.1000/summary-delivery"))
		if err != nil {
			t.Fatal(err)
		}
		row := h.process(id)
		detail := h.lastTransitionDetail(row.ID)
		if !strings.HasPrefix(detail["reason"].(string), "document_delivery") || detail["suppressed_hosts"] == nil {
			t.Fatalf("delivery park detail = %v, want a document_delivery park with suppressed_hosts", detail)
		}
	})
}

// gatingResolver runs gate after answering, as a source gate closing between
// resolution and the fetch-stage admission would.
type gatingResolver struct {
	fakeResolver
	gate func()
}

func (g *gatingResolver) Resolve(ctx context.Context, requested work.Work) ([]resolver.Candidate, error) {
	cands, err := g.fakeResolver.Resolve(ctx, requested)
	g.gate()
	return cands, err
}
