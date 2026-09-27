// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/job"
	"papio/internal/resolver"
	"papio/internal/store"
)

// ADR-0031 host suppression through the real fetchCandidates path. The
// clock is fake and sits ahead of wall time, so a retry_wait job parked by
// a pass is never claimable by the wall-clock ClaimNext until a test moves
// its retry_at explicitly.

const walledHost = "walled.example.org"

type suppressionHarness struct {
	t       *testing.T
	svc     *Service
	jobs    *job.Store
	now     time.Time
	fetched []string
}

func newSuppressionHarness(t *testing.T) *suppressionHarness {
	t.Helper()
	svc, jobs := newTestService(t)
	h := &suppressionHarness{t: t, svc: svc, jobs: jobs, now: time.Now().UTC().Add(24 * time.Hour)}
	svc.Now = func() time.Time { return h.now }
	svc.Validate = passValidation()
	svc.EnableHostSuppression(config.DefaultHostSuppression())
	return h
}

func (h *suppressionHarness) candidate(rawURL string, basis string) resolver.Candidate {
	return resolver.Candidate{
		Source: "fixture", URL: rawURL,
		Version: resolver.VersionPublished, AccessBasis: basis, ReuseLicense: "unknown",
		ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
	}
}

// seed writes fetch-stage attempt rows as an earlier daemon would have.
func (h *suppressionHarness) seed(outcome, detail string, at time.Time) {
	h.t.Helper()
	if _, err := h.jobs.S.DB().Exec(`INSERT INTO attempts (job_id, stage, source, started_at, ended_at, outcome, detail)
		VALUES ('job_seed', 'fetch', 'fixture', ?, ?, ?, ?)`, store.FormatTime(at), store.FormatTime(at), outcome, detail); err != nil {
		h.t.Fatal(err)
	}
}

// seedWalls writes n 403 rows twelve hours apart, the latest an hour ago, so
// three of them span the default 24h minimum wall span.
func (h *suppressionHarness) seedWalls(host string, n int) {
	for i := 0; i < n; i++ {
		h.seed("invalid", "*fetch.Error(http-403) host="+host+" final="+host, h.now.Add(-time.Hour-time.Duration(i)*12*time.Hour))
	}
}

// fetchWith answers every candidate with status by host, counting requests.
func (h *suppressionHarness) fetchWith(status map[string]int) {
	h.svc.Fetch = func(_ context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		host := candidateHost(c.URL)
		h.fetched = append(h.fetched, host)
		code := status[host]
		if code == 0 || code == http.StatusOK {
			body := pdfBytes(c.URL)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				return fetch.Result{}, err
			}
			sum := sha256.Sum256(body)
			return fetch.Result{TempPath: path, SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body)),
				SniffedMIME: "application/pdf", ContentType: "application/pdf", HTTPStatus: 200, FinalHost: host}, nil
		}
		fe := &fetch.Error{Class: fetch.ClassInvalid, HTTPStatus: code, Msg: "permanent HTTP response", FinalHost: host}
		if code == http.StatusTooManyRequests || code >= 500 {
			fe.Class, fe.Msg, fe.RetryAfter = fetch.ClassRetryable, "temporary HTTP response", 10*time.Minute
		}
		return fetch.Result{}, fe
	}
}

func (h *suppressionHarness) run(reqID string, cands ...resolver.Candidate) *job.Row {
	h.t.Helper()
	h.svc.Resolvers = []ResolverEntry{{Adapter: &fakeResolver{name: "fixture", cands: cands}, Policy: config.Source{Enabled: true}}}
	ctx := context.Background()
	id, err := h.svc.Submit(ctx, doiRequestFor(reqID))
	if err != nil {
		h.t.Fatal(err)
	}
	return h.process(id)
}

