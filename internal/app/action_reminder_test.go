// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/notify"
)

// errReminderRouteCrash stands in for a kill between the pass's Route and its
// marker writes: the first attempt fails after queuing, so no marker exists.
var errReminderRouteCrash = errors.New("route crashed after queueing")

func openReminderAction(t *testing.T, svc *Service, jobs *job.Store, requestID string, createdAt time.Time, requiresAuth bool) (int64, string) {
	t.Helper()
	return openReminderActionKind(t, svc, jobs, requestID, createdAt, job.StateAwaitingHuman, "openurl_handoff", requiresAuth)
}

func openReminderActionKind(t *testing.T, svc *Service, jobs *job.Store, requestID string, createdAt time.Time, state, kind string, requiresAuth bool) (int64, string) {
	t.Helper()
	ctx := context.Background()
	row := resolvingExhaustionJob(t, svc, jobs, requestID)
	if err := jobs.Transition(ctx, row.ID, job.StateResolving, state,
		map[string]any{"reason": "reminder_test"}); err != nil {
		t.Fatal(err)
	}
	actionID, err := jobs.OpenHumanAction(ctx, row.ID, kind, "human action",
		job.Access(requiresAuth, "paywall"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE human_actions SET created_at = ? WHERE id = ?`,
		createdAt.UTC().Format(time.RFC3339Nano), actionID); err != nil {
		t.Fatal(err)
	}
	return actionID, row.ID
}

func newReminderTestService(t *testing.T, now *time.Time) (*Service, *job.Store, *fakeNotificationSink) {
	t.Helper()
	svc, jobs := newTestService(t)
	svc.Config.Browser.ActionExpirySeconds = 1800
	svc.Now = func() time.Time { return *now }
	sink := &fakeNotificationSink{}
	svc.Notifier = sink
	return svc, jobs, sink
}

func reminderDetail(t *testing.T, jobs *job.Store, jobID string, actionID int64) map[string]any {
	t.Helper()
	events, err := jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event["kind"] != actionReminderEvent {
			continue
		}
		detail, ok := event["detail"].(map[string]any)
		if ok && eventInt64(detail["action_id"]) == actionID {
			return detail
		}
	}
	t.Fatalf("missing %s event for action %d", actionReminderEvent, actionID)
	return nil
}

func TestActionReminderWaitsForThreshold(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	openReminderAction(t, svc, jobs, "wr_reminder_before_threshold", now.Add(-29*time.Minute), true)

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 0 {
		t.Fatalf("reminders = %q, want none before threshold", sink.reminders)
	}
}

func TestActionReminderNotifiesAtThreshold(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	actionID, jobID := openReminderAction(t, svc, jobs, "wr_reminder_at_threshold", now.Add(-30*time.Minute), true)

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "1 paper has been waiting 30m for your institution sign-in — run: papio actions open"
	if len(sink.reminders) != 1 || sink.reminders[0] != want {
		t.Fatalf("reminders = %q, want %q", sink.reminders, want)
	}
	detail := reminderDetail(t, jobs, jobID, actionID)
	if got := eventInt(detail["count"]); got != 1 {
		t.Fatalf("reminder count = %d, want 1", got)
	}
	if got := eventInt64(detail["age_seconds"]); got != int64((30*time.Minute)/time.Second) {
		t.Fatalf("reminder age_seconds = %d, want %d", got, int64((30*time.Minute)/time.Second))
	}
}

func TestActionReminderManualDownloadNamesManualStep(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	openReminderActionKind(t, svc, jobs, "wr_reminder_manual_download", now.Add(-30*time.Minute),
		job.StateAwaitingHuman, "manual_download", true)

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The manual step is still named, and now so is the command that reaches
	// the page it happens on.
	want := "1 paper has been waiting 30m for you to sign in to your institution, then download the PDF yourself — papio will adopt it — run: papio actions open"
	if len(sink.reminders) != 1 || sink.reminders[0] != want {
		t.Fatalf("reminders = %q, want %q", sink.reminders, want)
	}
}

// TestActionReminderNamesOpenOnlyForOpenableActions protects against an
// inherited access classification turning a manual replacement into an
// openable handoff in a reminder.
func TestActionReminderNamesOpenOnlyForOpenableActions(t *testing.T) {
	kinds := []string{
		"openurl_handoff",
		"manual_download",
		"verify_identity",
		"human_auth_required",
		"terms_acceptance_required",
		"openurl_available",
		"downloads_access_required",
	}
	for _, kind := range kinds {
		for _, requiresAuth := range []bool{false, true} {
			authName := "false"
			if requiresAuth {
				authName = "true"
			}
			t.Run(kind+"/requires_auth="+authName, func(t *testing.T) {
				action := job.HumanAction{Kind: kind, RequiresAuth: requiresAuth}
				message := (actionReminderBatch{
					jobs: map[string]struct{}{"job": {}}, oldestAge: time.Hour,
				}).message(reminderBatchIndex(action))
				if got, want := strings.Contains(message, actionsOpenCommand), HumanActionNextStepFor(action).Command == actionsOpenCommand; got != want {
					t.Fatalf("reminder names %q = %t, want %t: %q", actionsOpenCommand, got, want, message)
				}
			})
		}
	}
}

func TestActionReminderDoublesItsBackoff(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	actionID, jobID := openReminderAction(t, svc, jobs, "wr_reminder_backoff", now.Add(-30*time.Minute), true)
	runner := svc.ActionReminder()

	if err := runner.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(59 * time.Minute)
	if err := runner.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 1 {
		t.Fatalf("reminders before doubled wait = %q, want one", sink.reminders)
	}
	now = now.Add(time.Minute)
	if err := runner.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 2 {
		t.Fatalf("reminders after doubled wait = %q, want two", sink.reminders)
	}
	if got := eventInt(reminderDetail(t, jobs, jobID, actionID)["count"]); got != 2 {
		t.Fatalf("second reminder count = %d, want 2", got)
	}
}

func TestActionReminderRebasesFutureTimestampAfterClockRollback(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	actionID, jobID := openReminderAction(t, svc, jobs, "wr_reminder_clock_rollback", now.Add(-8*time.Hour), true)
	runner := svc.ActionReminder()

	if err := runner.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(-time.Hour)
	if err := runner.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 2 {
		t.Fatalf("reminders after rollback = %q, want the stale future deadline rebased", sink.reminders)
	}
	if got := eventInt(reminderDetail(t, jobs, jobID, actionID)["count"]); got != 2 {
		t.Fatalf("rebased reminder count = %d, want 2", got)
	}

	now = now.Add(119 * time.Minute)
	if err := runner.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 2 {
		t.Fatalf("reminders before preserved backoff = %q, want two", sink.reminders)
	}
	now = now.Add(time.Minute)
	if err := runner.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 3 {
		t.Fatalf("reminders after preserved backoff = %q, want three", sink.reminders)
	}
}

func TestActionReminderStateSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	openReminderAction(t, svc, jobs, "wr_reminder_restart", now.Add(-30*time.Minute), true)

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	restarted := &Service{Config: svc.Config, Jobs: jobs, Notifier: sink, Now: func() time.Time { return now }}
	if err := restarted.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 1 {
		t.Fatalf("reminders after restart = %q, want no immediate repeat", sink.reminders)
	}
}

func TestActionReminderStopsForResolvedAction(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	actionID, _ := openReminderAction(t, svc, jobs, "wr_reminder_resolved", now.Add(-8*time.Hour), true)
	if err := jobs.ResolveHumanAction(context.Background(), actionID, "resolved"); err != nil {
		t.Fatal(err)
	}

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 0 {
		t.Fatalf("reminders for resolved action = %q, want none", sink.reminders)
	}
}

func TestActionReminderMessageGroupsActionKinds(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	openReminderAction(t, svc, jobs, "wr_reminder_auth_oldest", now.Add(-8*time.Hour), true)
	openReminderAction(t, svc, jobs, "wr_reminder_auth_newer", now.Add(-7*time.Hour), true)
	openReminderAction(t, svc, jobs, "wr_reminder_open", now.Add(-8*time.Hour), false)
	for _, kind := range []string{"validation_error", "unsafe_pdf", "verify_identity"} {
		openReminderActionKind(t, svc, jobs, "wr_reminder_"+kind, now.Add(-8*time.Hour), job.StateNeedsReview, kind, false)
	}

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "6 papers need your attention — institution sign-in: 2, open handoff: 1, review: 3; oldest waiting 8h — run: papio actions list"
	if len(sink.reminders) != 1 || sink.reminders[0] != want {
		t.Fatalf("reminders = %q, want one bounded digest %q", sink.reminders, want)
	}
}

func TestActionReminderNamesOnlyActionsRecordedThisPass(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	freshID, freshJobID := openReminderAction(t, svc, jobs, "wr_reminder_fresh_event", now.Add(-8*time.Hour), true)
	_, dueJobID := openReminderAction(t, svc, jobs, "wr_reminder_due_event", now.Add(-8*time.Hour), true)
	if err := jobs.RecordEvent(context.Background(), freshJobID, actionReminderEvent, map[string]any{
		"action_id": freshID, "count": 1, "age_seconds": int64((8 * time.Hour) / time.Second),
		"reminded_at": now.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "1 paper has been waiting 8h for your institution sign-in — run: papio actions open"
	if len(sink.reminders) != 1 || sink.reminders[0] != want {
		t.Fatalf("reminders = %q, want only the newly due action", sink.reminders)
	}
	if events, err := jobs.Events(context.Background(), dueJobID); err != nil || len(events) == 0 {
		t.Fatalf("due action event = %v, %v", events, err)
	}
}

func TestActionReminderReachesOldestJobBeyondMaintenancePage(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	_, oldestJobID := openReminderAction(t, svc, jobs, "wr_reminder_oldest_page", now.Add(-8*time.Hour), true)
	ctx := context.Background()
	if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
		now.Add(-48*time.Hour).Format(time.RFC3339Nano), oldestJobID); err != nil {
		t.Fatal(err)
	}
	for i := range job.ListLimitMax {
		_, jobID := openReminderAction(t, svc, jobs, fmt.Sprintf("wr_reminder_new_%03d", i), now.Add(-time.Minute), true)
		if _, err := jobs.S.DB().ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`,
			now.Add(time.Duration(i+1)*time.Second).Format(time.RFC3339Nano), jobID); err != nil {
			t.Fatal(err)
		}
	}

	if err := svc.ActionReminder().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 1 {
		t.Fatalf("reminders = %q, want the oldest action beyond a newest-first page", sink.reminders)
	}
}

