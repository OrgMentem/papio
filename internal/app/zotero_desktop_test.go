// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/notify"
	"papio/internal/zotio"
)

// fakeZoteroDesktop is Zotero desktop as zotio reports it, plus the connector
// save that fails while it is closed. It counts every zotio process papio
// would have spawned.
type fakeZoteroDesktop struct {
	mu          sync.Mutex
	open        bool
	unsupported bool
	// statusErr fails `zotio desktop status` the way a broken zotio does.
	statusErr error
	waitErr   error
	// stuck is a zotio state ("unresponsive", "connector_off") for a Zotero
	// that runs while its connector cannot take requests.
	stuck       string
	opened      chan struct{}
	statusCalls int
	waitCalls   int
	activeWaits int
	maxWaits    int
	importCalls []string
}

func newFakeZoteroDesktop() *fakeZoteroDesktop {
	return &fakeZoteroDesktop{opened: make(chan struct{})}
}

func (f *fakeZoteroDesktop) DesktopStatus(context.Context) (zotio.DesktopStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	if f.unsupported {
		return zotio.DesktopStatus{}, zotio.ErrDesktopPresenceUnsupported
	}
	if f.statusErr != nil {
		return zotio.DesktopStatus{}, f.statusErr
	}
	if f.stuck != "" {
		return zotio.DesktopStatus{Running: true, State: f.stuck}, nil
	}
	return zotio.DesktopStatus{Running: f.open, ConnectorReachable: f.open}, nil
}

func (f *fakeZoteroDesktop) WaitForDesktop(ctx context.Context) (zotio.DesktopStatus, error) {
	f.mu.Lock()
	f.waitCalls++
	f.activeWaits++
	f.maxWaits = max(f.maxWaits, f.activeWaits)
	opened, waitErr, stuck := f.opened, f.waitErr, f.stuck
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.activeWaits--
		f.mu.Unlock()
	}()
	if waitErr != nil {
		return zotio.DesktopStatus{}, waitErr
	}
	if stuck != "" {
		// zotio answers a stuck Zotero at once (exit 15).
		return zotio.DesktopStatus{Running: true, State: stuck, Outcome: stuck}, nil
	}
	select {
	case <-ctx.Done():
		return zotio.DesktopStatus{}, ctx.Err()
	case <-opened:
		return zotio.DesktopStatus{Running: true, ConnectorReachable: true, Outcome: "ready"}, nil
	}
}

func (f *fakeZoteroDesktop) setOpen(open bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if open && !f.open {
		close(f.opened)
	}
	if !open && f.open {
		f.opened = make(chan struct{})
	}
	f.open = open
}

// PlanAndApply is the connector save: Zotero refuses it while closed, with
// the error the live store recorded.
func (f *fakeZoteroDesktop) PlanAndApply(_ context.Context, jobID string) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.importCalls = append(f.importCalls, jobID)
	if !f.open {
		return "", "UMXT6G74", "", zotio.WithErrorInfo(errors.New("applying Zotio mutation: checking for a resumable connector attachment: looking for a resumable temporary parent: connector refused"))
	}
	return "applied", "UMXT6G74", "ATTACH01", nil
}

func (f *fakeZoteroDesktop) setStuck(state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stuck = state
}

func (f *fakeZoteroDesktop) counts() (status, wait, imports, maxWaits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusCalls, f.waitCalls, len(f.importCalls), f.maxWaits
}

// seedReadyExistingItemJob drives a missing-PDF queue job, whose Zotero item
// already exists, to ready. Its import attaches through the desktop
// connector, which is the route that needs Zotero running.
func seedReadyExistingItemJob(t *testing.T, svc *Service, jobs *job.Store, wrID string) string {
	t.Helper()
	ctx := context.Background()
	request := doiRequestFor(wrID)
	request.ZotioItemKey = "UMXT6G74"
	id, err := svc.Submit(ctx, request)
	if err != nil {
		t.Fatalf("submit %s: %v", wrID, err)
	}
	row, err := jobs.ClaimNext(ctx, "worker", 100000000000)
	if err != nil || row == nil {
		t.Fatalf("claim %s: %v %+v", wrID, err, row)
	}
	if err := svc.Process(ctx, row); err != nil {
		t.Fatalf("process %s: %v", wrID, err)
	}
	if got, err := jobs.Get(ctx, id); err != nil || got.State != job.StateReady {
		t.Fatalf("job %s = %+v, %v; want ready", wrID, got, err)
	}
	return id
}

