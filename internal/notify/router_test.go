// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/store"
	"papio/internal/store/storetest"
)

type routerLedger struct {
	next                    int64
	rows                    map[int64]Record
	firstFailed             map[int64]time.Time
	failSetDesktopAvailable error
	failSetDesktopState     error
}

func newRouterLedger() *routerLedger {
	return &routerLedger{rows: map[int64]Record{}, firstFailed: map[int64]time.Time{}}
}
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

func sameFakeWebhookIdentity(a, b Record) bool {
	return a.Intent.Category == b.Intent.Category && a.Intent.EventKind == b.Intent.EventKind && a.Intent.AggregateKey == b.Intent.AggregateKey && a.Intent.Phase == b.Intent.Phase
}

func isFakePureReplay(stored, incoming Record) bool {
	a, aErr := json.Marshal(stored.Intent.Detail)
	b, bErr := json.Marshal(incoming.Intent.Detail)
	if aErr != nil || bErr != nil || string(a) != string(b) {
		return false
	}
	if stored.Count != incoming.Count {
		return false
	}
	if stored.Intent.JobID != incoming.Intent.JobID || stored.Intent.BatchID != incoming.Intent.BatchID || stored.Intent.ScanID != incoming.Intent.ScanID {
		return false
	}
	return !incoming.LastAt.After(stored.LastAt)
}

