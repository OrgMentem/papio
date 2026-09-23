// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package drive

import (
	"context"
	"slices"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/notify"
	"papio/internal/store"
	"papio/internal/store/storetest"
	"papio/internal/work"
)

// fakeBrowser stands in for the holder bridge. An empty request is the
// pacer's holder probe; a non-empty one is an open.
type fakeBrowser struct {
	live   bool
	opened [][]string
}

func (f *fakeBrowser) FocusHandoffs(_ context.Context, jobIDs []string) (int, bool, error) {
	if len(jobIDs) > 0 {
		f.opened = append(f.opened, append([]string(nil), jobIDs...))
	}
	if !f.live {
		return 0, false, nil
	}
	return len(jobIDs), true, nil
}

func (f *fakeBrowser) openedJobs() []string {
	var out []string
	for _, batch := range f.opened {
		out = append(out, batch...)
	}
	return out
}

type fakeSink struct{ intents []notify.Intent }

func (f *fakeSink) Route(_ context.Context, intent notify.Intent) error {
	f.intents = append(f.intents, intent)
	return nil
}

type harness struct {
	t       *testing.T
	jobs    *job.Store
	browser *fakeBrowser
	sink    *fakeSink
	clock   time.Time
	pacer   *Pacer
}

func newHarness(t *testing.T, enabled bool) *harness {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.DataDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := &harness{t: t, jobs: &job.Store{S: s}, browser: &fakeBrowser{live: true}, sink: &fakeSink{}, clock: time.Now()}
	cfg := config.Default()
	cfg.AccessMode = config.ModeDelegated
	cfg.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	cfg.Drive.Enabled = enabled
	h.pacer = &Pacer{Jobs: h.jobs, Config: cfg, Browser: h.browser, Notifier: h.sink, Now: func() time.Time { return h.clock }}
	return h
}

// park creates one job parked awaiting_human on an open action of kind.
func (h *harness) park(requestID, doi, kind string) string {
	h.t.Helper()
	ctx := context.Background()
	id, err := h.jobs.CreateRequest(ctx, requestID, work.Work{DOI: doi}, "", "",
		job.Policy{AccessMode: config.ModeDelegated, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalCLI)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		h.t.Fatal(err)
	}
	if err := h.jobs.ParkWithHumanAction(ctx, id, job.StateResolving, job.StateAwaitingHuman,
		kind, "institutional OpenURL handoff", nil, job.Access(true, "paywall")); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) run() {
	h.t.Helper()
	if err := h.pacer.RunDue(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) status() Status {
	h.t.Helper()
	status, err := h.pacer.Status(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return status
}

// settle stands in for the browser answering a drive.
func (h *harness) settle(jobID string) {
	h.t.Helper()
	if err := h.jobs.RecordEvent(context.Background(), jobID, "browser.provider_outcome", map[string]any{"outcome": "no_entitlement"}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) eventKinds(jobID string) []string {
	h.t.Helper()
	events, err := h.jobs.Events(context.Background(), jobID)
	if err != nil {
		h.t.Fatal(err)
	}
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event["kind"].(string))
	}
	return kinds
}

func TestPacerIsOffByDefault(t *testing.T) {
	h := newHarness(t, false)
	id := h.park("wr_off", "10.1000/off", "openurl_handoff")
	h.run()
	if len(h.browser.opened) != 0 {
		t.Fatalf("a default config opened %v", h.browser.opened)
	}
	status := h.status()
	if status.Enabled || !slices.Contains(status.Blockers, BlockDisabled) {
		t.Fatalf("status = %+v, want disabled", status)
	}
	if status.Next == nil || status.Next.JobID != id {
		t.Fatalf("status.next = %+v, want the parked job shown even while disabled", status.Next)
	}
}

// One paced open at a time: the second paper waits until the browser answers
// the first, and each open goes through the audited open path as "pacer".
func TestPacerOpensOldestFirstAndOneAtATime(t *testing.T) {
	h := newHarness(t, true)
	first := h.park("wr_first", "10.1000/first", "openurl_handoff")
	second := h.park("wr_second", "10.1000/second", "openurl_handoff")

	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{first}) {
		t.Fatalf("opened %v, want the oldest parked job only", got)
	}
	events, err := h.jobs.Events(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	var principal any
	for _, event := range events {
		if event["kind"] == HandoffOpenedEvent {
			principal = event["detail"].(map[string]any)["principal"]
		}
	}
	if principal != "pacer" {
		t.Fatalf("handoff.opened principal = %v, want pacer", principal)
	}

	h.clock = h.clock.Add(2 * time.Minute)
	h.run()
	if got := h.browser.openedJobs(); len(got) != 1 {
		t.Fatalf("opened %v while the first open was still in flight", got)
	}
	if status := h.status(); status.InFlight == nil || status.InFlight.JobID != first || !slices.Contains(status.Blockers, BlockInFlight) {
		t.Fatalf("status = %+v, want the first job in flight", status)
	}

	h.settle(first)
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{first, second}) {
		t.Fatalf("opened %v after the first settled, want the second next", got)
	}
}

