// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"strings"
	"testing"
	"time"
)

type routerLedger struct {
	next int64
	rows map[int64]Record
}

func newRouterLedger() *routerLedger { return &routerLedger{rows: map[int64]Record{}} }
func (l *routerLedger) Upsert(_ context.Context, r Record) (Record, error) {
	for id, old := range l.rows {
		if old.Intent.Category == r.Intent.Category && old.Intent.EventKind == r.Intent.EventKind && old.Intent.AggregateKey == r.Intent.AggregateKey && old.Intent.Phase == r.Intent.Phase && old.Intent.WindowStart.Equal(r.Intent.WindowStart) {
			old.Count += r.Count
			old.LastAt = r.LastAt
			if old.DesktopState == "pending" || old.DesktopState == "held" {
				old.Intent.Detail = r.Intent.Detail
			}
			l.rows[id] = old
			return old, nil
		}
	}
	l.next++
	r.ID = l.next
	if r.Count == 0 {
		r.Count = 1
	}
	if r.DesktopState == "" {
		r.DesktopState = "pending"
	}
	if r.WebhookState == "" {
		r.WebhookState = "pending"
	}
	l.rows[r.ID] = r
	return r, nil
}
func (l *routerLedger) DueDesktop(_ context.Context, now time.Time, limit int) ([]Record, error) {
	out := []Record{}
	for _, r := range l.rows {
		if (r.DesktopState == "pending" || r.DesktopState == "held") && !r.AvailableAt.After(now) {
			out = append(out, r)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (l *routerLedger) DueWebhook(_ context.Context, now time.Time, limit int) ([]Record, error) {
	out := []Record{}
	for _, r := range l.rows {
		if r.WebhookState == "pending" && !r.AvailableAt.After(now) {
			out = append(out, r)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (l *routerLedger) ReserveDesktop(_ context.Context, id int64, _ time.Time, _ int) (bool, error) {
	r, ok := l.rows[id]
	if !ok || (r.DesktopState != "pending" && r.DesktopState != "held") {
		return false, nil
	}
	r.DesktopState = "reserved"
	l.rows[id] = r
	return true, nil
}
func (l *routerLedger) SetDesktopState(_ context.Context, id int64, state string, _ time.Time) error {
	r := l.rows[id]
	r.DesktopState = state
	l.rows[id] = r
	return nil
}
func (l *routerLedger) SetWebhookState(_ context.Context, id int64, state string, _ time.Time) error {
	r := l.rows[id]
	r.WebhookState = state
	l.rows[id] = r
	return nil
}
func (l *routerLedger) SupersedeCheckpoints(_ context.Context, aggregate string, _ time.Time) (int, error) {
	n := 0
	for id, r := range l.rows {
		if r.Intent.AggregateKey == aggregate && r.Intent.Category == CategoryCompletionBatch && r.Intent.Phase == PhaseCheckpoint && (r.DesktopState == "pending" || r.DesktopState == "held") {
			r.DesktopState = "superseded"
			l.rows[id] = r
			n++
		}
	}
	return n, nil
}
func (l *routerLedger) LatestCheckpoint(_ context.Context, aggregate string) (Record, bool, error) {
	for _, r := range l.rows {
		if r.Intent.AggregateKey == aggregate && r.Intent.Category == CategoryCompletionBatch && r.Intent.Phase == PhaseCheckpoint {
			return r, true, nil
		}
	}
	return Record{}, false, nil
}

type routerSender struct{ messages []string }

func (s *routerSender) Send(_ context.Context, message string) {
	s.messages = append(s.messages, message)
}

func TestRouterRejectsInvalidAndIgnoresAuditEvents(t *testing.T) {
	ledger := newRouterLedger()
	router := NewRouter(RouterOptions{Ledger: ledger})
	now := time.Now().UTC()
	intent := Intent{EventKind: "notify.attempted", Category: CategoryRequestOutcome, AggregateKey: "a", Phase: PhaseTerminal, WindowStart: now, HappenedAt: now}
	if err := router.Route(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if len(ledger.rows) != 0 {
		t.Fatal("notify audit recursively persisted")
	}
	intent.EventKind = "request.outcome"
	intent.Category = Category("unknown")
	if err := router.Route(context.Background(), intent); err == nil {
		t.Fatal("unknown category accepted")
	}
}

func TestRouterStripsTerminalControlsAndRunsWebhookIndependently(t *testing.T) {
	ledger := newRouterLedger()
	desktop, webhook := &routerSender{}, &routerSender{}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	policy := Policy{MaxPerHour: 1, Categories: map[Category]CategoryPolicy{CategoryRequestOutcome: {Desktop: "immediate", Webhook: "immediate", Window: time.Minute}}}
	router := NewRouter(RouterOptions{Ledger: ledger, Desktop: desktop, Webhook: webhook, Policy: policy, Now: func() time.Time { return now }})
	intent := Intent{EventKind: "request.outcome", Category: CategoryRequestOutcome, AggregateKey: "a", Phase: PhaseTerminal, WindowStart: now, HappenedAt: now, Message: "ready\x1b[31m\x1b[0m"}
	if err := router.Route(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if err := router.RunDueAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(desktop.messages) != 1 || strings.ContainsAny(desktop.messages[0], "\x1b") {
		t.Fatalf("desktop = %#v", desktop.messages)
	}
	if len(webhook.messages) != 1 || !strings.Contains(webhook.messages[0], "\x1b") {
		t.Fatalf("webhook = %#v", webhook.messages)
	}
}

func TestDecisionOpenedCoalescesWindowAndCarriesAggregateCount(t *testing.T) {
	ledger := newRouterLedger()
	sender := &routerSender{}
	now := time.Date(2026, 7, 15, 12, 5, 0, 0, time.UTC)
	window := now.Add(-5 * time.Minute)
	policy := Policy{MaxPerHour: 10, Categories: map[Category]CategoryPolicy{
		CategoryDecisionOpened: {Desktop: "immediate", Webhook: "off", Window: 5 * time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Desktop: sender, Policy: policy, Now: func() time.Time { return now }})
	for range 39 {
		intent := Intent{
			EventKind: "action.opened", Category: CategoryDecisionOpened,
			AggregateKey: "decision:" + window.Format(time.RFC3339Nano),
			Phase:        PhaseOpened, WindowStart: window, HappenedAt: now.Add(-time.Minute),
			Message: "papio has work waiting for you — open the papio inbox",
			Detail:  Event{Kind: "action.opened", Message: "papio has work waiting for you — open the papio inbox", Count: 1},
		}
		if err := router.Route(context.Background(), intent); err != nil {
			t.Fatal(err)
		}
	}
	if len(ledger.rows) != 1 {
		t.Fatalf("ledger rows = %d, want one coalesced row", len(ledger.rows))
	}
	if err := router.RunDueAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if len(sender.messages) != 1 || sender.messages[0] != "39 papers need you — open the papio inbox" {
		t.Fatalf("desktop messages = %#v, want one 39-paper notice", sender.messages)
	}
}

func TestCompletionAndDigestAvailabilityWindows(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	policy := Policy{
		DigestEvery:       time.Hour,
		CompletionQuiet:   10 * time.Minute,
		CompletionMaxHold: 2 * time.Hour,
		Categories: map[Category]CategoryPolicy{
			CategoryCompletionBatch: {Desktop: "digest", Webhook: "off", Window: time.Minute},
			CategoryDecisionPending: {Desktop: "digest", Webhook: "off", Window: 4 * time.Hour},
		},
	}
	router := NewRouter(RouterOptions{Ledger: ledger, Policy: policy, Now: func() time.Time { return now }})
	checkpoint := Intent{EventKind: "batch.checkpoint", Category: CategoryCompletionBatch, AggregateKey: "cohort:1", Phase: PhaseCheckpoint, WindowStart: now, HappenedAt: now, Detail: Event{Kind: "batch.checkpoint", Message: "checkpoint"}}
	if err := router.Route(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	pending := Intent{EventKind: "action.reminder", Category: CategoryDecisionPending, AggregateKey: "actions:pending", Phase: PhaseReminder, WindowStart: now, HappenedAt: now, Detail: Event{Kind: "action.reminder", Message: "reminder", Count: 1}}
	if err := router.Route(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	for _, row := range ledger.rows {
		switch row.Intent.Category {
		case CategoryCompletionBatch:
			if !row.AvailableAt.Equal(now.Add(10 * time.Minute)) {
				t.Fatalf("checkpoint available_at = %s, want +10m", row.AvailableAt)
			}
		case CategoryDecisionPending:
			if !row.AvailableAt.Equal(now.Add(time.Hour)) {
				t.Fatalf("digest available_at = %s, want +1h", row.AvailableAt)
			}
		}
	}
}

func TestWebhookDigestWaitsForDigestInterval(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	webhook := &routerSender{}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Now: func() time.Time { return now }})
	intent := Intent{EventKind: "request.outcome", Category: CategoryRequestOutcome, AggregateKey: "job:1", Phase: PhaseTerminal, WindowStart: now, HappenedAt: now, Detail: Event{Kind: "request.outcome", Message: "ready", Count: 1}}
	if err := router.Route(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	if len(webhook.messages) != 0 {
		t.Fatalf("webhook sent before digest interval: %#v", webhook.messages)
	}
	if err := router.RunDueAt(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(webhook.messages) != 1 || webhook.messages[0] != "ready" {
		t.Fatalf("webhook messages = %#v, want one deferred message", webhook.messages)
	}
}

func TestQuietReleaseHandlesDSTGapAndOverlap(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	quiet, err := ParseQuietHours("22:00-02:30", loc)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterOptions{Policy: Policy{QuietHours: quiet}})
	spring := time.Date(2026, 3, 8, 1, 45, 0, 0, loc)
	if got := router.nextQuietRelease(spring).In(loc); got.Hour() != 3 || got.Minute() != 0 {
		t.Fatalf("spring release = %s, want 03:00", got)
	}
	fallQuiet, err := ParseQuietHours("22:00-01:30", loc)
	if err != nil {
		t.Fatal(err)
	}
	router.policy.QuietHours = fallQuiet
	firstOccurrence := time.Date(2026, 11, 1, 1, 15, 0, 0, loc)
	if got := router.nextQuietRelease(firstOccurrence).In(loc); got.Hour() != 1 || got.Minute() != 30 {
		t.Fatalf("fall first release = %s, want first 01:30", got)
	}
	secondOccurrence := time.Unix(time.Date(2026, 11, 1, 6, 15, 0, 0, time.UTC).Unix(), 0).In(loc)
	if got := router.nextQuietRelease(secondOccurrence).In(loc); got.Day() != 2 {
		t.Fatalf("fall repeated release = %s, want next local day", got)
	}
}

func TestPreviewUsesSharedCategoryCopy(t *testing.T) {
	router := NewRouter(RouterOptions{})
	for _, category := range Categories() {
		got, err := router.Preview(category, 3)
		if err != nil {
			t.Fatal(err)
		}
		want := ComposeMessage(category, 3, Event{}, "")
		if got != want {
			t.Fatalf("preview %s = %q, want shared producer copy %q", category, got, want)
		}
	}
}