func TestActionReminderUsesConfiguredThreshold(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	svc.Config.Browser.ActionExpirySeconds = int(time.Hour / time.Second)
	openReminderAction(t, svc, jobs, "wr_reminder_configured_threshold", now.Add(-59*time.Minute), true)

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 0 {
		t.Fatalf("reminders before configured threshold = %q, want none", sink.reminders)
	}
	now = now.Add(time.Minute)
	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 1 {
		t.Fatalf("reminders at configured threshold = %q, want one", sink.reminders)
	}
}

func TestActionReminderBackoffCapsAtDay(t *testing.T) {
	if got := actionReminderBackoff(30*time.Minute, 6); got != 24*time.Hour {
		t.Fatalf("backoff = %s, want 24h", got)
	}
	if got := actionReminderBackoff(30*time.Minute, 20); got != 24*time.Hour {
		t.Fatalf("capped backoff = %s, want 24h", got)
	}
}

// The backoff above caps the interval at 24h but never the count, so before
// job.QuiesceAfter an action nobody could finish was re-notified once a day
// forever — the reported handoff reached seven. Going quiet is not expiry: the
// action stays open and `papio actions open` still drives it.
func TestActionReminderStopsAtTheQuiesceWindow(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	openReminderAction(t, svc, jobs, "wr_reminder_quiesced", now.Add(-job.QuiesceAfter-time.Hour), true)

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 0 {
		t.Fatalf("reminders = %q, want none once the action has gone quiet", sink.reminders)
	}
	// And the action is still open, so nothing has been taken away from the user.
	open, err := jobs.ListHumanActions(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open actions = %d, want the quiesced action still open and listable", len(open))
	}
}

