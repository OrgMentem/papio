// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"papio/internal/budget"
	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/fetch"
	"papio/internal/hostsuppress"
	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/store"
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
	if _, err := h.jobs.S.DB().Exec(`UPDATE jobs SET retry_at = ? WHERE id = ?`, store.FormatTime(time.Now().Add(-time.Minute)), row.ID); err != nil {
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

// A startup rebuild that fails part-way through its scan must leave the
// tracker untouched. The rows it had already read would otherwise stay
// counted while the rebuild is retried from the start on the next pass, and
// two historical 403s counted twice suppress a host that never earned it.
func TestFailedStartupRebuildCountsNothingTwice(t *testing.T) {
	h := newSuppressionHarness(t)
	cfg := config.DefaultHostSuppression()
	cfg.MinWallSpanHours = 0
	h.svc.EnableHostSuppression(cfg)
	h.seedWalls(walledHost, 2)
	h.seed("invalid", "*fetch.Error", h.now.Add(-time.Minute))
	scans := 0
	realScan := scanSuppressionRow
	scanSuppressionRow = func(rows *sql.Rows, dest ...any) error {
		scans++
		if scans == 3 {
			return errors.New("injected scan failure")
		}
		return realScan(rows, dest...)
	}
	t.Cleanup(func() { scanSuppressionRow = realScan })
	if h.svc.hostSuppression(context.Background()) == nil {
		t.Fatal("tracker is nil")
	}
	if scans != 3 {
		t.Fatalf("first rebuild scanned %d rows, want the injected failure on the third", scans)
	}
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	h.run("wr_rebuild_retry", h.candidate("https://"+walledHost+"/p.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 1 {
		t.Fatalf("after a failed then a clean rebuild of two walls: fetched %v, want a normal fetch", h.fetched)
	}
}

// A live fetch observation during a failed startup rebuild must not
// double-count when the rebuild later succeeds. The live row is already
// committed (FinishAttempt before observeFetchAttempt, the production order),
// so the next clean rebuild reads it; folding it live too counts one wall
// twice and suppresses a host that never earned it.
func TestFailedStartupRebuildWithLiveObservationCountsOnce(t *testing.T) {
	h := newSuppressionHarness(t)
	cfg := config.DefaultHostSuppression()
	cfg.MinWallSpanHours = 0
	h.svc.EnableHostSuppression(cfg)
	h.seedWalls(walledHost, 1)
	scans := 0
	realScan := scanSuppressionRow
	scanSuppressionRow = func(rows *sql.Rows, dest ...any) error {
		scans++
		if scans == 1 {
			return errors.New("injected scan failure")
		}
		return realScan(rows, dest...)
	}
	t.Cleanup(func() { scanSuppressionRow = realScan })
	if h.svc.hostSuppression(context.Background()) == nil {
		t.Fatal("tracker is nil")
	}
	if scans != 1 {
		t.Fatalf("first rebuild scanned %d rows, want the injected failure on the first", scans)
	}
	// Production order for a fetch that completes during the outage: commit
	// the row, then fold it. The rebuild already failed, so the fold must
	// wait for the retry rather than counting immediately.
	detail := "*fetch.Error(http-403) host=" + walledHost + " final=" + walledHost
	h.seed("invalid", detail, h.now.Add(-time.Minute))
	var committed int64
	if err := h.jobs.S.DB().QueryRow(`SELECT MAX(id) FROM attempts`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	h.svc.observeFetchAttempt(context.Background(), committed, "invalid", detail)
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	h.run("wr_rebuild_live_once", h.candidate("https://"+walledHost+"/p.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 1 {
		t.Fatalf("after a failed rebuild, one live wall, and a clean rebuild of two rows: fetched %v, want a normal fetch (two walls, not three)", h.fetched)
	}
}

// A rebuild that lands between FinishAttempt and observeFetchAttempt must not
// double-count: the committed row is counted by the rebuild under the same
// mutex that guards the counted set, so the live fold for that attempt id is
// the same row and is skipped. Two walls stay two walls however the calls
// interleave; without the set the loaded flag alone folds it again and
// the host suppresses on three.
func TestRebuildBetweenFinishAndObserveCountsOnce(t *testing.T) {
	h := newSuppressionHarness(t)
	cfg := config.DefaultHostSuppression()
	cfg.MinWallSpanHours = 0
	h.svc.EnableHostSuppression(cfg)
	h.seedWalls(walledHost, 1)
	detail := "*fetch.Error(http-403) host=" + walledHost + " final=" + walledHost
	// Stands in for FinishAttempt: commit the second wall.
	h.seed("invalid", detail, h.now.Add(-time.Minute))
	var committed int64
	if err := h.jobs.S.DB().QueryRow(`SELECT MAX(id) FROM attempts`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	// Another worker's rebuild lands here, between the commit and the fold.
	if h.svc.hostSuppression(context.Background()) == nil {
		t.Fatal("tracker is nil")
	}
	h.svc.observeFetchAttempt(context.Background(), committed, "invalid", detail)
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	h.run("wr_rebuild_race_once", h.candidate("https://"+walledHost+"/p.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 1 {
		t.Fatalf("after a rebuild between commit and fold of two rows: fetched %v, want a normal fetch (two walls, not three)", h.fetched)
	}
}

// Attempts FINISH out of order: 10 starts before 11 while 11 finishes first.
// A maximum-id watermark at 11 would silently drop 10 when it finishes later.
// The counted set keeps the actual ids, so each row counts exactly once: 11
// is skipped live (the rebuild counted it) and 10 is folded live (nothing
// counted it). The threshold is 2 so a dropped wall fetches (one wall, not
// two) while the correct two suppress.
func TestOutOfOrderFinishesCountExactlyOnce(t *testing.T) {
	h := newSuppressionHarness(t)
	cfg := config.DefaultHostSuppression()
	cfg.MinWallSpanHours = 0
	cfg.FailureThreshold = 2
	h.svc.EnableHostSuppression(cfg)
	ctx := context.Background()
	detail := "*fetch.Error(http-403) host=" + walledHost + " final=" + walledHost
	first, err := h.jobs.StartAttempt(ctx, "job_seed", 0, "fetch", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.jobs.StartAttempt(ctx, "job_seed", 0, "fetch", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("attempt ids out of order: first=%d second=%d", first, second)
	}
	// 11 finishes first, then a rebuild counts it before its live fold.
	if err := h.jobs.FinishAttempt(ctx, second, "invalid", http.StatusForbidden, detail); err != nil {
		t.Fatal(err)
	}
	if h.svc.hostSuppression(ctx) == nil {
		t.Fatal("tracker is nil")
	}
	h.svc.observeFetchAttempt(ctx, second, "invalid", detail)
	// 10 finishes later; nothing counted it, so the live fold must count it.
	if err := h.jobs.FinishAttempt(ctx, first, "invalid", http.StatusForbidden, detail); err != nil {
		t.Fatal(err)
	}
	h.svc.observeFetchAttempt(ctx, first, "invalid", detail)
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	h.run("wr_out_of_order_once", h.candidate("https://"+walledHost+"/p.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 0 {
		t.Fatalf("after out-of-order finishes of two walls: fetched %v, want no request (two walls suppress at threshold 2)", h.fetched)
	}
}

// A redirect into a host inside its 429 backoff is deferred on the hop, not
// sent: the job waits out the cooldown and nothing is recorded as a wall.
func TestRedirectIntoARateLimitedHostWaitsOutTheBackoff(t *testing.T) {
	h := newSuppressionHarness(t)
	const busy = "busy.example.org"
	tracker := h.svc.hostSuppression(context.Background())
	tracker.Observe429(busy, 10*time.Minute, h.now)
	var reached []string
	dl, err := fetch.New(fetch.DefaultPolicy(), publicHostResolver{}, handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Host)
		http.Redirect(w, r, "https://"+busy+"/paper.pdf", http.StatusFound)
	})})
	if err != nil {
		t.Fatal(err)
	}
	dl.SetHostGate(h.svc.HostGate)
	h.svc.Fetch = func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		return dl.DownloadWithHeaders(ctx, c.URL, c.RequestHeaders, path)
	}
	row := h.run("wr_hop_backoff", h.candidate("https://doi.example.org/10.1/x", resolver.AccessLicensedAPI))
	if len(reached) != 1 || reached[0] != "doi.example.org" {
		t.Fatalf("requests reached %v, want only the redirector inside the backoff", reached)
	}
	want := attemptRow{outcome: fetch.ClassRetryable, detail: "*fetch.Error(backoff) host=doi.example.org"}
	if got := h.fetchAttempts(row.ID); len(got) != 1 || got[0] != want {
		t.Fatalf("attempts = %+v, want [%+v]", got, want)
	}
	if row.State != job.StateRetryWait || h.candidateStatus(row.ID, "doi.example.org") != "retryable" {
		t.Fatalf("state %s, candidate %s; want a retry wait with the row retryable", row.State, h.candidateStatus(row.ID, "doi.example.org"))
	}
}

// A candidate whose redirect was refused at a walled publisher is revived
// for that publisher's probe. The skip recorded the publisher; keying the
// revival on the candidate's own host (a DOI resolver that is never walled)
// would never revive it, and the publisher would never be probed.
func TestProbeRevivesARedirectRefusedCandidate(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	const busy = "busy.example.org"
	var walledHits int
	dl, err := fetch.New(fetch.DefaultPolicy(), publicHostResolver{}, handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "doi.example.org":
			http.Redirect(w, r, "https://"+walledHost+"/via-doi.pdf", http.StatusFound)
		case busy:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			walledHits++
			w.WriteHeader(http.StatusForbidden)
		}
	})})
	if err != nil {
		t.Fatal(err)
	}
	dl.SetHostGate(h.svc.HostGate)
	h.svc.Fetch = func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		return dl.DownloadWithHeaders(ctx, c.URL, c.RequestHeaders, path)
	}
	row := h.run("wr_revive_redirect",
		h.candidate("https://doi.example.org/10.1/x", resolver.AccessLicensedAPI),
		h.candidate("https://"+busy+"/paper.pdf", resolver.AccessLicensedAPI))
	if walledHits != 0 || row.State != job.StateRetryWait {
		t.Fatalf("pass 1: walled reached %d times, state %s; want a skip and a retry wait", walledHits, row.State)
	}
	h.now = h.now.Add(8 * 24 * time.Hour)
	if _, err := h.jobs.S.DB().Exec(`UPDATE jobs SET retry_at = ? WHERE id = ?`, store.FormatTime(time.Now().Add(-time.Minute)), row.ID); err != nil {
		t.Fatal(err)
	}
	h.process(row.ID)
	if walledHits != 1 {
		t.Fatalf("probe pass reached the walled publisher %d times, want one probe", walledHits)
	}
	attempts := h.fetchAttempts(row.ID)
	probe := false
	for _, a := range attempts {
		if strings.HasPrefix(a.detail, "*fetch.Error(http-403) host=doi.example.org final="+walledHost) && strings.HasSuffix(a.detail, " probe") {
			probe = true
		}
	}
	if !probe {
		t.Fatalf("attempts %+v: want the revived candidate's 403 probe row", attempts)
	}
}