func (l *routerLedger) UpsertWebhookGeneration(ctx context.Context, r Record) (Record, error) {
	for id, old := range l.rows {
		if sameFakeWebhookIdentity(old, r) && old.Intent.WindowStart.Equal(r.Intent.WindowStart) {
			// Mirror the store rule: a deferred pending row returns
			// unchanged for a byte-identical retry, so a producer that
			// re-routes while the drain owns the retry does not inflate
			// the count.
			if old.WebhookState == "pending" && !old.WebhookAttemptedAt.IsZero() && isFakePureReplay(old, r) {
				return old, nil
			}
			if old.WebhookState == "" || old.WebhookState == "pending" {
				old.Count += r.Count
				old.LastAt = r.LastAt
				old.Intent.Detail = r.Intent.Detail
				l.rows[id] = old
				return old, nil
			}
			// Mirror the store rule: a byte-identical retry of the
			// same event returns the stored generation in any
			// non-pending state, while any event with a later LastAt
			// queues a new generation. The search covers the identity
			// row and every existing generation window.
			if isFakePureReplay(old, r) {
				return old, nil
			}
			for offset := int64(1); ; offset++ {
				candidate := r.Intent.WindowStart.Add(time.Duration(offset) * time.Nanosecond)
				var matched *Record
				free := true
				for _, generation := range l.rows {
					if sameFakeWebhookIdentity(generation, r) && generation.Intent.WindowStart.Equal(candidate) {
						free = false
						if isFakePureReplay(generation, r) {
							found := generation
							matched = &found
						}
						break
					}
				}
				if matched != nil {
					return *matched, nil
				}
				if free {
					l.next++
					r.ID = l.next
					r.Intent.WindowStart = candidate
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
			}
		}
	}
	return l.Upsert(ctx, r)
}

func (l *routerLedger) UpsertPendingDigest(_ context.Context, r Record) (Record, error) {
	for id, old := range l.rows {
		if old.Intent.Category == r.Intent.Category && old.Intent.EventKind == r.Intent.EventKind && old.Intent.AggregateKey == r.Intent.AggregateKey && old.Intent.Phase == r.Intent.Phase && old.Intent.WindowStart.Equal(r.Intent.WindowStart) {
			if old.DesktopState != "pending" && old.DesktopState != "held" && old.WebhookState != "pending" {
				return old, nil
			}
			old.Count = r.Count
			old.LastAt = r.LastAt
			old.AvailableAt = r.AvailableAt
			old.Intent.Detail = r.Intent.Detail
			old.Intent.Message = r.Intent.Message
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
		deferred := !r.WebhookAttemptedAt.IsZero()
		if r.WebhookState == "pending" && ((!deferred && !r.AvailableAt.After(now)) || (deferred && !r.WebhookAttemptedAt.After(now))) {
			out = append(out, r)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (l *routerLedger) ReserveDesktop(_ context.Context, id int64, _ time.Time, maxPerHour int) (bool, error) {
	r, ok := l.rows[id]
	if !ok || (r.DesktopState != "pending" && r.DesktopState != "held") {
		return false, nil
	}
	if maxPerHour > 0 {
		used := 0
		for _, row := range l.rows {
			if row.DesktopState == "reserved" || row.DesktopState == "attempted" {
				used++
			}
		}
		if used >= maxPerHour {
			return false, nil
		}
	}
	r.DesktopState = "reserved"
	l.rows[id] = r
	return true, nil
}
func (l *routerLedger) SetDesktopState(_ context.Context, id int64, state string, _ time.Time) (bool, error) {
	if l.failSetDesktopState != nil {
		return false, l.failSetDesktopState
	}
	r, ok := l.rows[id]
	if !ok {
		return false, nil
	}
	if state == "attempted" {
		if r.DesktopState != "reserved" {
			return false, nil
		}
		r.DesktopSentDetail, r.DesktopSentCount = r.Intent.Detail, r.Count
	} else if r.DesktopState != "pending" && r.DesktopState != "held" {
		return false, nil
	}
	r.DesktopState = state
	l.rows[id] = r
	return true, nil
}

// ClaimWebhook mirrors the store rule: a pending leg is claimable unless a
// deferral put its retry time after now.
func (l *routerLedger) ClaimWebhook(_ context.Context, id int64, now time.Time) (Record, bool, error) {
	r, ok := l.rows[id]
	if !ok || r.WebhookState != "pending" || r.WebhookAttemptedAt.After(now) {
		return Record{}, false, nil
	}
	r.WebhookState = "sending"
	l.rows[id] = r
	return r, true, nil
}
func (l *routerLedger) SetWebhookState(_ context.Context, id int64, state string, _ time.Time) error {
	r := l.rows[id]
	r.WebhookState = state
	l.rows[id] = r
	return nil
}

// DeferWebhook mirrors the store rule: only a claimed sending row returns to
// pending, with the retry time in WebhookAttemptedAt, and only while window
// has not elapsed since its first failure; settled rows are left alone.
func (l *routerLedger) DeferWebhook(_ context.Context, id int64, failedAt, retryAt time.Time, window time.Duration) (bool, error) {
	r := l.rows[id]
	if r.WebhookState != "sending" {
		return false, nil
	}
	first, seen := l.firstFailed[id]
	if seen && !first.After(failedAt.Add(-window)) {
		return false, nil
	}
	if !seen {
		l.firstFailed[id] = failedAt
	}
	r.WebhookState = "pending"
	r.WebhookAttemptedAt = retryAt
	l.rows[id] = r
	return true, nil
}

func (l *routerLedger) GetByID(_ context.Context, id int64) (Record, error) {
	r, ok := l.rows[id]
	if !ok {
		return Record{}, errors.New("notification row not found")
	}
	return r, nil
}
func (l *routerLedger) SetDesktopAvailable(_ context.Context, id int64, _ time.Time) error {
	if l.failSetDesktopAvailable != nil {
		return l.failSetDesktopAvailable
	}
	r := l.rows[id]
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
	// Both fall-back 01:30 instants share a wall clock, so an hour/minute
	// assertion cannot tell them apart. Compare absolute instants.
	// 01:15 is inside the repeated hour, so time.Date's zone choice for it is
	// documented as not guaranteed. Naming the instant in UTC picks the FIRST
	// 01:15 (EDT, -04:00) with no ambiguity; a local construction could hand
	// this line the second one and fail against wantFirst on a correct runtime.
	firstOccurrence := time.Date(2026, 11, 1, 5, 15, 0, 0, time.UTC).In(loc)
	wantFirst := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	if got := router.nextQuietRelease(firstOccurrence); !got.Equal(wantFirst) {
		t.Fatalf("fall first release = %s, want first 01:30 EDT (%s)", got.UTC(), wantFirst)
	}
	secondOccurrence := time.Unix(time.Date(2026, 11, 1, 6, 15, 0, 0, time.UTC).Unix(), 0).In(loc)
	wantSecond := time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC)
	if got := router.nextQuietRelease(secondOccurrence); !got.Equal(wantSecond) {
		t.Fatalf("fall repeated release = %s, want next local day 01:30 EST (%s)", got.UTC(), wantSecond)
	}
}

func TestPreviewUsesSharedCategoryCopy(t *testing.T) {
	router := NewRouter(RouterOptions{})
	for _, category := range Categories() {
		count := 3
		if CategoryRepresentsOneEvent(category) {
			count = 1
		}
		got, err := router.Preview(category, count)
		if err != nil {
			t.Fatal(err)
		}
		want := ComposeMessage(category, count, Event{}, "")
		if got != want {
			t.Fatalf("preview %s = %q, want shared producer copy %q", category, got, want)
		}
	}
}

// TestNotifyCategoryCopyAtRealisticCounts pins the exact public copy of every
// notification category at the counts papio actually produces: one event, a
// small aggregate, and a large browser cohort's worth of turns. It is the
// regression guard for the whole notification vocabulary, so every expectation
// is a literal string rather than a pattern, and the two categories that stand
// for exactly one event are asserted to reject an impossible count instead of
// rendering copy nobody will ever receive.
func TestNotifyCategoryCopyAtRealisticCounts(t *testing.T) {
	cases := []struct {
		category Category
		count    int
		want     string
		rejected bool
	}{
		{category: CategoryRequestOutcome, count: 1, want: "Request finished — open the papio inbox"},
		{category: CategoryRequestOutcome, count: 2, rejected: true},
		{category: CategoryRequestOutcome, count: 39, rejected: true},

		{category: CategoryDecisionOpened, count: 1, want: "1 paper needs you — open the papio inbox"},
		{category: CategoryDecisionOpened, count: 2, want: "2 papers need you — open the papio inbox"},
		{category: CategoryDecisionOpened, count: 39, want: "39 papers need you — open the papio inbox"},

		{category: CategoryDecisionPending, count: 1, want: "1 paper needs your attention — run: papio actions list"},
		{category: CategoryDecisionPending, count: 2, want: "2 papers need your attention — run: papio actions list"},
		{category: CategoryDecisionPending, count: 39, want: "39 papers need your attention — run: papio actions list"},

		{category: CategoryCompletionBatch, count: 1, want: "Batch update · 1 paper — open the papio inbox"},
		{category: CategoryCompletionBatch, count: 2, want: "Batch update · 2 papers — open the papio inbox"},
		{category: CategoryCompletionBatch, count: 39, want: "Batch update · 39 papers — open the papio inbox"},

		{category: CategoryDiscoveryNew, count: 1, want: "1 new watch hit — open the papio inbox"},
		{category: CategoryDiscoveryNew, count: 2, want: "2 new watch hits — open the papio inbox"},
		{category: CategoryDiscoveryNew, count: 39, want: "39 new watch hits — open the papio inbox"},

		{category: CategoryIntegrityNotice, count: 1, want: "1 library integrity notice — open the papio inbox"},
		{category: CategoryIntegrityNotice, count: 2, want: "2 library integrity notices — open the papio inbox"},
		{category: CategoryIntegrityNotice, count: 39, want: "39 library integrity notices — open the papio inbox"},

		{category: CategorySystemDegraded, count: 1, want: "papio cannot make progress — run: papio doctor"},
		{category: CategorySystemDegraded, count: 2, rejected: true},
		{category: CategorySystemDegraded, count: 39, rejected: true},
	}
	if len(cases) != 3*len(Categories()) {
		t.Fatalf("copy table covers %d cases, want every category at counts 1, 2, and 39", len(cases))
	}
	router := NewRouter(RouterOptions{})
	for _, tc := range cases {
		got, err := router.Preview(tc.category, tc.count)
		if tc.rejected {
			if !errors.Is(err, ErrPreviewCountUnrepresentable) {
				t.Fatalf("preview %s count %d = (%q, %v), want an unrepresentable-count error", tc.category, tc.count, got, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("preview %s count %d: %v", tc.category, tc.count, err)
		}
		if got != tc.want {
			t.Fatalf("preview %s count %d = %q, want %q", tc.category, tc.count, got, tc.want)
		}
		if shared := ComposeMessage(tc.category, tc.count, Event{}, ""); shared != tc.want {
			t.Fatalf("producer copy %s count %d = %q, want the previewed %q", tc.category, tc.count, shared, tc.want)
		}
	}
}

// TestNotifyComposedCopyStaysHonestAtEveryCount sweeps the counts between the pinned
// table entries so a future edit cannot reintroduce a placeholder plural or
// drop the recoverable surface at some count nobody thought to pin.
func TestNotifyComposedCopyStaysHonestAtEveryCount(t *testing.T) {
	pluralNouns := regexp.MustCompile(`\b(papers|hits|notices)\b`)
	singularNouns := regexp.MustCompile(`\b(paper|hit|notice)\b`)
	for _, category := range Categories() {
		for count := 1; count <= 40; count++ {
			got := ComposeMessage(category, count, Event{}, "")
			for _, banned := range []string{"(s)", "%!", "delivered", "estimated", "ETA"} {
				if strings.Contains(got, banned) {
					t.Fatalf("%s count %d = %q, must not contain %q", category, count, got, banned)
				}
			}
			if !strings.Contains(got, "open the papio inbox") && !strings.Contains(got, "run: papio ") {
				t.Fatalf("%s count %d = %q, must name a recoverable papio surface or command", category, count, got)
			}
			// A category that carries a count must agree with it; one that
			// stands for a single event carries no counted noun at all.
			if CategoryRepresentsOneEvent(category) {
				continue
			}
			if count == 1 && pluralNouns.MatchString(got) {
				t.Fatalf("%s count 1 = %q, wants singular nouns", category, got)
			}
			if count > 1 && singularNouns.MatchString(got) {
				t.Fatalf("%s count %d = %q, wants plural nouns", category, count, got)
			}
		}
	}
}

// TestPreviewClampsAbsentCountAndExplainsImpossibleOnes keeps the operator
// surface teachable: an omitted count previews one event, and an impossible one
// says which category can aggregate instead.
func TestPreviewClampsAbsentCountAndExplainsImpossibleOnes(t *testing.T) {
	router := NewRouter(RouterOptions{})
	for _, count := range []int{0, -5} {
		got, err := router.Preview(CategoryRequestOutcome, count)
		if err != nil {
			t.Fatalf("preview count %d: %v", count, err)
		}
		if got != "Request finished — open the papio inbox" {
			t.Fatalf("preview count %d = %q, want the one-request copy", count, got)
		}
	}
	_, err := router.Preview(CategoryRequestOutcome, 27)
	if err == nil {
		t.Fatal("preview request_outcome count 27 = nil error, want a rejection")
	}
	for _, fragment := range []string{"request_outcome", "exactly one standalone request", "count of 27", "preview it with count 1", "completion_batch"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("rejection %q must explain %q", err, fragment)
		}
	}
	if _, err := router.Preview(Category("made_up"), 1); err == nil {
		t.Fatal("preview of an unknown category must fail")
	}
}

// TestNotifyTestSendsOneSharedCopyPerCategory proves the explicit operator test uses the
// same composition as preview and the producers rather than its own template.
func TestNotifyTestSendsOneSharedCopyPerCategory(t *testing.T) {
	for _, category := range Categories() {
		sender := &routerSender{}
		router := NewRouter(RouterOptions{Desktop: sender})
		message, err := router.Test(context.Background(), category)
		if err != nil {
			t.Fatalf("test %s: %v", category, err)
		}
		want := ComposeMessage(category, 1, Event{}, "")
		if message != want {
			t.Fatalf("test %s returned %q, want shared copy %q", category, message, want)
		}
		if len(sender.messages) != 1 || sender.messages[0] != want {
			t.Fatalf("test %s sent %#v, want exactly one %q", category, sender.messages, want)
		}
	}
}

func TestRunDueAtPropagatesSetDesktopAvailableFailure(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	ledger.failSetDesktopAvailable = errors.New("disk full")
	quiet, _ := ParseQuietHours("22:00-12:30", time.UTC)
	policy := Policy{MaxPerHour: 10, QuietHours: quiet, Categories: map[Category]CategoryPolicy{
		CategoryDiscoveryNew: {Desktop: "immediate", Webhook: "off", Window: time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Desktop: &routerSender{}, Policy: policy, Now: func() time.Time { return now }})
	rec := Record{Intent: Intent{EventKind: "discovery.new", Category: CategoryDiscoveryNew, AggregateKey: "w:1", Phase: PhaseScan, WindowStart: now, HappenedAt: now, Message: "hit", Detail: Event{Message: "hit"}}, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, DesktopState: "pending"}
	if _, err := ledger.Upsert(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	// Force quiet hours so RunDueAt goes through deferDesktop
	if err := router.RunDueAt(context.Background(), now); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("RunDueAt = %v, want SetDesktopAvailable error", err)
	}
}

func TestRunDueAtPropagatesRateLimitDeferralFailure(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	ledger.failSetDesktopAvailable = errors.New("rate defer failed")
	policy := Policy{MaxPerHour: 1, Categories: map[Category]CategoryPolicy{
		CategoryDecisionOpened: {Desktop: "immediate", Webhook: "off", Window: time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Desktop: &routerSender{}, Policy: policy, Now: func() time.Time { return now }})
	// Insert two rows; first will exhaust rate limit for second
	rec1 := Record{Intent: Intent{EventKind: "action.opened", Category: CategoryDecisionOpened, AggregateKey: "a:1", Phase: PhaseOpened, WindowStart: now, HappenedAt: now, Message: "opened", Detail: Event{Message: "opened"}}, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, DesktopState: "pending"}
	rec2 := Record{Intent: Intent{EventKind: "action.opened", Category: CategoryDecisionOpened, AggregateKey: "a:2", Phase: PhaseOpened, WindowStart: now.Add(time.Second), HappenedAt: now.Add(time.Second), Message: "opened2", Detail: Event{Message: "opened2"}}, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, DesktopState: "pending"}
	if _, err := ledger.Upsert(context.Background(), rec1); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Upsert(context.Background(), rec2); err != nil {
		t.Fatal(err)
	}
	if err := router.RunDueAt(context.Background(), now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "rate defer failed") {
		t.Fatalf("RunDueAt = %v, want rate defer error", err)
	}
}

func TestRunDueAtDoesNotAuditSupersessionWhenWriteFails(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	ledger.failSetDesktopState = errors.New("state write failed")
	audited := 0
	recorder := &countingActivity{count: &audited}
	policy := Policy{MaxPerHour: 10, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "immediate", Webhook: "off", Window: time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Desktop: &routerSender{}, Policy: policy, Activity: recorder, Now: func() time.Time { return now }, Revalidate: func(_ context.Context, _ Record) (bool, error) { return false, nil }})
	rec := Record{Intent: Intent{EventKind: "request.outcome", Category: CategoryRequestOutcome, AggregateKey: "job:1", Phase: PhaseTerminal, WindowStart: now, HappenedAt: now, Message: "done", Detail: Event{Message: "done"}}, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, DesktopState: "pending"}
	if _, err := ledger.Upsert(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err := router.RunDueAt(context.Background(), now); err == nil || !strings.Contains(err.Error(), "state write failed") {
		t.Fatalf("RunDueAt = %v, want supersede write error", err)
	}
	if audited != 0 {
		t.Fatalf("audit count = %d, want 0 when supersede write failed", audited)
	}
}

type countingActivity struct {
	count *int
}

func (c *countingActivity) RecordSystemEvent(_ context.Context, _ string, _ map[string]any) error {
	*c.count++
	return nil
}

// recordingActivity captures the router's advisory audit records so tests can
// assert a failure was recorded without reading the store activity table.
type recordingActivity struct {
	failures []recordedFailure
}

type recordedFailure struct {
	kind  string
	cause string
}

func (a *recordingActivity) RecordSystemEvent(_ context.Context, kind string, detail map[string]any) error {
	if kind == "notify.webhook_failed" {
		cause, _ := detail["cause"].(string)
		a.failures = append(a.failures, recordedFailure{kind: kind, cause: cause})
	}
	return nil
}

func TestSamePublicOutcomeReturnsErrorOnUnmarshalableDetail(t *testing.T) {
	// Event.Detail is map[string]any — an unmarshalable value like a channel is reachable.
	ch := make(chan int)
	a := Event{Kind: "request.outcome", Message: "a", Detail: map[string]any{"bad": ch}}
	b := Event{Kind: "request.outcome", Message: "a", Detail: map[string]any{"bad": ch}}
	if _, err := samePublicOutcome(a, b); err == nil {
		t.Fatal("samePublicOutcome = nil error, want marshal failure")
	}
}

func TestRouteReturnsErrorWhenSamePublicOutcomeFails(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	// Seed a prior checkpoint with attempted state
	chk := Record{Intent: Intent{EventKind: "batch.checkpoint", Category: CategoryCompletionBatch, AggregateKey: "cohort:1", Phase: PhaseCheckpoint, WindowStart: now, HappenedAt: now, Detail: Event{Message: "checkpoint"}}, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, DesktopState: "attempted"}
	ledger.rows[1] = chk
	ledger.rows[1] = Record{ID: 1, Intent: Intent{EventKind: "batch.checkpoint", Category: CategoryCompletionBatch, AggregateKey: "cohort:1", Phase: PhaseCheckpoint, WindowStart: now, HappenedAt: now, Detail: Event{Message: "checkpoint"}}, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, DesktopState: "attempted"}
	ledger.next = 1
	router := NewRouter(RouterOptions{Ledger: ledger, Policy: Policy{}, Now: func() time.Time { return now }})
	ch := make(chan int)
	intent := Intent{EventKind: "batch.final", Category: CategoryCompletionBatch, AggregateKey: "cohort:1", Phase: PhaseFinal, WindowStart: now.Add(time.Hour), HappenedAt: now.Add(time.Hour), Detail: Event{Kind: "batch.final", Detail: map[string]any{"bad": ch}}}
	if err := router.Route(context.Background(), intent); err == nil {
		t.Fatal("Route = nil error, want samePublicOutcome marshal failure")
	}
}

func TestStoreLedgerRejectsCorruptRows(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sl := NewStoreLedger(db)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	// Upsert a valid row then corrupt payload
	rec := Record{Intent: Intent{EventKind: "request.outcome", Category: CategoryRequestOutcome, AggregateKey: "job:corrupt", Phase: PhaseTerminal, WindowStart: now, HappenedAt: now, Message: "ok", Detail: Event{Message: "ok"}}, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, DesktopState: "pending", WebhookState: "pending"}
	stored, err := sl.Upsert(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET payload_json=? WHERE id=?`, "{bad json", stored.ID); err != nil {
		t.Fatal(err)
	}
	check := func(name string, fn func() error) {
		t.Helper()
		if err := fn(); err == nil {
			t.Fatalf("%s with bad payload = nil, want error naming row %d", name, stored.ID)
		} else if !strings.Contains(err.Error(), itoaNotify(stored.ID)) {
			t.Fatalf("%s error %q must name row %d", name, err, stored.ID)
		}
	}
	check("DueDesktop", func() error { _, err := sl.DueDesktop(ctx, now.Add(time.Hour), 10); return err })
	check("DueWebhook", func() error { _, err := sl.DueWebhook(ctx, now.Add(time.Hour), 10); return err })
	check("Upsert", func() error {
		_, err := sl.Upsert(ctx, Record{Intent: Intent{EventKind: "request.outcome", Category: CategoryRequestOutcome, AggregateKey: "job:corrupt", Phase: PhaseTerminal, WindowStart: now, HappenedAt: now, Detail: Event{Message: "new"}}, FirstAt: now, LastAt: now, AvailableAt: now, Count: 1, DesktopState: "pending", WebhookState: "pending"})
		return err
	})
	// LatestCheckpoint requires a completion_batch checkpoint row; create separate
	now2 := now.Add(time.Hour)
	chk := Record{Intent: Intent{EventKind: "batch.checkpoint", Category: CategoryCompletionBatch, AggregateKey: "cohort:corrupt", Phase: PhaseCheckpoint, WindowStart: now2, HappenedAt: now2, Detail: Event{Message: "chk"}}, FirstAt: now2, LastAt: now2, AvailableAt: now2, Count: 1, DesktopState: "pending", WebhookState: "pending"}
	chkStored, err := sl.Upsert(ctx, chk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET payload_json=? WHERE id=?`, "{bad", chkStored.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sl.LatestCheckpoint(ctx, "cohort:corrupt"); err == nil {
		t.Fatalf("LatestCheckpoint with bad payload = nil, want error naming row %d", chkStored.ID)
	} else if !strings.Contains(err.Error(), itoaNotify(chkStored.ID)) {
		t.Fatalf("LatestCheckpoint error %q must name row %d", err, chkStored.ID)
	}
	// Timestamp corruption path
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET payload_json=? WHERE id=?`, `{"message":"fixed"}`, stored.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET first_at=? WHERE id=?`, "not-rfc3339", stored.ID); err != nil {
		t.Fatal(err)
	}
	check("DueDesktop bad timestamp", func() error { _, err := sl.DueDesktop(ctx, now.Add(time.Hour), 10); return err })
}

func openTestStore(ctx context.Context, t *testing.T) (*store.Store, error) {
	t.Helper()
	return store.Open(ctx, storetest.DataDir(t))
}

func itoaNotify(n int64) string {
	s := ""
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}

// recordingEventSender is safe for concurrent Route calls: distinct delivery
// keys may POST in parallel.
type recordingEventSender struct {
	mu     sync.Mutex
	events []Event
}

func (s *recordingEventSender) Send(_ context.Context, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, Event{Message: message})
}
func (s *recordingEventSender) SendEvent(_ context.Context, event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func desktopSentColumns(t *testing.T, db *store.Store, id int64) (string, int64) {
	t.Helper()
	var payload *string
	var count *int64
	if err := db.DB().QueryRowContext(context.Background(),
		`SELECT desktop_sent_payload_json, desktop_sent_count FROM notification_intents WHERE id=?`, id).
		Scan(&payload, &count); err != nil {
		t.Fatalf("reading desktop snapshot of intent %d: %v", id, err)
	}
	if payload == nil || count == nil {
		return "", 0
	}
	return *payload, *count
}

func desktopStateColumn(t *testing.T, db *store.Store, id int64) string {
	t.Helper()
	var state string
	if err := db.DB().QueryRowContext(context.Background(),
		`SELECT desktop_state FROM notification_intents WHERE id=?`, id).Scan(&state); err != nil {
		t.Fatalf("reading desktop_state of intent %d: %v", id, err)
	}
	return state
}

// A desktop leg that is switched off must not freeze the webhook digest that
// shares its row: every later event still joins the digest.
func TestDesktopOffKeepsWebhookDigestCoalescing(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	webhook := &recordingEventSender{}
	desktop := &routerSender{}
	policy := Policy{MaxPerHour: 10, DigestEvery: 30 * time.Minute, Categories: map[Category]CategoryPolicy{
		CategoryDiscoveryNew: {Desktop: "off", Webhook: "digest", Window: time.Hour},
	}}
	router := NewRouter(RouterOptions{Ledger: NewStoreLedger(db), Desktop: desktop, Webhook: webhook, Policy: policy, Now: func() time.Time { return clock }})
	route := func(at time.Time, message, detailKey string) {
		t.Helper()
		clock = at
		intent := Intent{EventKind: "watch.alert", Category: CategoryDiscoveryNew, AggregateKey: "watch:1",
			Phase: PhaseDigest, WindowStart: now, HappenedAt: at, Message: message,
			Detail: Event{Kind: "watch.alert", Message: message, Count: 1, Detail: map[string]any{detailKey: true}}}
		if err := router.Route(ctx, intent); err != nil {
			t.Fatalf("route %q: %v", message, err)
		}
	}
	route(now, "first paper", "first")
	route(now.Add(5*time.Minute), "second paper", "second")

	clock = now.Add(2 * time.Hour)
	if err := router.RunDueAt(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events = %#v, want exactly one digest", webhook.events)
	}
	digest := webhook.events[0]
	if digest.Count != 2 {
		t.Fatalf("digest count = %d, want 2: the second event never joined the digest", digest.Count)
	}
	if digest.Detail["first"] != true || digest.Detail["second"] != true {
		t.Fatalf("digest detail = %v, want both events merged", digest.Detail)
	}
	if want := ComposeMessage(CategoryDiscoveryNew, 2, digest, digest.Message); digest.Message != want {
		t.Fatalf("digest message = %q, want %q", digest.Message, want)
	}
	if len(desktop.messages) != 0 {
		t.Fatalf("desktop sent %#v with the desktop leg switched off", desktop.messages)
	}
}

// After the desktop leg delivers, the row keeps coalescing for the pending
// webhook digest, and the delivery snapshot still names exactly what the
// desktop said.
func TestDesktopDeliveryKeepsItsAuditWhileDigestCoalesces(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := NewStoreLedger(db)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	webhook := &recordingEventSender{}
	desktop := &routerSender{}
	policy := Policy{MaxPerHour: 10, DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryDiscoveryNew: {Desktop: "immediate", Webhook: "digest", Window: time.Hour},
	}}
	now2 := func() time.Time { return clock }
	router := NewRouter(RouterOptions{Ledger: ledger, Desktop: desktop, Webhook: webhook, Policy: policy, Now: now2})
	// Both legs share one availability, so a drain that reaches the desktop leg
	// would also flush the digest. A deployment without a webhook sender drains
	// the desktop leg alone, which is the state this test needs: desktop sent,
	// digest still pending.
	desktopOnly := NewRouter(RouterOptions{Ledger: ledger, Desktop: desktop, Policy: policy, Now: now2})
	route := func(at time.Time, message, detailKey string) {
		t.Helper()
		clock = at
		intent := Intent{EventKind: "watch.alert", Category: CategoryDiscoveryNew, AggregateKey: "watch:1",
			Phase: PhaseDigest, WindowStart: now, HappenedAt: at, Message: message,
			Detail: Event{Kind: "watch.alert", Message: message, Count: 1, Detail: map[string]any{detailKey: true}}}
		if err := router.Route(ctx, intent); err != nil {
			t.Fatalf("route %q: %v", message, err)
		}
	}
	route(now, "first paper", "first")
	clock = now.Add(90 * time.Minute)
	if err := desktopOnly.RunDueAt(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if len(desktop.messages) != 1 {
		t.Fatalf("desktop messages = %#v, want the one delivered notification", desktop.messages)
	}
	sentCopy := desktop.messages[0]

	route(now.Add(100*time.Minute), "second paper", "second")
	clock = now.Add(5 * time.Hour)
	pending, err := ledger.DueWebhook(ctx, clock, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending webhook rows = %d, want 1", len(pending))
	}
	row := pending[0]
	if row.Count != 2 {
		t.Fatalf("row count = %d, want 2: the desktop delivery froze the digest", row.Count)
	}
	sentDetail, sentCount := row.DesktopSent()
	if sentCount != 1 || sentDetail.Detail["second"] == true {
		t.Fatalf("desktop audit = %d/%v, want the single event the desktop actually sent", sentCount, sentDetail.Detail)
	}
	if sentDetail.Message != sentCopy {
		t.Fatalf("desktop audit copy = %q, want the delivered copy %q", sentDetail.Message, sentCopy)
	}

	if err := router.RunDueAt(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events = %#v, want exactly one digest", webhook.events)
	}
	digest := webhook.events[0]
	if digest.Count != 2 || digest.Detail["first"] != true || digest.Detail["second"] != true {
		t.Fatalf("digest = %+v, want both events", digest)
	}
	payload, count := desktopSentColumns(t, db, row.ID)
	if count != 1 || !strings.Contains(payload, "first paper") || strings.Contains(payload, "second paper") {
		t.Fatalf("desktop snapshot = %d/%q, want only the delivered event", count, payload)
	}
}

// staleDesktopLedger replays a due-row snapshot taken before a concurrent
// supersession, which is exactly what a drain holds when the final
// notification commits between the due read and the drain's own write.
type staleDesktopLedger struct {
	*StoreLedger
	rows []Record
}

func (l *staleDesktopLedger) DueDesktop(context.Context, time.Time, int) ([]Record, error) {
	return l.rows, nil
}

func TestRunDueAtDoesNotResurrectSupersededCheckpoint(t *testing.T) {
	quiet, err := ParseQuietHours("22:00-23:30", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	// Both cases route the checkpoint under a live desktop policy, so the row
	// is pending and genuinely due. Only the drain differs: quiet hours start,
	// or the operator switches the desktop channel off.
	for _, tc := range []struct {
		name         string
		drainDesktop string
		drainQuiet   QuietHours
	}{
		{name: "quiet hours hold", drainDesktop: "immediate", drainQuiet: quiet},
		{name: "platform unavailable", drainDesktop: "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := openTestStore(ctx, t)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ledger := NewStoreLedger(db)
			// 22:15 UTC is inside the quiet window above.
			now := time.Date(2026, 7, 15, 22, 15, 0, 0, time.UTC)
			clock := now
			desktop := &routerSender{}
			routePolicy := Policy{MaxPerHour: 10, Categories: map[Category]CategoryPolicy{
				CategoryCompletionBatch: {Desktop: "immediate", Webhook: "off", Window: time.Minute},
			}}
			drainPolicy := Policy{MaxPerHour: 10, QuietHours: tc.drainQuiet, QuietMode: "hold", Categories: map[Category]CategoryPolicy{
				CategoryCompletionBatch: {Desktop: tc.drainDesktop, Webhook: "off", Window: time.Minute},
			}}
			router := NewRouter(RouterOptions{Ledger: ledger, Desktop: desktop, Policy: routePolicy, Now: func() time.Time { return clock }})
			checkpoint := Intent{EventKind: "batch.checkpoint", Category: CategoryCompletionBatch, AggregateKey: "cohort:1",
				Phase: PhaseCheckpoint, WindowStart: now, HappenedAt: now, Message: "checkpoint", Detail: Event{Kind: "batch.checkpoint", Message: "checkpoint"}}
			if err := router.Route(ctx, checkpoint); err != nil {
				t.Fatal(err)
			}
			// The drain reads its due rows, then the final notification commits
			// its supersession before the drain writes.
			due, err := ledger.DueDesktop(ctx, now, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(due) != 1 {
				t.Fatalf("due rows = %d, want the checkpoint", len(due))
			}
			checkpointID := due[0].ID
			clock = now.Add(time.Minute)
			final := Intent{EventKind: "batch.completed", Category: CategoryCompletionBatch, AggregateKey: "cohort:1",
				Phase: PhaseFinal, WindowStart: clock, HappenedAt: clock, Message: "batch done", Detail: Event{Kind: "batch.completed", Message: "batch done"}}
			if err := router.Route(ctx, final); err != nil {
				t.Fatal(err)
			}
			if got := desktopStateColumn(t, db, checkpointID); got != "superseded" {
				t.Fatalf("checkpoint desktop_state = %q after finalisation, want superseded", got)
			}

			stale := NewRouter(RouterOptions{Ledger: &staleDesktopLedger{StoreLedger: ledger, rows: due}, Desktop: desktop, Policy: drainPolicy, Now: func() time.Time { return clock }})
			if err := stale.RunDueAt(ctx, clock); err != nil {
				t.Fatal(err)
			}
			if got := desktopStateColumn(t, db, checkpointID); got != "superseded" {
				t.Fatalf("stale drain rewrote desktop_state to %q, want superseded", got)
			}
			// A later real drain must never deliver the retired checkpoint.
			clock = now.Add(12 * time.Hour)
			if err := router.RunDueAt(ctx, clock); err != nil {
				t.Fatal(err)
			}
			for _, message := range desktop.messages {
				if strings.Contains(message, "checkpoint") {
					t.Fatalf("superseded checkpoint delivered after its final notification: %#v", desktop.messages)
				}
			}
		})
	}
}

func webhookStateColumn(t *testing.T, db *store.Store, id int64) string {
	t.Helper()
	var state string
	if err := db.DB().QueryRowContext(context.Background(),
		`SELECT webhook_state FROM notification_intents WHERE id=?`, id).Scan(&state); err != nil {
		t.Fatalf("reading webhook_state of intent %d: %v", id, err)
	}
	return state
}

type resultWebhookSender struct {
	events []Event
	calls  int
	err    error
}

func (s *resultWebhookSender) Send(_ context.Context, message string) {
	s.calls++
	s.events = append(s.events, Event{Message: message})
}
func (s *resultWebhookSender) SendEvent(_ context.Context, event Event) {
	s.calls++
	s.events = append(s.events, event)
}
func (s *resultWebhookSender) SendEventResult(_ context.Context, event Event) error {
	s.calls++
	s.events = append(s.events, event)
	return s.err
}

func immediateTestPolicy() Policy {
	return Policy{MaxPerHour: 10, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "immediate", Window: time.Minute},
	}}
}

func immediateTestIntent(now time.Time) Intent {
	return immediateTestIntentAt(now, now)
}

func immediateTestIntentAt(window, happened time.Time) Intent {
	return Intent{EventKind: "request.outcome", Category: CategoryRequestOutcome, AggregateKey: "job:1",
		Phase: PhaseTerminal, WindowStart: window, HappenedAt: happened,
		Message: "Request finished — open the papio inbox",
		Detail:  Event{Kind: "request.outcome", Message: "Request finished — open the papio inbox", Count: 1}}
}

// A replayed immediate intent is the same event twice: the generation upsert
// returns the in-flight generation without queueing a new one, and the lost
// claim skips delivery, so exactly one POST leaves the row. The replay runs
// while the first generation is still sending; a distinct second decision
// with an identical payload but a later HappenedAt is a new event and opens
// a new generation (see TestIdenticalPayloadAfterClaimOpensNewGeneration).
func TestImmediateWebhookReplayPostsOnce(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := NewStoreLedger(db)
	webhook := &blockingEventSender{ch: make(chan Event, 8), release: make(chan struct{}, 8)}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	intent := immediateTestIntent(now)
	done := make(chan error, 1)
	go func() { done <- router.Route(ctx, intent) }()
	// Wait for the winner's claim snapshot to be in flight, then replay
	// the identical intent while the webhook leg is still sending.
	first := <-webhook.ch
	if err := router.Route(ctx, intent); err != nil {
		t.Fatal(err)
	}
	webhook.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := countIntentRows(t, db); n != 1 {
		t.Fatalf("ledger rows = %d, want the single in-flight generation", n)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events = %d, want exactly one POST for a replayed intent", len(webhook.events))
	}
	if got := webhook.events[0].DeliveryKey; got == "" || got != first.DeliveryKey {
		t.Fatalf("delivery key = %q, want the stable key %q", webhook.events[0].DeliveryKey, first.DeliveryKey)
	}
	rows, err := NewStoreLedger(db).DueWebhook(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("replayed leg is still due: %+v", rows)
	}
}

// A producer retry after the first generation settles is the exact same
// event: the generation upsert returns the settled row, the lost claim
// skips delivery, and the key POSTs once across both Routes.
func TestImmediateWebhookSettledRetryPostsOnce(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := NewStoreLedger(db)
	webhook := &recordingEventSender{}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	intent := immediateTestIntent(now)
	if err := router.Route(ctx, intent); err != nil {
		t.Fatal(err)
	}
	// Producer crashes before its marker and retries the identical
	// intent: same identity, HappenedAt, count, and payload.
	if err := router.Route(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events = %d, want exactly one POST for a settled retry", len(webhook.events))
	}
	if got := webhook.events[0].DeliveryKey; got == "" {
		t.Fatal("settled POST carries no stable delivery key")
	}
	if n := countIntentRows(t, db); n != 1 {
		t.Fatalf("ledger rows = %d, want the single settled generation", n)
	}
}

// A distinct second decision with an identical payload is a new event, not
// a replay: it carries a later HappenedAt, so the generation upsert must
// queue a fresh generation with its own key instead of suppressing it.
func TestIdenticalPayloadAfterClaimOpensNewGeneration(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := NewStoreLedger(db)
	webhook := &recordingEventSender{}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	intent := immediateTestIntent(now)
	if err := router.Route(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events = %d, want the first generation", len(webhook.events))
	}
	// Same payload bytes and count as the settled generation, but a later
	// HappenedAt: a distinct second decision the ledger must not drop.
	second := immediateTestIntent(now.Add(time.Minute))
	if err := router.Route(ctx, second); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 2 {
		t.Fatalf("webhook events = %d, want the settled generation plus the distinct second decision", len(webhook.events))
	}
	first, next := webhook.events[0], webhook.events[1]
	if first.DeliveryKey == "" || next.DeliveryKey == "" || first.DeliveryKey == next.DeliveryKey {
		t.Fatalf("delivery keys = %q/%q, want two distinct generation keys", first.DeliveryKey, next.DeliveryKey)
	}
	if total := first.Count + next.Count; total != 2 {
		t.Fatalf("total delivered = %d, want 2 across both generations", total)
	}
	if n := countIntentRows(t, db); n != 2 {
		t.Fatalf("ledger rows = %d, want two generations", n)
	}
}

func countIntentRows(t *testing.T, db *store.Store) int {
	t.Helper()
	var n int
	if err := db.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notification_intents`).Scan(&n); err != nil {
		t.Fatalf("counting notification rows: %v", err)
	}
	return n
}

// blockingEventSender holds each POST open until the test releases it, so
// the test can Route a replay while the first generation is still sending.
type blockingEventSender struct {
	ch      chan Event
	release chan struct{}
	events  []Event
}

func (s *blockingEventSender) Send(_ context.Context, _ string) {}

func (s *blockingEventSender) SendEventResult(_ context.Context, event Event) error {
	s.events = append(s.events, event)
	s.ch <- event
	<-s.release
	return nil
}

// Concurrent immediate Routes are competing producers, so each Route is one
// new event: the first claims the identity generation, and every later
// Route that finds the identity claimed or settled queues a fresh
// generation with its own key. Each key POSTs exactly once and the counts
// across keys total the eight concurrent events.
func TestImmediateWebhookConcurrentRoutesPostOnce(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	webhook := &recordingEventSender{}
	router := NewRouter(RouterOptions{Ledger: NewStoreLedger(db), Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Distinct WindowStart per producer keeps the eight events in
			// eight generations: the replay rule matches only the exact
			// same event, so distinct starts never collapse into one.
			_ = router.Route(ctx, immediateTestIntentAt(now.Add(time.Duration(i)*time.Microsecond), now.Add(time.Duration(i)*time.Microsecond)))
		}()
	}
	wg.Wait()
	if len(webhook.events) == 0 {
		t.Fatal("webhook events = 0, want coverage for eight concurrent Routes")
	}
	seen := map[string]int{}
	total := 0
	for i, event := range webhook.events {
		if event.DeliveryKey == "" {
			t.Fatalf("webhook event %d carries no stable delivery key", i)
		}
		seen[event.DeliveryKey]++
		total += event.Count
	}
	for key, posts := range seen {
		if posts != 1 {
			t.Fatalf("delivery key %q POSTed %d times, want exactly once per key", key, posts)
		}
	}
	if total != 8 {
		t.Fatalf("total delivered = %d, want 8: every concurrent Route's event exactly once", total)
	}
}

// A digest claim held across a restart never becomes due again, so the next
// drain cannot duplicate the POST. The held sending row is the at-most-once
// boundary, not a retry.
func TestDigestWebhookClaimPreventsRestartDuplicate(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	webhook := &recordingEventSender{}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	ledger := NewStoreLedger(db)
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Now: func() time.Time { return clock }})
	if err := router.Route(ctx, immediateTestIntent(now)); err != nil {
		t.Fatal(err)
	}
	due, err := ledger.DueWebhook(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("pending digest rows = %d, want one", len(due))
	}
	// Simulate a crash after the claim but before the POST settled.
	_, claimed, err := ledger.ClaimWebhook(ctx, due[0].ID, now.Add(time.Hour))
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v, want true", claimed, err)
	}
	clock = now.Add(time.Hour)
	if err := router.RunDueAt(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 0 {
		t.Fatalf("webhook events after a crashed claim = %d, want no duplicate POST", len(webhook.events))
	}
	if got := webhookStateColumn(t, db, due[0].ID); got != "sending" {
		t.Fatalf("webhook_state after crashed claim = %q, want sending held for at-most-once", got)
	}
}

// A normal digest delivers once with a stable key and never replays.
func TestDigestWebhookDeliversOnceWithStableKey(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	webhook := &recordingEventSender{}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: NewStoreLedger(db), Webhook: webhook, Policy: policy, Now: func() time.Time { return clock }})
	if err := router.Route(ctx, immediateTestIntent(now)); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(time.Hour)
	if err := router.RunDueAt(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events = %d, want one digest POST", len(webhook.events))
	}
	key := webhook.events[0].DeliveryKey
	if key == "" {
		t.Fatal("digest POST carries no stable delivery key")
	}
	if err := router.RunDueAt(ctx, clock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events after second drain = %d, want no replay", len(webhook.events))
	}
}

// A refused dial never reaches the endpoint, so it requeues the row as
// pending instead of settling failed, and the failure reaches the caller:
// producers that retry from durable state (the watch alert path) must not
// record the alert as delivered. A response failure after the request went
// out (an EOF mid-body) may mean the receiver processed it, so it settles
// terminal failed instead.
func TestImmediateWebhookDialRefusedRequeuesPendingAndSurfacesError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	dialRefused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	webhook := &resultWebhookSender{err: dialRefused}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	if err := router.Route(ctx, immediateTestIntent(now)); err == nil {
		t.Fatal("Route = nil error, want the webhook delivery failure")
	} else if !strings.Contains(err.Error(), "webhook") {
		t.Fatalf("Route error = %q, want it to name the webhook leg", err)
	}
	if webhook.calls != 1 {
		t.Fatalf("immediate delivery calls = %d, want one attempt", webhook.calls)
	}
	for _, row := range ledger.rows {
		if row.WebhookState != "pending" {
			t.Fatalf("webhook_state = %q, want pending so the drain retries", row.WebhookState)
		}
		if !row.WebhookAttemptedAt.Equal(now.Add(webhookRetryBackoff)) {
			t.Fatalf("retry time = %s, want the retry backoff %s", row.WebhookAttemptedAt, now.Add(webhookRetryBackoff))
		}
		if !row.AvailableAt.Equal(now) {
			t.Fatalf("available_at = %s, want the desktop leg's %s kept", row.AvailableAt, now)
		}
	}
}

// Mid-body response failures are ambiguous: an EOF or reset after the request
// went out may mean the receiver processed it, so the leg settles terminal
// failed instead of retrying under the same key.
func TestImmediateWebhookMidBodyEOFSettlesFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	midBodyEOF := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("unexpected EOF")}
	webhook := &resultWebhookSender{err: midBodyEOF}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	if err := router.Route(ctx, immediateTestIntent(now)); err == nil {
		t.Fatal("Route = nil error, want the webhook delivery failure")
	}
	for _, row := range ledger.rows {
		if row.WebhookState != "failed" {
			t.Fatalf("webhook_state = %q, want failed for an ambiguous response failure", row.WebhookState)
		}
	}
}

// The drain redelivers a requeued immediate row once its backoff passes: an
// immediate category such as a request outcome has no producer retry of its
// own, so leaving the row to Route would strand it. A byte-identical
// producer Route inside the backoff window does not POST under the deferred
// key: the retry returns the deferred row without redelivering, and Route
// reports the deferral so the caller retries instead of marking the alert
// delivered.
func TestDigestDrainRetriesRequeuedImmediateRow(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	dialRefused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	webhook := &resultWebhookSender{err: dialRefused}
	ledger := NewStoreLedger(db)
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	if err := router.Route(ctx, immediateTestIntent(now)); err == nil {
		t.Fatal("Route = nil error, want the webhook delivery failure")
	}
	if webhook.calls != 1 {
		t.Fatalf("immediate delivery calls = %d, want one attempt", webhook.calls)
	}
	var id int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM notification_intents LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := webhookStateColumn(t, db, id); got != "pending" {
		t.Fatalf("webhook_state = %q, want pending so the drain retries", got)
	}
	// A byte-identical producer retry inside the backoff window does not
	// POST: it is the router's own retry of the same event, so it returns
	// the deferred row without redelivering and Route skips the immediate
	// dispatch for the not-due row, reporting the deferral so the caller
	// retries instead of marking the alert delivered.
	if err := router.Route(ctx, immediateTestIntent(now)); !deliveryFailed(err) {
		t.Fatalf("retry Route inside the backoff = %v, want the deferred delivery failure, not nil", err)
	}
	if n := countIntentRows(t, db); n != 1 {
		t.Fatalf("ledger rows = %d, want the single deferred row", n)
	}
	if webhook.calls != 1 {
		t.Fatalf("immediate delivery calls = %d, want only the first attempt", webhook.calls)
	}
	// Once the backoff passes the drain POSTs the deferred row under its
	// stable key. The endpoint recovers.
	webhook.err = nil
	clock := now.Add(webhookRetryBackoff + time.Minute)
	if err := router.RunDueAt(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 2 {
		t.Fatalf("webhook events = %d, want the failed attempt plus the drain retry", len(webhook.events))
	}
	if got := webhook.events[1].DeliveryKey; got == "" || got != WebhookDeliveryKey(id) {
		t.Fatalf("retry delivery key = %q, want the stable key %q", got, WebhookDeliveryKey(id))
	}
	if got := webhookStateColumn(t, db, id); got != "attempted" {
		t.Fatalf("webhook_state = %q, want attempted after the drain retry", got)
	}
}

// A row still undelivered a day after its first failure settles failed, so an
// outage that never clears stays visible instead of retrying forever.
func TestDigestWebhookRetryWindowExpiresToFailed(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	webhook := &resultWebhookSender{err: &WebhookStatusError{StatusCode: 503, Status: "503 Service Unavailable"}}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	ledger := NewStoreLedger(db)
	recorder := &recordingActivity{}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Activity: recorder, Now: func() time.Time { return clock }})
	if err := router.Route(ctx, immediateTestIntent(now)); err != nil {
		t.Fatal(err)
	}
	drain := func() {
		t.Helper()
		if err := router.RunDueAt(ctx, clock); !deliveryFailed(err) {
			t.Fatalf("RunDueAt = %v, want the recorded delivery failure", err)
		}
	}
	clock = now.Add(time.Hour)
	drain()
	if len(recorder.failures) != 1 || recorder.failures[0].cause == "" {
		t.Fatalf("audit failures = %+v, want the failed POST recorded with its cause", recorder.failures)
	}
	// The endpoint stays down past the retry window: the next due drain
	// settles the row failed with the cause audited.
	clock = now.Add(webhookRetryExpiry + 2*time.Hour)
	drain()
	var id int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM notification_intents LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := webhookStateColumn(t, db, id); got != "failed" {
		t.Fatalf("webhook_state after the window = %q, want failed so the outage stays visible", got)
	}
	if last := recorder.failures[len(recorder.failures)-1]; !strings.Contains(last.cause, "retry window expired") {
		t.Fatalf("audit cause = %q, want the expiry named", last.cause)
	}
	due, err := ledger.DueWebhook(ctx, clock.Add(24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due rows after the window = %d, want none retried", len(due))
	}
	if webhook.calls != 2 {
		t.Fatalf("digest delivery calls = %d, want the first attempt plus the expiry attempt", webhook.calls)
	}
}

// An endpoint answer that rejects the payload settles the immediate leg as
// failed instead of requeuing: the receiver has the bytes, so a retry would
// resend them. The failure still reaches the caller.
func TestImmediateWebhookClientErrorSettlesFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	webhook := &resultWebhookSender{err: &WebhookStatusError{StatusCode: 400, Status: "400 Bad Request"}}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	if err := router.Route(ctx, immediateTestIntent(now)); err == nil {
		t.Fatal("Route = nil error, want the rejected POST surfaced")
	}
	for _, row := range ledger.rows {
		if row.WebhookState != "failed" {
			t.Fatalf("webhook_state = %q, want failed for a rejected payload", row.WebhookState)
		}
	}
	due, err := ledger.DueWebhook(ctx, now.Add(24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due rows after a rejection = %d, want none retried", len(due))
	}
}

// A 503 digest POST requeues the row with a backoff and the next drain sends
// exactly one POST under the same delivery key. The digest is never marked
// failed while the outage may not have delivered anything.
func TestDigestWebhookRetryableFailureRetriesUnderSameKey(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	webhook := &resultWebhookSender{err: &WebhookStatusError{StatusCode: 503, Status: "503 Service Unavailable"}}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	ledger := NewStoreLedger(db)
	recorder := &recordingActivity{}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Activity: recorder, Now: func() time.Time { return clock }})
	if err := router.Route(ctx, immediateTestIntent(now)); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(time.Hour)
	if err := router.RunDueAt(ctx, clock); !deliveryFailed(err) {
		t.Fatalf("RunDueAt = %v, want the recorded delivery failure", err)
	}
	if webhook.calls != 1 {
		t.Fatalf("digest delivery calls = %d, want one attempt", webhook.calls)
	}
	var id int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM notification_intents LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := webhookStateColumn(t, db, id); got != "pending" {
		t.Fatalf("digest webhook_state = %q, want pending so the drain retries", got)
	}
	var retryAt string
	if err := db.DB().QueryRowContext(ctx, `SELECT webhook_attempted_at FROM notification_intents WHERE id=?`, id).Scan(&retryAt); err != nil {
		t.Fatal(err)
	}
	if retryAt != store.FormatTime(clock.Add(webhookRetryBackoff)) {
		t.Fatalf("retry time = %q, want the retry backoff %q", retryAt, store.FormatTime(clock.Add(webhookRetryBackoff)))
	}
	due, err := ledger.DueWebhook(ctx, clock, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("due rows inside the backoff = %d, want none", len(due))
	}
	if len(recorder.failures) != 1 || recorder.failures[0].cause == "" {
		t.Fatalf("audit failures = %+v, want the failed POST recorded with its cause", recorder.failures)
	}
	// After the backoff the endpoint recovers: the same row delivers once
	// under the same key and settles attempted.
	key := webhook.events[0].DeliveryKey
	webhook.err = nil
	clock = clock.Add(webhookRetryBackoff + time.Minute)
	if err := router.RunDueAt(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 2 {
		t.Fatalf("webhook events = %d, want the failed attempt plus the retry", len(webhook.events))
	}
	if got := webhook.events[1].DeliveryKey; got == "" || got != key {
		t.Fatalf("retry delivery key = %q, want the stable key %q", got, key)
	}
	if got := webhookStateColumn(t, db, id); got != "attempted" {
		t.Fatalf("digest webhook_state = %q, want attempted after the retry", got)
	}
	if err := router.RunDueAt(ctx, clock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 2 {
		t.Fatalf("webhook events after second drain = %d, want no replay", len(webhook.events))
	}
}

// A drain cancelled between claim and POST must not strand the row in
// sending: the cancellation proves nothing went out, so the claim reverts to
// pending and the next live drain sends exactly one POST.
func TestRunDueAtCancelledDrainRevertsClaim(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	webhook := &recordingEventSender{}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	ledger := NewStoreLedger(db)
	drain, cancel := context.WithCancel(ctx)
	cancelling := &cancelOnClaimLedger{StoreLedger: ledger, cancel: cancel}
	router := NewRouter(RouterOptions{Ledger: cancelling, Webhook: webhook, Policy: policy, Now: func() time.Time { return clock }})
	if err := router.Route(ctx, immediateTestIntent(now)); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(time.Hour)
	if err := router.RunDueAt(drain, clock); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunDueAt on a cancelled drain = %v, want context.Canceled", err)
	}
	if len(webhook.events) != 0 {
		t.Fatalf("webhook events on a cancelled drain = %d, want no POST", len(webhook.events))
	}
	var id int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM notification_intents LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := webhookStateColumn(t, db, id); got != "pending" {
		t.Fatalf("webhook_state after a cancelled drain = %q, want pending so the row resends", got)
	}
	if err := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Now: func() time.Time { return clock }}).RunDueAt(ctx, clock); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events after recovery = %d, want exactly one POST", len(webhook.events))
	}
	if got := webhookStateColumn(t, db, id); got != "attempted" {
		t.Fatalf("webhook_state after recovery = %q, want attempted", got)
	}
}

// cancelOnClaimLedger cancels the drain context from inside ClaimWebhook, so
// the cancellation lands exactly between claim and POST: the daemon-stop
// interleaving a graceful shutdown can hit every pass.
type cancelOnClaimLedger struct {
	*StoreLedger
	cancel context.CancelFunc
	once   bool
}

func (l *cancelOnClaimLedger) ClaimWebhook(ctx context.Context, id int64, now time.Time) (Record, bool, error) {
	row, claimed, err := l.StoreLedger.ClaimWebhook(ctx, id, now)
	if claimed && !l.once {
		l.once = true
		l.cancel()
	}
	return row, claimed, err
}

// A drain that starts cancelled never claims: the due row stays pending for
// the next live drain instead of settling failed for a POST that never ran.
func TestRunDueAtAlreadyCancelledDrainNeverClaims(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	ledger := newRouterLedger()
	webhook := &recordingEventSender{}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Now: func() time.Time { return clock }})
	if err := router.Route(ctx, immediateTestIntent(now)); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	clock = now.Add(time.Hour)
	if err := router.RunDueAt(cancelled, clock); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunDueAt on a cancelled context = %v, want context.Canceled", err)
	}
	if len(webhook.events) != 0 {
		t.Fatalf("webhook events on a cancelled drain = %d, want no POST", len(webhook.events))
	}
	for _, row := range ledger.rows {
		if row.WebhookState != "pending" {
			t.Fatalf("webhook_state after a cancelled drain = %q, want pending so the row resends", row.WebhookState)
		}
	}
}

// A concurrent Route can coalesce another event into the same decision_opened
// row after the winner's Upsert returns but before its webhook claim wins.
// The winner must send the claimed row's authoritative count, not the stale
// Upsert snapshot, because the loser of the claim sends nothing.
type mergeBeforeClaimLedger struct {
	*routerLedger
	merge  func(id int64)
	merged bool
}

func (l *mergeBeforeClaimLedger) ClaimWebhook(ctx context.Context, id int64, now time.Time) (Record, bool, error) {
	if !l.merged && l.merge != nil {
		l.merged = true
		l.merge(id)
	}
	return l.routerLedger.ClaimWebhook(ctx, id, now)
}

func TestImmediateWebhookSendsClaimedCountAfterConcurrentMerge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	window := now.Add(-5 * time.Minute)
	base := newRouterLedger()
	ledger := &mergeBeforeClaimLedger{routerLedger: base}
	webhook := &recordingEventSender{}
	policy := Policy{MaxPerHour: 10, Categories: map[Category]CategoryPolicy{
		CategoryDecisionOpened: {Desktop: "off", Webhook: "immediate", Window: 5 * time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Now: func() time.Time { return now }})
	intent := Intent{EventKind: "action.opened", Category: CategoryDecisionOpened, AggregateKey: "decision:interleave",
		Phase: PhaseOpened, WindowStart: window, HappenedAt: now,
		Message: "1 paper needs you — open the papio inbox",
		Detail:  Event{Kind: "action.opened", Message: "1 paper needs you — open the papio inbox", Count: 1}}
	// The losing concurrent Route merges its event after the winner's Upsert
	// snapshot but before the winner's claim.
	ledger.merge = func(id int64) {
		r := base.rows[id]
		r.Count++
		detail := r.Intent.Detail
		detail.Count = r.Count
		r.Intent.Detail = detail
		base.rows[id] = r
	}
	if err := router.Route(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events = %d, want one POST covering both merged intents", len(webhook.events))
	}
	if got := webhook.events[0].Count; got != 2 {
		t.Fatalf("webhook count = %d, want 2 from the claimed row, not the stale Upsert snapshot of 1", got)
	}
	if got, want := webhook.events[0].Message, "2 papers need you — open the papio inbox"; got != want {
		t.Fatalf("webhook message = %q, want %q", got, want)
	}
}

// A crash between the reminder Route and its markers retries the identical
// absolute total. The pending digest must replace the count, not add it,
// so the retry stays at 2 instead of inflating to 4.
func TestPendingDigestRetryReplacesInsteadOfAdding(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	policy := Policy{MaxPerHour: 10, Categories: map[Category]CategoryPolicy{
		CategoryDecisionPending: {Desktop: "digest", Webhook: "off", Window: 4 * time.Hour},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Policy: policy, Now: func() time.Time { return now }})
	intent := Intent{EventKind: "action.reminder", Category: CategoryDecisionPending, AggregateKey: "actions:pending",
		Phase: PhaseReminder, WindowStart: now, HappenedAt: now,
		Message: "reminder", Detail: Event{Kind: "action.reminder", Message: "reminder", Count: 2}}
	if err := router.Route(ctx, intent); err != nil {
		t.Fatal(err)
	}
	// Crash before markers: the next pass sees the same due set and routes
	// the identical snapshot again.
	if err := router.Route(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if len(ledger.rows) != 1 {
		t.Fatalf("ledger rows = %d, want one snapshot row", len(ledger.rows))
	}
	for _, row := range ledger.rows {
		if row.Count != 2 {
			t.Fatalf("pending digest count = %d, want 2 after an identical retry, not 4", row.Count)
		}
	}
}

// The production snapshot path replaces through the store: two identical
// Routes keep the row at the absolute total.
func TestStorePendingDigestRetryKeepsAbsoluteTotal(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := NewStoreLedger(db)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	rec := Record{Intent: Intent{EventKind: "action.reminder", Category: CategoryDecisionPending, AggregateKey: "actions:pending",
		Phase: PhaseReminder, WindowStart: now, HappenedAt: now, Message: "reminder",
		Detail: Event{Kind: "action.reminder", Message: "reminder", Count: 2}},
		FirstAt: now, LastAt: now, AvailableAt: now, Count: 2, DesktopState: "pending", WebhookState: "pending"}
	if _, err := ledger.UpsertPendingDigest(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.UpsertPendingDigest(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rows, err := ledger.DueDesktop(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("pending digest rows = %d, want one snapshot row", len(rows))
	}
	if rows[0].Count != 2 {
		t.Fatalf("pending digest count = %d, want the absolute total 2, not 4", rows[0].Count)
	}
}

// Reminder growth after the webhook leg claimed its snapshot must queue a new
// webhook generation with its own delivery key, never rewrite the claimed row,
// and leave exactly one desktop digest carrying the grown total.
func TestStorePendingDigestGrowthAfterWebhookClaimOpensNewGeneration(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger := NewStoreLedger(db)
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	reminder := func(count int, at time.Time) Record {
		return Record{Intent: Intent{EventKind: "action.reminder", Category: CategoryDecisionPending, AggregateKey: "actions:pending",
			Phase: PhaseReminder, WindowStart: now, HappenedAt: at, Message: "reminder",
			Detail: Event{Kind: "action.reminder", Message: "reminder", Count: count}},
			FirstAt: at, LastAt: at, AvailableAt: now.Add(time.Minute), Count: count, DesktopState: "pending", WebhookState: "pending"}
	}
	first, err := ledger.UpsertPendingDigest(ctx, reminder(2, now))
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := ledger.ClaimWebhook(ctx, first.ID, now); err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if err := ledger.SetWebhookState(ctx, first.ID, "attempted", now); err != nil {
		t.Fatal(err)
	}
	grown, err := ledger.UpsertPendingDigest(ctx, reminder(3, now.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if grown.ID == first.ID || grown.WebhookState != "pending" || grown.Count != 3 {
		t.Fatalf("grown digest = id %d webhook %q count %d, want a new pending generation with count 3 (claimed id %d)",
			grown.ID, grown.WebhookState, grown.Count, first.ID)
	}
	claimedRow, err := ledger.GetByID(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimedRow.Count != 2 {
		t.Fatalf("claimed snapshot count = %d, want immutable 2", claimedRow.Count)
	}
	again, err := ledger.UpsertPendingDigest(ctx, reminder(3, now.Add(2*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != grown.ID {
		t.Fatalf("identical total opened generation %d, want it to stay on %d", again.ID, grown.ID)
	}
	rows, err := ledger.DueDesktop(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Count != 3 {
		t.Fatalf("due desktop digests = %+v, want one row with count 3", rows)
	}
}

// A Route that lands after the winner's webhook claim must not rewrite the
// claimed snapshot and must not be lost: the fake ledger mirrors the store
// redirect below by queueing the late event as a fresh generation row with
// its own key. The winner POSTs only the immutable claim snapshot; the new
// generation drains separately with a distinct key, and the two counts sum
// to the full total.
type postClaimRedirectLedger struct {
	*routerLedger
	redirected *Record
}

func (l *postClaimRedirectLedger) UpsertWebhookGeneration(ctx context.Context, r Record) (Record, error) {
	rec, err := l.routerLedger.UpsertWebhookGeneration(ctx, r)
	if err != nil {
		return Record{}, err
	}
	if len(l.rows) > 1 {
		for id, old := range l.rows {
			if id == rec.ID {
				redirected := old
				l.redirected = &redirected
				break
			}
		}
	}
	return rec, nil
}

type mergeAfterClaimSender struct {
	events []Event
}

func (s *mergeAfterClaimSender) Send(_ context.Context, _ string) {}

func (s *mergeAfterClaimSender) SendEventResult(_ context.Context, event Event) error {
	s.events = append(s.events, event)
	return nil
}

func TestImmediateWebhookPostClaimMergeQueuesNewGeneration(t *testing.T) {
	// Deterministic by construction: drive the winner, then drive the loser
	// after the winner settled, and assert the generation upsert redirected
	// the late event instead of rewriting the claimed snapshot.
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	window := now.Add(-5 * time.Minute)
	ledger := &postClaimRedirectLedger{routerLedger: newRouterLedger()}
	sender := &mergeAfterClaimSender{}
	// Quiet-like preset: the desktop leg stays pending so only the webhook
	// claim makes the row immutable, while the webhook is immediate.
	policy := Policy{MaxPerHour: 10, Categories: map[Category]CategoryPolicy{
		CategoryDecisionOpened: {Desktop: "digest", Webhook: "immediate", Window: 5 * time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: sender, Policy: policy, Now: func() time.Time { return now }})
	intent := Intent{EventKind: "action.opened", Category: CategoryDecisionOpened, AggregateKey: "decision:postclaim",
		Phase: PhaseOpened, WindowStart: window, HappenedAt: now,
		Message: "1 paper needs you — open the papio inbox",
		Detail:  Event{Kind: "action.opened", Message: "1 paper needs you — open the papio inbox", Count: 1}}
	if err := router.Route(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if len(sender.events) != 1 || sender.events[0].Count != 1 {
		t.Fatalf("winner events = %#v, want the single immutable claim snapshot with count 1", sender.events)
	}
	// The loser lands after the winner settled: same identity, same event.
	// The generation upsert must redirect it to a new generation row with
	// its own key instead of rewriting the claimed snapshot.
	secondIntent := intent
	secondIntent.HappenedAt = now.Add(time.Minute)
	if err := router.Route(ctx, secondIntent); err != nil {
		t.Fatal(err)
	}
	if ledger.redirected == nil {
		t.Fatal("post-claim merge did not redirect to a new generation row")
	}
	if len(sender.events) != 2 {
		t.Fatalf("webhook events = %d, want the claim snapshot plus the redirected generation", len(sender.events))
	}
	first, second := sender.events[0], sender.events[1]
	if first.Count != 1 || second.Count != 1 {
		t.Fatalf("webhook counts = %d/%d, want 1/1: immutable snapshot plus one new generation", first.Count, second.Count)
	}
	if first.DeliveryKey == "" || second.DeliveryKey == "" {
		t.Fatal("both POSTs must carry stable delivery keys")
	}
	if first.DeliveryKey == second.DeliveryKey {
		t.Fatalf("delivery keys = %q twice, want distinct keys per generation", first.DeliveryKey)
	}
	if total := first.Count + second.Count; total != 2 {
		t.Fatalf("total delivered = %d, want 2 across both generations", total)
	}
	if got, want := second.Message, "1 paper needs you — open the papio inbox"; got != want {
		t.Fatalf("generation message = %q, want %q", got, want)
	}
}

// End-to-end post-claim timing on decision_opened: the first Route claims and
// POSTs the immutable snapshot while the desktop leg stays pending, a second
// Route merges after the claim and must not rewrite the claimed payload, and
// the late event drains under its own delivery key. Both keys POST exactly
// once and the counts total the full set.
func TestDecisionOpenedPostClaimMergeDeliversTwoKeys(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	webhook := &recordingEventSender{}
	ledger := NewStoreLedger(db)
	policy := Policy{MaxPerHour: 10, Categories: map[Category]CategoryPolicy{
		CategoryDecisionOpened: {Desktop: "digest", Webhook: "immediate", Window: 5 * time.Minute},
	}}
	window := now.Add(-5 * time.Minute)
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Now: func() time.Time { return now }})
	first := Intent{EventKind: "action.opened", Category: CategoryDecisionOpened, AggregateKey: "decision:two-keys",
		Phase: PhaseOpened, WindowStart: window, HappenedAt: now,
		Message: "1 paper needs you — open the papio inbox",
		Detail:  Event{Kind: "action.opened", Message: "1 paper needs you — open the papio inbox", Count: 1}}
	if err := router.Route(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.HappenedAt = now.Add(time.Minute)
	if err := router.Route(ctx, second); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 2 {
		t.Fatalf("webhook events = %d, want the claim snapshot plus the redirected generation", len(webhook.events))
	}
	seen := map[string]int{}
	total := 0
	for i, event := range webhook.events {
		if event.DeliveryKey == "" {
			t.Fatalf("webhook event %d carries no stable delivery key", i)
		}
		seen[event.DeliveryKey]++
		total += event.Count
	}
	for key, posts := range seen {
		if posts != 1 {
			t.Fatalf("delivery key %q POSTed %d times, want exactly once per key", key, posts)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("delivery keys = %d, want two generations with distinct keys", len(seen))
	}
	if total != 2 {
		t.Fatalf("total delivered = %d, want 2 across both generations", total)
	}
}

// A second Route after the first generation settled opens a new generation
// under its own key; the two keys total both events exactly once.
func TestImmediateWebhookSecondRouteOpensNewGeneration(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	window := now.Add(-5 * time.Minute)
	ledger := newRouterLedger()
	webhook := &recordingEventSender{}
	policy := Policy{MaxPerHour: 10, Categories: map[Category]CategoryPolicy{
		CategoryDecisionOpened: {Desktop: "off", Webhook: "immediate", Window: 5 * time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Now: func() time.Time { return now }})
	intent := Intent{EventKind: "action.opened", Category: CategoryDecisionOpened, AggregateKey: "decision:preclaim",
		Phase: PhaseOpened, WindowStart: window, HappenedAt: now,
		Message: "1 paper needs you — open the papio inbox",
		Detail:  Event{Kind: "action.opened", Message: "1 paper needs you — open the papio inbox", Count: 1}}
	if err := router.Route(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 1 {
		t.Fatalf("webhook events = %d, want the single claim snapshot", len(webhook.events))
	}
	key := webhook.events[0].DeliveryKey
	if key == "" {
		t.Fatal("claim snapshot carries no stable delivery key")
	}
	// The claim settled the first generation, so the second Route opens a
	// new generation under its own key; the two keys total both events.
	second := intent
	second.HappenedAt = now.Add(time.Minute)
	if err := router.Route(ctx, second); err != nil {
		t.Fatal(err)
	}
	if len(webhook.events) != 2 {
		t.Fatalf("webhook events = %d, want the claim plus the next generation", len(webhook.events))
	}
	if webhook.events[1].DeliveryKey == "" || webhook.events[1].DeliveryKey == key {
		t.Fatalf("second key = %q, want a distinct generation key after the claim settled", webhook.events[1].DeliveryKey)
	}
	if total := webhook.events[0].Count + webhook.events[1].Count; total != 2 {
		t.Fatalf("total delivered = %d, want 2 across both generations", total)
	}
}

// slowWebhookSender accepts every POST after a delay, the shape of a slow
// endpoint that answers just inside the sender's own timeout.
type slowWebhookSender struct {
	delay time.Duration
	calls int
}

func (s *slowWebhookSender) Send(context.Context, string) {}
func (s *slowWebhookSender) SendEventResult(context.Context, Event) error {
	s.calls++
	time.Sleep(s.delay)
	return nil
}

// The settle deadline starts after the POST returns: a send slower than the
// settle timeout still records the delivery instead of stranding the row in
// sending.
func TestSlowWebhookSendStillSettlesAttempted(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	previous := webhookSettleTimeout
	// The bound must comfortably cover one SQLite settle write on a loaded CI
	// runner (50 ms did not on macOS); the send outlasts it by a whole bound,
	// so a deadline started before the POST still expires mid-send.
	webhookSettleTimeout = 300 * time.Millisecond
	defer func() { webhookSettleTimeout = previous }()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	webhook := &slowWebhookSender{delay: 2 * webhookSettleTimeout}
	router := NewRouter(RouterOptions{Ledger: NewStoreLedger(db), Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	if err := router.Route(ctx, immediateTestIntent(now)); err != nil {
		t.Fatalf("Route after a slow accepted POST = %v, want nil", err)
	}
	var id int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM notification_intents LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := webhookStateColumn(t, db, id); got != "attempted" {
		t.Fatalf("webhook_state after a slow accepted POST = %q, want attempted", got)
	}
}

// deferBeforeClaimLedger lets the drain claim and defer the row between the
// Route's due snapshot and its own claim: the interleaving where a stale
// snapshot could POST inside the backoff.
type deferBeforeClaimLedger struct {
	*StoreLedger
	now  time.Time
	done bool
}

func (l *deferBeforeClaimLedger) ClaimWebhook(ctx context.Context, id int64, now time.Time) (Record, bool, error) {
	if !l.done {
		l.done = true
		if _, claimed, err := l.StoreLedger.ClaimWebhook(ctx, id, l.now); err != nil || !claimed {
			return Record{}, false, fmt.Errorf("drain claim = %v: %w", claimed, err)
		}
		if deferred, err := l.DeferWebhook(ctx, id, l.now, l.now.Add(webhookRetryBackoff), webhookRetryExpiry); err != nil || !deferred {
			return Record{}, false, fmt.Errorf("drain defer = %v: %w", deferred, err)
		}
	}
	return l.StoreLedger.ClaimWebhook(ctx, id, now)
}

// A Route holding a due snapshot cannot claim a row the drain deferred after
// that snapshot was read: the claim checks the retry time atomically, so no
// POST goes out inside the backoff, and the lost claim reports a delivery
// failure instead of success so a producer records no receipt.
func TestImmediateWebhookClaimRespectsConcurrentDeferral(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	webhook := &resultWebhookSender{}
	ledger := &deferBeforeClaimLedger{StoreLedger: NewStoreLedger(db), now: now}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	if err := router.Route(ctx, immediateTestIntent(now)); !deliveryFailed(err) {
		t.Fatalf("Route after losing the claim to a deferral = %v, want the delivery failure", err)
	}
	if webhook.calls != 0 {
		t.Fatalf("webhook calls = %d, want no POST inside the drain's backoff", webhook.calls)
	}
	var id int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM notification_intents LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := webhookStateColumn(t, db, id); got != "pending" {
		t.Fatalf("webhook_state = %q, want the deferred row still pending for the drain", got)
	}
}

// Retries and their expiry run on webhook-owned times. Quiet hours or a
// reminder upsert can move the shared available_at later, but the deferred
// row still retries once its own backoff passes, and the retry window still
// closes a day after the first failure.
func TestDigestWebhookRetryIgnoresDesktopAvailability(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	clock := now
	webhook := &resultWebhookSender{err: &WebhookStatusError{StatusCode: 503, Status: "503 Service Unavailable"}}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	recorder := &recordingActivity{}
	router := NewRouter(RouterOptions{Ledger: NewStoreLedger(db), Webhook: webhook, Policy: policy, Activity: recorder, Now: func() time.Time { return clock }})
	if err := router.Route(ctx, immediateTestIntent(now)); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM notification_intents LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	moveAvailability := func(at time.Time) {
		t.Helper()
		if _, err := db.DB().ExecContext(ctx, `UPDATE notification_intents SET available_at=? WHERE id=?`, store.FormatTime(at), id); err != nil {
			t.Fatal(err)
		}
	}
	firstFailure := now.Add(time.Hour)
	clock = firstFailure
	if err := router.RunDueAt(ctx, clock); !deliveryFailed(err) {
		t.Fatalf("first drain = %v, want the recorded delivery failure", err)
	}
	// The desktop leg's availability moves eight hours out; the webhook
	// retry still comes due after its own backoff.
	moveAvailability(clock.Add(8 * time.Hour))
	clock = clock.Add(webhookRetryBackoff + time.Minute)
	if err := router.RunDueAt(ctx, clock); !deliveryFailed(err) {
		t.Fatalf("retry drain = %v, want the recorded delivery failure", err)
	}
	if webhook.calls != 2 {
		t.Fatalf("webhook calls = %d, want the retry despite the moved availability", webhook.calls)
	}
	// Moving the availability again cannot extend the retry window: the
	// first drain past a day since the first failure settles the row.
	clock = firstFailure.Add(webhookRetryExpiry + time.Minute)
	moveAvailability(clock)
	if err := router.RunDueAt(ctx, clock); !deliveryFailed(err) {
		t.Fatalf("expiry drain = %v, want the recorded delivery failure", err)
	}
	if got := webhookStateColumn(t, db, id); got != "failed" {
		t.Fatalf("webhook_state a day after the first failure = %q, want failed", got)
	}
	if last := recorder.failures[len(recorder.failures)-1]; !strings.Contains(last.cause, "retry window expired") {
		t.Fatalf("audit cause = %q, want the expiry named", last.cause)
	}
}

// A producer replay that finds its row already settled failed reports the
// failure again: the notification was never confirmed, so a watch retry
// must not mark the alert delivered.
func TestImmediateWebhookFailedReplayReportsFailure(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	webhook := &resultWebhookSender{err: &WebhookStatusError{StatusCode: 400, Status: "400 Bad Request"}}
	router := NewRouter(RouterOptions{Ledger: NewStoreLedger(db), Webhook: webhook, Policy: immediateTestPolicy(), Now: func() time.Time { return now }})
	if err := router.Route(ctx, immediateTestIntent(now)); !deliveryFailed(err) {
		t.Fatalf("first Route = %v, want the delivery failure", err)
	}
	if err := router.Route(ctx, immediateTestIntent(now)); !deliveryFailed(err) {
		t.Fatalf("replay Route of a failed row = %v, want the delivery failure, not nil", err)
	}
	if webhook.calls != 1 {
		t.Fatalf("webhook calls = %d, want the replay to send nothing", webhook.calls)
	}
}

// A read that fails after the claim committed (a context cancelled between
// the two, here forced by a trigger that corrupts the claimed row) must
// release the claim: nothing was dispatched, so the row stays due instead of
// stranding in sending, which DueWebhook never returns again.
func TestClaimReleasedWhenPostClaimReadFails(t *testing.T) {
	ctx := context.Background()
	db, err := openTestStore(ctx, t)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	webhook := &resultWebhookSender{}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	ledger := NewStoreLedger(db)
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Now: func() time.Time { return now }})
	if err := router.Route(ctx, immediateTestIntent(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `CREATE TRIGGER corrupt_on_claim AFTER UPDATE OF webhook_state ON notification_intents
		WHEN NEW.webhook_state = 'sending'
		BEGIN UPDATE notification_intents SET desktop_sent_payload_json = 'not json' WHERE id = NEW.id; END`); err != nil {
		t.Fatal(err)
	}
	if err := router.RunDueAt(ctx, now.Add(time.Hour)); err == nil {
		t.Fatal("RunDueAt = nil, want the post-claim read failure")
	}
	if webhook.calls != 0 {
		t.Fatalf("webhook calls = %d, want no POST after a failed read", webhook.calls)
	}
	var id int64
	if err := db.DB().QueryRowContext(ctx, `SELECT id FROM notification_intents LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := webhookStateColumn(t, db, id); got != "pending" {
		t.Fatalf("webhook_state after a failed post-claim read = %q, want pending so the row stays due", got)
	}
}

// lateCancelContext reports cancellation from its cancelAt-th Err call on,
// so a test can land a cancellation between two checks on the send path.
type lateCancelContext struct {
	context.Context
	mu       sync.Mutex
	calls    int
	cancelAt int
}

func (c *lateCancelContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

// A cancellation that stops the send before any request goes out is not an
// ambiguous delivery: the claim returns to pending so the next drain sends
// the row, instead of settling failed. Here the cancellation lands after
// the router's own check, inside the production sender's pre-request check.
func TestWebhookSendSkippedByCancellationReleasesClaim(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	webhook := &Webhook{URL: server.URL, Client: server.Client()}
	policy := Policy{DigestEvery: time.Hour, Categories: map[Category]CategoryPolicy{
		CategoryRequestOutcome: {Desktop: "off", Webhook: "digest", Window: time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Webhook: webhook, Policy: policy, Now: func() time.Time { return now }})
	if err := router.Route(context.Background(), immediateTestIntent(now)); err != nil {
		t.Fatal(err)
	}
	var id int64
	for rowID := range ledger.rows {
		id = rowID
	}
	claimed, ok, err := ledger.ClaimWebhook(context.Background(), id, now.Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v, want true", ok, err)
	}
	ctx := &lateCancelContext{Context: context.Background(), cancelAt: 2}
	if err := router.sendWebhookLeg(ctx, claimed, now.Add(time.Hour)); !errors.Is(err, context.Canceled) || deliveryFailed(err) {
		t.Fatalf("sendWebhookLeg = %v, want the cancellation, not a recorded delivery failure", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("endpoint requests = %d, want none for a skipped dispatch", n)
	}
	if got := ledger.rows[id].WebhookState; got != "pending" {
		t.Fatalf("webhook_state after a skipped dispatch = %q, want pending so the drain resends", got)
	}
}