func TestPacerRespectsTheHourlyBound(t *testing.T) {
	h := newHarness(t, true)
	h.pacer.Config.Drive.MaxOpensPerHour = 2
	ids := []string{
		h.park("wr_rate_a", "10.1000/rate-a", "openurl_handoff"),
		h.park("wr_rate_b", "10.1000/rate-b", "openurl_handoff"),
		h.park("wr_rate_c", "10.1000/rate-c", "openurl_handoff"),
	}
	for range 3 {
		h.run()
		for _, id := range ids {
			h.settle(id)
		}
		h.clock = h.clock.Add(5 * time.Minute)
	}
	if got := h.browser.openedJobs(); !slices.Equal(got, ids[:2]) {
		t.Fatalf("opened %v within one hour, want exactly two", got)
	}
	if status := h.status(); status.OpensLastHour != 2 || !slices.Contains(status.Blockers, BlockRateLimit) {
		t.Fatalf("status = %+v, want the hourly bound reported", status)
	}
	h.clock = h.clock.Add(time.Hour)
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, ids) {
		t.Fatalf("opened %v after the hour passed, want the third", got)
	}
}

func TestPacerDoesNothingWithoutAHolder(t *testing.T) {
	h := newHarness(t, true)
	h.browser.live = false
	id := h.park("wr_absent", "10.1000/absent", "openurl_handoff")
	h.run()
	if len(h.browser.opened) != 0 || slices.Contains(h.eventKinds(id), PacedOpenEvent) {
		t.Fatalf("opened %v with events %v while no holder was connected", h.browser.opened, h.eventKinds(id))
	}
	if status := h.status(); status.HolderConnected || !slices.Contains(status.Blockers, BlockHolderAbsent) {
		t.Fatalf("status = %+v, want holder_absent", status)
	}
}

// A provider serving its block page puts its host in cooldown. The pacer maps
// that host back to the DOI prefix of the papers that went there, so no paper
// under that prefix is opened until the cooldown ends.
func TestPacerSkipsProvidersInCooldown(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	blocked := h.park("wr_cool_blocked", "10.1016/j.example.2020.01.001", "openurl_handoff")
	other := h.park("wr_cool_other", "10.1234/other", "openurl_handoff")
	witness := h.park("wr_cool_witness", "10.1016/j.example.2020.01.002", "manual_download")
	until := h.clock.Add(30 * time.Minute).UTC().Format(time.RFC3339Nano)
	if err := h.jobs.RecordEvent(ctx, witness, job.ProviderCooldownEvent, map[string]any{
		"host": "www.sciencedirect.com", "until": until, "outcome": "rate_limited",
	}); err != nil {
		t.Fatal(err)
	}
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{other}) {
		t.Fatalf("opened %v during a sciencedirect cooldown, want only the unrelated paper", got)
	}
	if status := h.status(); status.Skipped[SkipHostCooldown] < 1 {
		t.Fatalf("status.skipped = %v, want the cooled paper counted", status.Skipped)
	}

	h.settle(other)
	h.clock = h.clock.Add(31 * time.Minute)
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{other, blocked}) {
		t.Fatalf("opened %v after the cooldown, want the cooled paper next", got)
	}
}

func TestPacerSkipsAJobBehindARecentChallenge(t *testing.T) {
	h := newHarness(t, true)
	challenged := h.park("wr_challenge", "10.1002/challenged", "openurl_handoff")
	sibling := h.park("wr_challenge_sibling", "10.1002/sibling", "openurl_handoff")
	other := h.park("wr_challenge_other", "10.5555/other", "openurl_handoff")
	if err := h.jobs.S.AppendEvent(context.Background(), challenged, "browser.error", map[string]any{"code": "challenge_blocked"}); err != nil {
		t.Fatal(err)
	}
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{other}) {
		t.Fatalf("opened %v, want neither the challenged paper nor its prefix sibling %s", got, sibling)
	}
}

// A sign-in nobody completes pauses the drive once, with one notification,
// and the drive resumes by itself when the sign-in returns.
func TestPacerPausesForAStaleSignInAndResumes(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	id := h.park("wr_signin", "10.1000/signin", "openurl_handoff")
	opened := h.clock.Add(-11 * time.Minute).UTC().Format(time.RFC3339Nano)
	if err := h.jobs.UpsertHumanGateObservation(ctx, job.HumanGateObservation{
		ID: "gate-signin-1", GateType: job.HumanGateLogin, ScopeClass: string(job.HumanGateScopeAuthenticationClaim),
		ScopeKey: "claim-signin", DependentJobIDs: []string{id}, ObservationRevision: 1, Status: job.HumanGateOpen,
		DetailJSON: `{"source":"auth_pending"}`, CreatedAt: opened, UpdatedAt: opened,
	}); err != nil {
		t.Fatal(err)
	}
	h.run()
	h.run()
	if len(h.browser.opened) != 0 {
		t.Fatalf("opened %v while a sign-in waited for a person", h.browser.opened)
	}
	if len(h.sink.intents) != 1 || h.sink.intents[0].Category != notify.CategoryDecisionOpened {
		t.Fatalf("notifications = %+v, want exactly one decision notice", h.sink.intents)
	}
	status := h.status()
	if !status.Paused || status.PausedReason != PauseSignIn {
		t.Fatalf("status = %+v, want paused for sign-in", status)
	}

	if err := h.jobs.ResolveHumanGate(ctx, job.HumanGateLogin, string(job.HumanGateScopeAuthenticationClaim), "claim-signin", 1); err != nil {
		t.Fatal(err)
	}
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{id}) {
		t.Fatalf("opened %v after the sign-in returned, want the drive resumed", got)
	}
	latest, found, err := h.jobs.LatestSystemEvent(ctx, PausedEvent, ResumedEvent)
	if err != nil || !found || latest.Kind != ResumedEvent {
		t.Fatalf("latest drive event = %+v found=%t err=%v, want drive.resumed", latest, found, err)
	}
}