// A 429 that lands between candidateSuppression claiming a host's probe and
// the first hop defers that hop before any request. The unspent probe goes
// back: the host is probed once its backoff ends, not a whole probe interval
// later, and the deferred attempt carries no probe marker.
func TestBackoffOnTheFirstHopReleasesTheProbe(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	h.now = h.now.Add(8 * 24 * time.Hour)
	var reached []string
	dl, err := fetch.New(fetch.DefaultPolicy(), publicHostResolver{}, handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Host)
		w.WriteHeader(http.StatusForbidden)
	})})
	if err != nil {
		t.Fatal(err)
	}
	dl.SetHostGate(h.svc.HostGate)
	tracker := h.svc.hostSuppression(context.Background())
	h.svc.Fetch = func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		// The concurrent worker's 429 arrives after the probe was claimed.
		tracker.Observe429(walledHost, 10*time.Minute, h.now)
		return dl.DownloadWithHeaders(ctx, c.URL, c.RequestHeaders, path)
	}
	row := h.run("wr_probe_backoff", h.candidate("https://"+walledHost+"/p.pdf", resolver.AccessLicensedAPI))
	if len(reached) != 0 {
		t.Fatalf("requests reached %v, want none inside the backoff", reached)
	}
	want := attemptRow{outcome: fetch.ClassRetryable, detail: "*fetch.Error(backoff) host=" + walledHost}
	if got := h.fetchAttempts(row.ID); len(got) != 1 || got[0] != want {
		t.Fatalf("attempts = %+v, want [%+v] without the probe marker", got, want)
	}
	if got := tracker.Decide(walledHost, h.now.Add(time.Hour)); got != hostsuppress.Probe {
		t.Fatalf("after the backoff the host's decision is %v, want its probe still owed", got)
	}
}