func autoImportStatuses(t *testing.T, jobs *job.Store, id string) []string {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for _, event := range events {
		if event["kind"] == "zotio.auto_import" {
			detail, _ := event["detail"].(map[string]any)
			status, _ := detail["status"].(string)
			statuses = append(statuses, status)
		}
	}
	return statuses
}

func zoteroWaitingIntents(sink *fakeNotificationSink) []notify.Intent {
	var out []notify.Intent
	for _, intent := range sink.intents {
		if intent.EventKind == "zotio.import_waiting" {
			out = append(out, intent)
		}
	}
	return out
}

// The live sequence of 2026-09-24: a paper reached ready while Zotero desktop
// was closed, the inline import and four one-minute passes each failed with
// zotero_connector_refused, all five attempts were gone in four minutes, and
// the paper was never filed after Zotero opened. Spacing the retries only
// moved the exhaustion to thirteen hours. A closed Zotero must be a wait: no
// attempt spent, no per-pass zotio process, one notice, and the import runs
// as soon as Zotero accepts connector saves.
func TestClosedZoteroWaitsWithoutSpendingAttemptsThenImportsWhenItOpens(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	svc.AutoImporter = desktop
	svc.ZoteroDesktop = desktop
	sink := &fakeNotificationSink{}
	svc.Notifier = sink

	id := seedReadyExistingItemJob(t, svc, jobs, "wr_zotero_closed_live")

	// An unattended day with Zotero closed: a pass each minute for the first
	// quarter hour, where the live store lost every attempt, then one each
	// quarter hour, which still reaches every spaced retry before the day ends.
	start := time.Now()
	passes := 0
	for elapsed := time.Minute; elapsed <= 24*time.Hour; {
		svc.Now = func() time.Time { return start.Add(elapsed) }
		if err := svc.ImportRetrier().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		passes++
		if elapsed < 15*time.Minute {
			elapsed += time.Minute
		} else {
			elapsed += 15 * time.Minute
		}
	}
	statusCalls, waitCalls, importCalls, _ := desktop.counts()
	if importCalls != 0 {
		t.Fatalf("connector save attempted %d times while Zotero was closed, want 0", importCalls)
	}
	if statusCalls != 1 {
		t.Fatalf("zotio desktop status ran %d times over %d passes, want 1 (presence is cached)", statusCalls, passes)
	}
	if waitCalls != 0 {
		t.Fatalf("the retry pass ran zotio desktop wait %d times, want 0 (only the watcher waits)", waitCalls)
	}
	if got := autoImportStatuses(t, jobs, id); strings.Join(got, ",") != "waiting" {
		t.Fatalf("auto-import outcomes = %v, want exactly one waiting event", got)
	}
	events, err := jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !importNeedsRetry(events) {
		t.Fatal("a day of closed Zotero exhausted the import attempts")
	}
	notices := zoteroWaitingIntents(sink)
	if len(notices) != 1 {
		t.Fatalf("waiting notifications = %d, want exactly 1 for the episode", len(notices))
	}
	if want := "Zotero is closed. 1 paper is ready to add. Open Zotero and papio adds it."; notices[0].Message != want {
		t.Fatalf("notification = %q, want %q", notices[0].Message, want)
	}

	// The watcher holds one waiter; opening Zotero ends it and files the paper.
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.ZoteroDesktopWatcher().Run(watchCtx)
	}()
	waitFor(t, "the watcher to start zotio desktop wait", func() bool {
		_, waits, _, _ := desktop.counts()
		return waits == 1
	})
	desktop.setOpen(true)
	waitFor(t, "the import after Zotero opened", func() bool {
		got := autoImportStatuses(t, jobs, id)
		return len(got) > 0 && got[len(got)-1] == "applied"
	})
	stop()
	<-done
	_, waitCalls, importCalls, maxWaits := desktop.counts()
	if importCalls != 1 || waitCalls != 1 || maxWaits != 1 {
		t.Fatalf("after Zotero opened: imports=%d waits=%d concurrent waits=%d, want 1/1/1", importCalls, waitCalls, maxWaits)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Zotero can quit after the probe that found it running. The failed save is
// the closure, not a fault of the paper, so it waits instead of counting.
func TestConnectorFailureAfterZoteroQuitsWaitsInsteadOfCounting(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	desktop.setOpen(true)
	svc.AutoImporter = desktop
	svc.ZoteroDesktop = desktop
	first := seedReadyExistingItemJob(t, svc, jobs, "wr_zotero_open_first")
	if got := autoImportStatuses(t, jobs, first); strings.Join(got, ",") != "applied" {
		t.Fatalf("import while Zotero ran = %v, want applied", got)
	}

	desktop.setOpen(false) // presence still cached as ready
	second := seedReadyExistingItemJob(t, svc, jobs, "wr_zotero_quit_second")
	if got := autoImportStatuses(t, jobs, second); strings.Join(got, ",") != "waiting" {
		t.Fatalf("import after Zotero quit = %v, want waiting (no error counted)", got)
	}
	if statusCalls, _, _, _ := desktop.counts(); statusCalls != 2 {
		t.Fatalf("status probes = %d, want 2 (startup, then after the failed save)", statusCalls)
	}
}

// A genuine connector refusal while Zotero runs is still a failure, spaced and
// capped as before.
func TestConnectorFailureWhileZoteroRunsStillCounts(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	desktop.setOpen(true)
	svc.ZoteroDesktop = desktop
	svc.AutoImporter = &selectiveImporter{fallback: zotio.WithErrorInfo(errors.New("connector saveItems: HTTP 500"))}
	id := seedReadyExistingItemJob(t, svc, jobs, "wr_zotero_plugin_broken")
	if got := autoImportStatuses(t, jobs, id); strings.Join(got, ",") != "error" {
		t.Fatalf("refusal while Zotero runs = %v, want one error", got)
	}
}

// An older zotio cannot report presence. papio then behaves as before the
// wait existed: it attempts the import and records the failure, and asks the
// registry question only once.
func TestOlderZotioWithoutPresenceKeepsSpacedRetries(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	desktop.unsupported = true
	svc.AutoImporter = desktop
	svc.ZoteroDesktop = desktop
	id := seedReadyExistingItemJob(t, svc, jobs, "wr_zotio_without_presence")
	pastImportBackoff(svc)
	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := autoImportStatuses(t, jobs, id); strings.Join(got, ",") != "error,error" {
		t.Fatalf("outcomes with an older zotio = %v, want two counted errors", got)
	}
	if svc.ZoteroDesktopWatcher() == nil {
		t.Fatal("watcher is nil although a presence source is configured")
	}
}

// An import whose route does not use the connector never waits for Zotero
// desktop: a linked-file import of a new item uploads nothing.
func TestNewItemImportDoesNotWaitForZotero(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	svc.Config.Zotio.AttachmentMode = "linked-file"
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	svc.ZoteroDesktop = desktop
	importer := &selectiveImporter{status: "applied"}
	svc.AutoImporter = importer
	id := seedReadyJobWithImportResult(t, svc, jobs, "wr_new_item_closed_zotero")
	if !importer.calledFor(id) {
		t.Fatal("new-item import waited for Zotero desktop")
	}
	if statusCalls, _, _, _ := desktop.counts(); statusCalls != 0 {
		t.Fatalf("new-item import asked for Zotero presence %d times, want 0", statusCalls)
	}
}

// One notice per closed episode, whatever the number of papers or passes. A
// restarted daemon routes the same aggregate and window, which the ledger
// coalesces into the notice it already delivered; a new episode after Zotero
// ran again is a new notice.
func TestZoteroWaitingNotificationOncePerEpisode(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	svc.AutoImporter = desktop
	svc.ZoteroDesktop = desktop
	sink := &fakeNotificationSink{}
	svc.Notifier = sink
	seedReadyExistingItemJob(t, svc, jobs, "wr_episode_a")
	seedReadyExistingItemJob(t, svc, jobs, "wr_episode_b")
	seedReadyExistingItemJob(t, svc, jobs, "wr_episode_c")
	for range 5 {
		if err := svc.ImportRetrier().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	notices := zoteroWaitingIntents(sink)
	if len(notices) != 1 {
		t.Fatalf("notices after 5 passes over 3 waiting papers = %d, want 1", len(notices))
	}
	if want := "Zotero is closed. 3 papers are ready to add. Open Zotero and papio adds them."; notices[0].Message != want {
		t.Fatalf("notice = %q, want %q", notices[0].Message, want)
	}
	if notices[0].Category != notify.CategorySystemDegraded || notices[0].Phase != notify.PhaseEpisode {
		t.Fatalf("notice category/phase = %s/%s, want system_degraded/episode", notices[0].Category, notices[0].Phase)
	}

	restarted := &Service{Config: svc.Config, Jobs: jobs, AutoImporter: desktop, ZoteroDesktop: desktop, Notifier: sink, Now: svc.Now}
	if err := restarted.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	notices = zoteroWaitingIntents(sink)
	if len(notices) != 2 {
		t.Fatalf("notices after restart = %d, want 2 routes", len(notices))
	}
	if notices[1].AggregateKey != notices[0].AggregateKey || !notices[1].WindowStart.Equal(notices[0].WindowStart) {
		t.Fatalf("restart routed a new notice identity %s@%s, want %s@%s so the ledger coalesces it",
			notices[1].AggregateKey, notices[1].WindowStart, notices[0].AggregateKey, notices[0].WindowStart)
	}

	// Zotero runs, files everything, and closes again with a new paper.
	desktop.setOpen(true)
	restarted.desktop.set(zotio.DesktopStatus{Running: true, ConnectorReachable: true})
	pastImportBackoff(restarted)
	for range 2 {
		if err := restarted.ImportRetrier().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	desktop.setOpen(false)
	restarted.desktop.forget()
	seedReadyExistingItemJob(t, svc, jobs, "wr_episode_d") // restarted holds no acquisition pipeline
	if err := restarted.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	notices = zoteroWaitingIntents(sink)
	if len(notices) != 3 || notices[2].WindowStart.Equal(notices[0].WindowStart) {
		t.Fatalf("second episode notices = %d (window %v), want a third notice with a new window", len(notices), notices[len(notices)-1].WindowStart)
	}
	if want := "Zotero is closed. 1 paper is ready to add. Open Zotero and papio adds it."; notices[2].Message != want {
		t.Fatalf("second episode notice = %q, want %q", notices[2].Message, want)
	}
}

// A waiter that fails (zotio exits 10 on a non-local base URL, for one) leaves
// nothing watching Zotero. The papers must not stay parked behind a presence
// nobody observes: after a backoff the watcher forgets it, and the next pass
// asks zotio once and imports when Zotero is open.
func TestFailedWaiterDoesNotStrandWaitingPapers(t *testing.T) {
	ctx := context.Background()
	previous := desktopWaitRetryDelays
	desktopWaitRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { desktopWaitRetryDelays = previous })
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	desktop.waitErr = errors.New("zotio desktop wait: exit status 10")
	svc.AutoImporter = desktop
	svc.ZoteroDesktop = desktop
	id := seedReadyExistingItemJob(t, svc, jobs, "wr_waiter_fails")

	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.ZoteroDesktopWatcher().Run(watchCtx)
	}()
	waitFor(t, "the failed waiter to release presence", func() bool {
		return svc.desktop.current() == desktopUnknown
	})
	stop()
	<-done

	desktop.setOpen(true)
	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := autoImportStatuses(t, jobs, id); strings.Join(got, ",") != "waiting,applied" {
		t.Fatalf("outcomes = %v, want waiting then applied", got)
	}
}

// In stored mode a new Zotero item is saved by Zotero desktop too, so it waits
// for a closed Zotero like an attachment to an existing item does.
func TestStoredNewItemImportWaitsForZotero(t *testing.T) {
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	svc.Config.Zotio.AttachmentMode = "stored"
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	svc.ZoteroDesktop = desktop
	svc.AutoImporter = desktop
	id := seedReadyJobWithImportResult(t, svc, jobs, "wr_new_item_stored_closed")
	if got := autoImportStatuses(t, jobs, id); strings.Join(got, ",") != "waiting" {
		t.Fatalf("stored new-item import with Zotero closed = %v, want waiting", got)
	}
	if _, _, imports, _ := desktop.counts(); imports != 0 {
		t.Fatalf("connector save attempted %d times while Zotero was closed, want 0", imports)
	}
}

// lockedSink records intents routed from the watcher's goroutine.
type lockedSink struct {
	mu      sync.Mutex
	intents []notify.Intent
}

func (l *lockedSink) Route(_ context.Context, intent notify.Intent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.intents = append(l.intents, intent)
	return nil
}

func (l *lockedSink) waitingMessages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, intent := range l.intents {
		if intent.EventKind == "zotio.import_waiting" {
			out = append(out, intent.Message)
		}
	}
	return out
}

