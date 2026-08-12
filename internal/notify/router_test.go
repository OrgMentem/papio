// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"errors"
	"regexp"
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