// A deferral later in a redirect chain keeps the probe: the first hop
// already reached the probed host, so the probe is spent even though a 429
// elsewhere put the host into backoff before the chain came back to it.
func TestBackoffAfterTheProbedHopKeepsTheProbe(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	h.now = h.now.Add(8 * 24 * time.Hour)
	tracker := h.svc.hostSuppression(context.Background())
	var reached []string
	dl, err := fetch.New(fetch.DefaultPolicy(), publicHostResolver{}, handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if host == "" {
			host = r.URL.Host
		}
		reached = append(reached, host)
		if host == walledHost {
			http.Redirect(w, r, "https://mid.example.org/hop", http.StatusFound)
			return
		}
		// Another job's 429 from the probed host lands mid-chain.
		tracker.Observe429(walledHost, 10*time.Minute, h.now)
		http.Redirect(w, r, "https://"+walledHost+"/back.pdf", http.StatusFound)
	})})
	if err != nil {
		t.Fatal(err)
	}
	dl.SetHostGate(h.svc.HostGate)
	h.svc.Fetch = func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		return dl.DownloadWithHeaders(ctx, c.URL, c.RequestHeaders, path)
	}
	row := h.run("wr_probe_chain_backoff", h.candidate("https://"+walledHost+"/p.pdf", resolver.AccessLicensedAPI))
	if strings.Join(reached, ",") != walledHost+",mid.example.org" {
		t.Fatalf("requests reached %v, want the probed host then the redirector", reached)
	}
	if got := h.fetchAttempts(row.ID); len(got) != 1 || !strings.HasSuffix(got[0].detail, " probe") {
		t.Fatalf("attempts = %+v, want one deferred row carrying the probe marker", got)
	}
	if got := tracker.Decide(walledHost, h.now.Add(time.Hour)); got != hostsuppress.Skip {
		t.Fatalf("after the backoff the host's decision is %v, want Skip: the probe was spent", got)
	}
}