func importReasons(t *testing.T, jobs *job.Store, id string) []string {
	t.Helper()
	events, err := jobs.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, event := range events {
		if event["kind"] == "zotio.auto_import" {
			detail, _ := event["detail"].(map[string]any)
			status, _ := detail["status"].(string)
			reason, _ := detail["reason"].(string)
			out = append(out, status+":"+reason)
		}
	}
	return out
}

// Seen live 2026-09-24: Zotero was open and held its profile lock, its
// connector port accepted connections, and nothing answered. "Open Zotero" is
// the wrong advice then. papio must say to restart it, once for that
// condition, keep the papers waiting without spending attempts, ask zotio
// again only after a pause, and import as soon as Zotero answers.
func TestStuckZoteroSaysRestartOnceAndRewaitsWithoutSpendingAttempts(t *testing.T) {
	ctx := context.Background()
	previous := desktopStuckRewait
	desktopStuckRewait = 20 * time.Millisecond
	t.Cleanup(func() { desktopStuckRewait = previous })
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	svc.AutoImporter = desktop
	svc.ZoteroDesktop = desktop
	sink := &lockedSink{}
	svc.Notifier = sink
	id := seedReadyExistingItemJob(t, svc, jobs, "wr_zotero_stuck")
	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatal(err)
	}

	desktop.setStuck(zotio.DesktopStateUnresponsive)
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.ZoteroDesktopWatcher().Run(watchCtx)
	}()
	waitFor(t, "several stuck re-waits", func() bool {
		_, waits, _, _ := desktop.counts()
		return waits >= 4
	})
	messages := sink.waitingMessages()
	want := []string{
		"Zotero is closed. 1 paper is ready to add. Open Zotero and papio adds it.",
		"Zotero is open but not responding. 1 paper is ready to add. Restart Zotero and papio adds it.",
	}
	if strings.Join(messages, "|") != strings.Join(want, "|") {
		t.Fatalf("notices = %q, want %q (one per condition, not one per re-wait)", messages, want)
	}
	if got := strings.Join(importReasons(t, jobs, id), ","); got != "waiting:zotero_not_running,waiting:zotero_unresponsive" {
		t.Fatalf("import events = %s, want one wait per condition", got)
	}
	if _, _, imports, maxWaits := desktop.counts(); imports != 0 || maxWaits != 1 {
		t.Fatalf("while stuck: imports=%d concurrent waiters=%d, want 0 and 1", imports, maxWaits)
	}

	// The operator restarts Zotero; the next re-wait sees it answer.
	desktop.setStuck("")
	desktop.setOpen(true)
	waitFor(t, "the import after Zotero was restarted", func() bool {
		got := autoImportStatuses(t, jobs, id)
		return len(got) > 0 && got[len(got)-1] == "applied"
	})
	stop()
	<-done
	if n := len(sink.waitingMessages()); n != 2 {
		t.Fatalf("notices after recovery = %d, want still 2", n)
	}
}