func TestActionReminderStillNotifiesJustInsideTheQuiesceWindow(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	openReminderAction(t, svc, jobs, "wr_reminder_nearly_quiesced", now.Add(-job.QuiesceAfter+time.Hour), true)

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 1 {
		t.Fatalf("reminders = %q, want one — the window must not shorten by rounding", sink.reminders)
	}
}

// A failed route must not advance the backoff: with no marker recorded the
// next pass retries the alert once, then stays quiet until the backoff
// expires. The same ordering covers a crash between Route and markers in
// production: no marker means the action stays due.
func TestActionReminderRecoversARouteThatOutlivedItsMarkers(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	actionID, jobID := openReminderAction(t, svc, jobs, "wr_reminder_crash_window", now.Add(-30*time.Minute), true)

	// The alert fails to queue, so no action.reminder event records it and the
	// next pass retries the route instead of honoring a backoff.
	sink.routeErr = errReminderRouteCrash
	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 0 {
		t.Fatalf("reminders = %q, want none after the failed route", sink.reminders)
	}
	events, err := jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == actionReminderEvent {
			t.Fatalf("found %s before recovery, want the crash to precede markers", actionReminderEvent)
		}
	}

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 1 {
		t.Fatalf("reminders = %q, want the alert queued after the retry", sink.reminders)
	}
	detail := reminderDetail(t, jobs, jobID, actionID)
	if got := eventInt(detail["count"]); got != 1 {
		t.Fatalf("reminder count = %d, want 1: the retry must not double-count", got)
	}

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.reminders) != 1 {
		t.Fatalf("reminders = %q, want no repeat before the backoff expires", sink.reminders)
	}
}