// A sign-in inside its wait bound is somebody at the identity provider: the
// pacer waits without pausing or notifying.
func TestPacerWaitsOnAFreshSignIn(t *testing.T) {
	h := newHarness(t, true)
	id := h.park("wr_fresh_signin", "10.1000/fresh", "openurl_handoff")
	now := h.clock.UTC().Format(time.RFC3339Nano)
	if err := h.jobs.UpsertHumanGateObservation(context.Background(), job.HumanGateObservation{
		ID: "gate-fresh-1", GateType: job.HumanGateMFA, ScopeClass: string(job.HumanGateScopeAuthenticationClaim),
		ScopeKey: "claim-fresh", DependentJobIDs: []string{id}, ObservationRevision: 1, Status: job.HumanGateOpen,
		DetailJSON: `{}`, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.run()
	status := h.status()
	if len(h.browser.opened) != 0 || len(h.sink.intents) != 0 || status.Paused || !slices.Contains(status.Blockers, BlockSignInPending) {
		t.Fatalf("opened=%v notices=%d status=%+v, want a quiet wait", h.browser.opened, len(h.sink.intents), status)
	}
}

func TestPacerBacksOffAJobItOpened(t *testing.T) {
	h := newHarness(t, true)
	id := h.park("wr_backoff", "10.1000/backoff", "openurl_handoff")
	h.run()
	h.settle(id)
	h.clock = h.clock.Add(5 * time.Hour)
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{id}) {
		t.Fatalf("opened %v within the backoff, want one open", got)
	}
	h.clock = h.clock.Add(time.Hour + time.Minute)
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{id, id}) {
		t.Fatalf("opened %v after the backoff, want the job again", got)
	}
}

func TestOperatorPauseHoldsUntilResume(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	id := h.park("wr_pause", "10.1000/pause", "openurl_handoff")
	if _, err := h.pacer.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	h.run()
	if len(h.browser.opened) != 0 {
		t.Fatalf("opened %v while the operator paused the drive", h.browser.opened)
	}
	status, err := h.pacer.Resume(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Paused {
		t.Fatalf("status after resume = %+v", status)
	}
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{id}) {
		t.Fatalf("opened %v after resume", got)
	}
}

// A manual download is redriven through the same store function `jobs
// redrive` uses, then opened; a conservative advisory is never opened,
// because its only surface is the OS launcher outside papio's window.
func TestPacerRedrivesManualDownloadsAndNeverOpensAdvisories(t *testing.T) {
	h := newHarness(t, true)
	ctx := context.Background()
	advisory := h.park("wr_advisory", "10.1000/advisory", "openurl_available")
	manual := h.park("wr_manual", "10.1000/manual", "manual_download")
	h.run()
	if got := h.browser.openedJobs(); !slices.Equal(got, []string{manual}) {
		t.Fatalf("opened %v, want the redriven manual download and not the advisory %s", got, advisory)
	}
	open, err := h.jobs.ListOpenHumanActionsForJobs(ctx, []string{manual})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "openurl_handoff" {
		t.Fatalf("open actions after the paced redrive = %+v, want one institutional handoff", open)
	}
	if !slices.Contains(h.eventKinds(manual), "job.retry_requested") {
		t.Fatalf("events %v, want the ordinary redrive record", h.eventKinds(manual))
	}
}

// Quiet hours in the notification config also quiet the drive.
func TestPacerHonoursQuietHours(t *testing.T) {
	h := newHarness(t, true)
	h.park("wr_quiet", "10.1000/quiet", "openurl_handoff")
	local := h.clock.In(time.Local)
	start := local.Add(-time.Hour).Format("15:04")
	end := local.Add(time.Hour).Format("15:04")
	h.pacer.Config.Notify.QuietHours = start + "-" + end
	h.run()
	if status := h.status(); len(h.browser.opened) != 0 || !slices.Contains(status.Blockers, BlockQuietHours) {
		t.Fatalf("opened %v with blockers %v inside quiet hours", h.browser.opened, status.Blockers)
	}
}