func (h *suppressionHarness) process(id string) *job.Row {
	h.t.Helper()
	ctx := context.Background()
	row, err := h.jobs.ClaimNext(ctx, "worker", time.Minute)
	if err != nil || row == nil || row.ID != id {
		h.t.Fatalf("claim = %+v, %v; want job %s", row, err, id)
	}
	if err := h.svc.Process(ctx, row); err != nil {
		h.t.Fatalf("process: %v", err)
	}
	out, err := h.jobs.Get(ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

type attemptRow struct {
	outcome string
	status  int
	detail  string
}

func (h *suppressionHarness) fetchAttempts(jobID string) []attemptRow {
	h.t.Helper()
	rows, err := h.jobs.S.DB().Query(`SELECT COALESCE(outcome,''), COALESCE(http_status,0), COALESCE(detail,'') FROM attempts
		WHERE job_id = ? AND stage = 'fetch' ORDER BY id`, jobID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []attemptRow
	for rows.Next() {
		var r attemptRow
		if err := rows.Scan(&r.outcome, &r.status, &r.detail); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (h *suppressionHarness) candidateStatus(jobID, host string) string {
	h.t.Helper()
	var status string
	if err := h.jobs.S.DB().QueryRow(`SELECT status FROM candidates WHERE job_id = ? AND url_redacted LIKE ?`,
		jobID, "https://"+host+"/%").Scan(&status); err != nil {
		h.t.Fatal(err)
	}
	return status
}

func (h *suppressionHarness) lastTransitionDetail(jobID string) map[string]any {
	h.t.Helper()
	var raw string
	if err := h.jobs.S.DB().QueryRow(`SELECT detail_json FROM events WHERE job_id = ? AND kind = 'job.transition' ORDER BY seq DESC LIMIT 1`, jobID).Scan(&raw); err != nil {
		h.t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		h.t.Fatal(err)
	}
	return detail
}

func TestHostSuppressionThresholdSkipsTheDaemonFetch(t *testing.T) {
	for _, tc := range []struct {
		walls    int
		wantSkip bool
	}{{2, false}, {3, true}} {
		h := newSuppressionHarness(t)
		h.seedWalls(walledHost, tc.walls)
		h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
		row := h.run("wr_threshold", h.candidate("https://"+walledHost+"/paper.pdf", resolver.AccessLicensedAPI))
		attempts := h.fetchAttempts(row.ID)
		if !tc.wantSkip {
			if len(h.fetched) != 1 || len(attempts) != 1 || attempts[0].outcome != fetch.ClassInvalid {
				t.Fatalf("%d walls: fetched %v, attempts %+v; want one real fetch", tc.walls, h.fetched, attempts)
			}
			continue
		}
		if len(h.fetched) != 0 {
			t.Fatalf("%d walls: fetched %v, want no request", tc.walls, h.fetched)
		}
		want := attemptRow{outcome: "suppressed", detail: "*fetch.Error(wall) host=" + walledHost + " suppressed=" + walledHost}
		if len(attempts) != 1 || attempts[0] != want {
			t.Fatalf("attempts = %+v, want [%+v]", attempts, want)
		}
		if status := h.candidateStatus(row.ID, walledHost); status != "skipped" {
			t.Fatalf("candidate = %s, want skipped", status)
		}
		// A pass of only skips exhausts; it never parks on a retry wait.
		if row.State != job.StateUnavailable {
			t.Fatalf("state = %s, want unavailable (exhausted)", row.State)
		}
		detail := h.lastTransitionDetail(row.ID)
		hosts, _ := detail["suppressed_hosts"].([]any)
		if len(hosts) != 1 {
			t.Fatalf("exhaustion detail = %v, want one suppressed host", detail)
		}
		entry, _ := hosts[0].(map[string]any)
		if entry["host"] != walledHost || entry["kind"] != "wall" || entry["count"] != float64(1) {
			t.Fatalf("suppressed host entry = %v", entry)
		}
	}
}

// Only a daemon fetch-stage success clears a wall. A validate accept or a
// browser-adopted file proves nothing about unauthenticated daemon access.
func TestHostSuppressionClearsOnlyOnDaemonSuccess(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	if _, err := h.jobs.S.DB().Exec(`INSERT INTO attempts (job_id, stage, source, started_at, ended_at, outcome, detail)
		VALUES ('job_seed', 'validate', 'browser', ?, ?, 'accepted', 'sha256=abc')`,
		store.FormatTime(h.now.Add(-time.Minute)), store.FormatTime(h.now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	h.run("wr_browser_accept", h.candidate("https://"+walledHost+"/a.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 0 {
		t.Fatalf("a validate/browser accept cleared the wall: fetched %v", h.fetched)
	}

	h2 := newSuppressionHarness(t)
	h2.seedWalls(walledHost, 3)
	h2.seed("success", "bytes=10 mime=application/pdf host="+walledHost, h2.now.Add(-time.Minute))
	h2.fetchWith(nil)
	row := h2.run("wr_daemon_success", h2.candidate("https://"+walledHost+"/b.pdf", resolver.AccessLicensedAPI))
	if len(h2.fetched) != 1 || row.State != job.StateReady {
		t.Fatalf("after a daemon success: fetched %v, state %s; want one fetch and ready", h2.fetched, row.State)
	}
}

// An open-access skip keeps the browser recovery the failed fetch would have
// produced: the durable hint and the OA browser handoff.
func TestHostSuppressionOASkipKeepsTheBrowserRoute(t *testing.T) {
	h := newSuppressionHarness(t)
	h.svc.Config.AccessMode = config.ModeDelegated
	h.svc.Config.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	h.seedWalls(walledHost, 3)
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	oaURL := "https://" + walledHost + "/oa.pdf"
	row := h.run("wr_oa_skip", h.candidate(oaURL, resolver.AccessOpen))
	if len(h.fetched) != 0 {
		t.Fatalf("fetched %v, want the OA candidate skipped", h.fetched)
	}
	events, err := h.jobs.Events(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	hinted := false
	for _, event := range events {
		if event["kind"] == oaBrowserHintEventKind {
			hinted = true
		}
	}
	if !hinted {
		t.Fatalf("no %s event after an OA skip", oaBrowserHintEventKind)
	}
	actions, err := h.jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.JobID == row.ID && action.Kind == "openurl_handoff" && action.Detail == OABrowserHandoffActionDetail(oaURL) {
			return
		}
	}
	t.Fatalf("state %s, actions %+v: want the OA browser handoff", row.State, actions)
}

// A suppressed host admits one probe per interval: the skipped row is
// revived, the probe carries its marker, a failed probe re-arms the wall,
// and a successful one clears it.
func TestHostSuppressionProbeRevivesOneRowPerInterval(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden, "busy.example.org": http.StatusServiceUnavailable})
	walled := h.candidate("https://"+walledHost+"/paper.pdf", resolver.AccessLicensedAPI)
	busy := h.candidate("https://busy.example.org/paper.pdf", resolver.AccessLicensedAPI)
	row := h.run("wr_probe", walled, busy)
	if row.State != job.StateRetryWait || h.candidateStatus(row.ID, walledHost) != "skipped" {
		t.Fatalf("pass 1: state %s, walled %s; want retry_wait with the walled row skipped", row.State, h.candidateStatus(row.ID, walledHost))
	}
	requeue := func() {
		if _, err := h.jobs.S.DB().Exec(`UPDATE jobs SET retry_at = ? WHERE id = ?`, store.FormatTime(time.Now().Add(-time.Minute)), row.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Before the interval the row stays skipped.
	h.now = h.now.Add(6 * 24 * time.Hour)
	h.fetched = nil
	requeue()
	h.process(row.ID)
	if strings.Contains(strings.Join(h.fetched, ","), walledHost) {
		t.Fatalf("probed before the interval: fetched %v", h.fetched)
	}
	// Past the interval: exactly one probe, marked.
	h.now = h.now.Add(2 * 24 * time.Hour)
	h.fetched = nil
	requeue()
	h.process(row.ID)
	if n := strings.Count(strings.Join(h.fetched, ","), walledHost); n != 1 {
		t.Fatalf("probe pass fetched %v, want one probe of %s", h.fetched, walledHost)
	}
	attempts := h.fetchAttempts(row.ID)
	last := attempts[len(attempts)-1]
	if !strings.HasSuffix(last.detail, " probe") || last.status != http.StatusForbidden {
		// The busy candidate may be last; find the probe row.
		found := false
		for _, a := range attempts {
			if strings.Contains(a.detail, "host="+walledHost) && strings.HasSuffix(a.detail, " probe") && a.status == http.StatusForbidden {
				found = true
			}
		}
		if !found {
			t.Fatalf("attempts %+v: want a 403 probe row with the probe marker", attempts)
		}
	}
	// The failed probe re-armed the wall: a new job skips again.
	h.fetched = nil
	h.run("wr_probe_rearmed", h.candidate("https://"+walledHost+"/other.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 0 {
		t.Fatalf("after a failed probe: fetched %v, want a skip", h.fetched)
	}
	// The failed probe consumed its row (now invalid), so the next due probe
	// is whichever candidate reaches the host first. It succeeds, carries
	// the marker, and clears the host for ordinary fetches.
	h.now = h.now.Add(8 * 24 * time.Hour)
	h.fetchWith(nil)
	probed := h.run("wr_probe_success", h.candidate("https://"+walledHost+"/third.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 1 || probed.State != job.StateReady {
		t.Fatalf("successful probe: fetched %v, state %s; want one fetch and ready", h.fetched, probed.State)
	}
	if got := h.fetchAttempts(probed.ID); len(got) != 1 || got[0].outcome != "success" || !strings.HasSuffix(got[0].detail, " probe") {
		t.Fatalf("probe attempt = %+v, want a success row with the probe marker", got)
	}
	h.fetched = nil
	cleared := h.run("wr_probe_cleared", h.candidate("https://"+walledHost+"/fourth.pdf", resolver.AccessLicensedAPI))
	if got := h.fetchAttempts(cleared.ID); len(h.fetched) != 1 || cleared.State != job.StateReady || strings.HasSuffix(got[0].detail, " probe") {
		t.Fatalf("after a successful probe: fetched %v, state %s, attempts %+v; want an ordinary fetch", h.fetched, cleared.State, got)
	}
}

// State survives a restart: a new Service over the same store rebuilds the
// wall from attempts.detail, and legacy rows count for nothing.
func TestHostSuppressionStartupRebuild(t *testing.T) {
	h := newSuppressionHarness(t)
	// Live rows carry the store's wall-clock ended_at, seconds apart, so the
	// span requirement (tested in internal/hostsuppress) is off here.
	cfg := config.DefaultHostSuppression()
	cfg.MinWallSpanHours = 0
	h.svc.EnableHostSuppression(cfg)
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	for _, id := range []string{"wr_live_1", "wr_live_2", "wr_live_3"} {
		h.run(id, h.candidate("https://"+walledHost+"/"+id+".pdf", resolver.AccessLicensedAPI))
	}
	for i := 0; i < 10; i++ {
		h.seed("invalid", "*fetch.Error", h.now.Add(-time.Minute))
	}
	restarted := New(h.svc.Config, h.jobs, h.svc.Artifacts, nil)
	restarted.Now = h.svc.Now
	restarted.Validate = passValidation()
	restarted.EnableHostSuppression(cfg)
	h.svc = restarted
	h.fetched = nil
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	h.run("wr_after_restart", h.candidate("https://"+walledHost+"/after.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 0 {
		t.Fatalf("after restart: fetched %v, want the rebuilt wall to skip", h.fetched)
	}
	h.fetched = nil
	h.run("wr_legacy_host", h.candidate("https://legacy.example.org/p.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 1 {
		t.Fatalf("legacy-only host: fetched %v, want a normal fetch", h.fetched)
	}
}

// The per-hop gate refuses a redirect into a suppressed host before any
// request reaches it; the redirector itself is never suppressed.
func TestHostSuppressionRefusesARedirectIntoAWall(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	var reached []string
	dl, err := fetch.New(fetch.DefaultPolicy(), publicHostResolver{}, handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.Host)
		http.Redirect(w, r, "https://"+walledHost+"/paper.pdf", http.StatusFound)
	})})
	if err != nil {
		t.Fatal(err)
	}
	dl.SetHostGate(h.svc.HostGate)
	h.svc.Fetch = func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
		return dl.DownloadWithHeaders(ctx, c.URL, c.RequestHeaders, path)
	}
	row := h.run("wr_hop_redirect", h.candidate("https://doi.example.org/10.1/x", resolver.AccessLicensedAPI))
	if len(reached) != 1 || reached[0] != "doi.example.org" {
		t.Fatalf("requests reached %v, want only the redirector", reached)
	}
	attempts := h.fetchAttempts(row.ID)
	want := attemptRow{outcome: "suppressed", detail: "*fetch.Error(wall) host=doi.example.org suppressed=" + walledHost}
	if len(attempts) != 1 || attempts[0] != want {
		t.Fatalf("attempts = %+v, want [%+v]", attempts, want)
	}
}

// A 429 paces the host: the next candidate there waits without a request,
// and nothing is ever recorded as suppressed.
func TestHostRateLimitBacksOffWithoutSuppressing(t *testing.T) {
	h := newSuppressionHarness(t)
	h.fetchWith(map[string]int{"busy.example.org": http.StatusTooManyRequests})
	first := h.run("wr_429_first", h.candidate("https://busy.example.org/a.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 1 || first.State != job.StateRetryWait {
		t.Fatalf("first: fetched %v, state %s", h.fetched, first.State)
	}
	h.fetched = nil
	second := h.run("wr_429_second", h.candidate("https://busy.example.org/b.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 0 {
		t.Fatalf("inside the backoff: fetched %v, want no request", h.fetched)
	}
	if second.State != job.StateRetryWait || h.candidateStatus(second.ID, "busy.example.org") != "retryable" {
		t.Fatalf("second: state %s, candidate %s; want a retry wait", second.State, h.candidateStatus(second.ID, "busy.example.org"))
	}
	var suppressed int
	if err := h.jobs.S.DB().QueryRow(`SELECT COUNT(*) FROM attempts WHERE outcome = 'suppressed'`).Scan(&suppressed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if suppressed != 0 {
		t.Fatalf("%d suppressed rows: a rate limit must never suppress", suppressed)
	}
}

// A probe is one pass: every candidate of the probing job on the walled host
// runs in that pass, all marked; a different job in the meantime still skips.
func TestHostSuppressionProbeRunsTheWholePass(t *testing.T) {
	h := newSuppressionHarness(t)
	h.seedWalls(walledHost, 3)
	h.now = h.now.Add(8 * 24 * time.Hour)
	h.fetchWith(map[string]int{walledHost: http.StatusForbidden})
	a := h.candidate("https://"+walledHost+"/a.pdf", resolver.AccessLicensedAPI)
	b := h.candidate("https://"+walledHost+"/b.pdf", resolver.AccessLicensedAPI)
	b.Source = "fixture"
	row := h.run("wr_probe_pass", a, b)
	if len(h.fetched) != 2 {
		t.Fatalf("probe pass fetched %v, want both candidates of the probing job", h.fetched)
	}
	for _, attempt := range h.fetchAttempts(row.ID) {
		if !strings.HasSuffix(attempt.detail, " probe") {
			t.Fatalf("probe-pass attempt %+v lacks the probe marker", attempt)
		}
	}
	h.fetched = nil
	h.run("wr_after_probe_pass", h.candidate("https://"+walledHost+"/c.pdf", resolver.AccessLicensedAPI))
	if len(h.fetched) != 0 {
		t.Fatalf("another job after the failed probe pass fetched %v, want a skip", h.fetched)
	}
}