// TestActionReminderRetryRoutesIdenticalDigest proves the crash-retry contract
// the pending-digest ledger depends on: a crash between Route and marker
// writes leaves the same actions due, so the next pass must route the
// identical absolute total under the identical five-part identity
// (category/event/aggregate/phase/WindowStart). The ledger replaces that
// identical retry instead of adding it, so the digest stays at the true due
// count (2) instead of doubling to 4.
func TestActionReminderRetryRoutesIdenticalDigest(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc, jobs, sink := newReminderTestService(t, &now)
	openReminderAction(t, svc, jobs, "wr_reminder_digest_a", now.Add(-30*time.Minute), true)
	openReminderAction(t, svc, jobs, "wr_reminder_digest_b", now.Add(-30*time.Minute), true)

	if err := svc.ActionReminder().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("intents after first pass = %d, want 1", len(sink.intents))
	}
	first := sink.intents[0]
	if first.Category != notify.CategoryDecisionPending || first.Phase != notify.PhaseReminder {
		t.Fatalf("first intent = %q/%q, want decision_pending/reminder", first.Category, first.Phase)
	}
	if first.AggregateKey != "actions:pending" {
		t.Fatalf("first aggregate = %q, want actions:pending", first.AggregateKey)
	}
	if first.Detail.Count != 2 {
		t.Fatalf("first digest count = %d, want the absolute total 2", first.Detail.Count)
	}

	// Simulate the crash: the Route queued (sink kept the intent) but none
	// of the per-action markers reached the job event stream.
	ctx := context.Background()
	if _, err := jobs.S.DB().ExecContext(ctx, `DELETE FROM events WHERE kind = ?`, actionReminderEvent); err != nil {
		t.Fatal(err)
	}

	if err := svc.ActionReminder().RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.intents) != 2 {
		t.Fatalf("intents after retry = %d, want 2 (first plus one identical retry)", len(sink.intents))
	}
	second := sink.intents[1]
	if second.Category != first.Category || second.EventKind != first.EventKind ||
		second.AggregateKey != first.AggregateKey || second.Phase != first.Phase {
		t.Fatalf("retry identity = %q/%q/%q/%q, want identical to first %q/%q/%q/%q",
			second.Category, second.EventKind, second.AggregateKey, second.Phase,
			first.Category, first.EventKind, first.AggregateKey, first.Phase)
	}
	if !second.WindowStart.Equal(first.WindowStart) {
		t.Fatalf("retry window = %v, want the stable first window %v", second.WindowStart, first.WindowStart)
	}
	if second.Detail.Count != first.Detail.Count {
		t.Fatalf("retry count = %d, want the identical absolute total %d (replace keeps %d, add would make %d)",
			second.Detail.Count, first.Detail.Count, first.Detail.Count, first.Detail.Count+second.Detail.Count)
	}
	if second.Detail.Count != 2 {
		t.Fatalf("retry digest count = %d, want 2: the retry must not double-count", second.Detail.Count)
	}
}