// A broken zotio can fail `desktop status` while Zotero is closed. The
// refused connector save is then the only evidence, and it is the closure:
// the paper waits, spends no attempt, and the next pass does not try the
// save again. A failure that says nothing about the connector still counts.
func TestConnectorRefusalWaitsWhenThePresenceProbeFails(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	desktop.statusErr = errors.New("zotio desktop status: exit status 1")
	svc.AutoImporter = desktop
	svc.ZoteroDesktop = desktop
	id := seedReadyExistingItemJob(t, svc, jobs, "wr_zotero_probe_fails")
	if got := strings.Join(importReasons(t, jobs, id), ","); got != "waiting:zotero_not_running" {
		t.Fatalf("import events = %s, want one wait and no counted error", got)
	}
	pastImportBackoff(svc)
	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, imports, _ := desktop.counts(); imports != 1 {
		t.Fatalf("connector saves = %d, want 1: the refusal holds the paper until the waiter sees Zotero", imports)
	}

	other, otherJobs := newTestService(t)
	other.Config.Zotio.AutoImport = true
	readyPipeline(other)
	broken := newFakeZoteroDesktop()
	broken.statusErr = errors.New("zotio desktop status: exit status 1")
	other.ZoteroDesktop = broken
	other.AutoImporter = &selectiveImporter{fallback: zotio.WithErrorInfo(errors.New("planning job: bundle validation: identity has no citation title"))}
	failed := seedReadyExistingItemJob(t, other, otherJobs, "wr_zotero_probe_fails_bundle")
	if got := strings.Join(autoImportStatuses(t, otherJobs, failed), ","); got != "error" {
		t.Fatalf("bundle failure with a failed probe = %s, want one counted error", got)
	}
}

// failingSink fails the first `failures` Zotero waiting notices, as a
// notification ledger that cannot write does.
type failingSink struct {
	mu       sync.Mutex
	failures int
	attempts int
	recorded int
}

func (f *failingSink) Route(_ context.Context, intent notify.Intent) error {
	if intent.EventKind != "zotio.import_waiting" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.failures > 0 {
		f.failures--
		return errors.New("notification ledger: disk I/O error")
	}
	f.recorded++
	return nil
}

// The episode's notice is latched only once the ledger holds it. A failed
// write must not leave a closed Zotero with no notice at all.
func TestZoteroWaitingNoticeIsRoutedAgainAfterAFailedRoute(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	svc.AutoImporter = desktop
	svc.ZoteroDesktop = desktop
	sink := &failingSink{failures: 1}
	svc.Notifier = sink
	seedReadyExistingItemJob(t, svc, jobs, "wr_notice_route_fails")
	for range 3 {
		if err := svc.ImportRetrier().RunDue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.attempts != 2 || sink.recorded != 1 {
		t.Fatalf("notice routes = %d (%d recorded), want 2 routes and 1 recorded notice", sink.attempts, sink.recorded)
	}
}

// Zotero opened with more papers waiting than one pass imports. The papers
// past the bound must stop telling doctor, status and activity to open
// Zotero: they are queued, spend no attempt, and later passes import them.
func TestWaitingPapersPastThePassBoundAreQueuedWhenZoteroOpens(t *testing.T) {
	ctx := context.Background()
	svc, jobs := newTestService(t)
	svc.Config.Zotio.AutoImport = true
	readyPipeline(svc)
	desktop := newFakeZoteroDesktop()
	svc.AutoImporter = desktop
	svc.ZoteroDesktop = desktop
	var ids []string
	for i := range maxImportsPerPass + 2 {
		ids = append(ids, seedReadyExistingItemJob(t, svc, jobs, fmt.Sprintf("wr_queue_%d", i)))
	}
	latest := func(id string) string {
		events := importReasons(t, jobs, id)
		return events[len(events)-1]
	}

	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.ZoteroDesktopWatcher().Run(watchCtx)
	}()
	waitFor(t, "the watcher to start zotio desktop wait", func() bool {
		_, waits, _, _ := desktop.counts()
		return waits == 1
	})
	desktop.setOpen(true)
	waitFor(t, "the papers past the pass bound to be queued", func() bool {
		queued := 0
		for _, id := range ids {
			if latest(id) == "queued:zotero_ready" {
				queued++
			}
		}
		return queued == 2
	})
	stop()
	<-done
	applied := 0
	for _, id := range ids {
		if latest(id) == "applied:" {
			applied++
		}
	}
	if applied != maxImportsPerPass {
		t.Fatalf("papers imported by the pass after Zotero opened = %d, want %d", applied, maxImportsPerPass)
	}

	if err := svc.ImportRetrier().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		events := importReasons(t, jobs, id)
		if latest(id) != "applied:" {
			t.Fatalf("job %s import events = %v, want the queued paper imported by the next pass", id, events)
		}
		for _, event := range events {
			if strings.HasPrefix(event, "error:") {
				t.Fatalf("job %s import events = %v, want no attempt spent", id, events)
			}
		}
	}
}
