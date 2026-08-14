// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// RouterOptions wires durable intent storage and independent channels.
type RouterOptions struct {
	Ledger     Ledger
	Desktop    Sender
	Webhook    Sender
	Policy     Policy
	Presence   PresenceProvider
	Activity   ActivityRecorder
	Now        func() time.Time
	Revalidate func(context.Context, Record) (bool, error)
}

type Ledger interface {
	Upsert(context.Context, Record) (Record, error)
	DueDesktop(context.Context, time.Time, int) ([]Record, error)
	ReserveDesktop(context.Context, int64, time.Time, int) (bool, error)
	SetDesktopState(context.Context, int64, string, time.Time) error
	SetWebhookState(context.Context, int64, string, time.Time) error
	SupersedeCheckpoints(context.Context, string, time.Time) (int, error)
	LatestCheckpoint(context.Context, string) (Record, bool, error)
}

type Router struct {
	ledger     Ledger
	desktop    Sender
	webhook    Sender
	policy     Policy
	presence   PresenceProvider
	activity   ActivityRecorder
	now        func() time.Time
	revalidate func(context.Context, Record) (bool, error)
	presenceMu sync.RWMutex
}

func NewRouter(options RouterOptions) *Router {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Router{ledger: options.Ledger, desktop: options.Desktop, webhook: options.Webhook, policy: options.Policy, presence: options.Presence, activity: options.Activity, now: now, revalidate: options.Revalidate}
}

// SetPresence updates the holder-independent focused-surface provider. It is
// safe for bootstrap to construct the router before the bridge is ready.
func (r *Router) SetPresence(provider PresenceProvider) {
	if r == nil {
		return
	}
	r.presenceMu.Lock()
	r.presence = provider
	r.presenceMu.Unlock()
}

func (r *Router) focusedSurface(now time.Time) bool {
	r.presenceMu.RLock()
	provider := r.presence
	r.presenceMu.RUnlock()
	return provider != nil && provider.AnyFocused(now)
}

func (r *Router) currentTime() time.Time {
	if r != nil && r.now != nil {
		return r.now().UTC()
	}
	return time.Now().UTC()
}

func (r *Router) Route(ctx context.Context, intent Intent) error {
	if r == nil || r.ledger == nil {
		return fmt.Errorf("notification router ledger is unavailable")
	}
	if strings.HasPrefix(intent.EventKind, "notify.") {
		return nil
	}
	if err := validateIntent(intent); err != nil {
		return err
	}
	now := r.currentTime()
	if intent.Category == CategoryCompletionBatch && intent.Phase == PhaseFinal {
		previous, found, err := r.ledger.LatestCheckpoint(ctx, intent.AggregateKey)
		if err != nil {
			return err
		}
		if found && previous.DesktopState == "attempted" && samePublicOutcome(previous.Intent.Detail, intent.Detail) {
			return nil
		}
	}
	payload := intent.Detail
	if payload.Message == "" {
		payload.Message = intent.Message
	}
	if payload.Kind == "" {
		payload.Kind = intent.EventKind
	}
	payload.Message = ComposeMessage(intent.Category, payload.Count, payload, payload.Message)
	if payload.Count == 0 {
		payload.Count = 1
	}
	categoryPolicy := r.policy.For(intent.Category)
	first := intent.HappenedAt.UTC()
	available := now
	if intent.Category == CategoryCompletionBatch && intent.Phase == PhaseCheckpoint {
		quiet := r.policy.CompletionQuiet
		ceiling := r.policy.CompletionMaxHold
		available = intent.HappenedAt.UTC().Add(quiet)
		if ceiling > 0 {
			limit := intent.WindowStart.UTC().Add(ceiling)
			if limit.Before(available) {
				available = limit
			}
		}
	} else if intent.Category == CategoryDecisionPending {
		interval := r.policy.DigestEvery
		if interval <= 0 {
			interval = categoryPolicy.Window
		}
		available = intent.WindowStart.UTC().Add(interval)
	} else if intent.Phase != PhaseFinal && (intent.Category == CategoryDecisionOpened || categoryPolicy.Desktop == "digest") && categoryPolicy.Window > 0 {
		// Immediate means "deliver when this coalescing window closes", not
		// "bypass coalescing"; digest modes use the same interval.
		available = intent.WindowStart.UTC().Add(categoryPolicy.Window)
	}
	if categoryPolicy.Webhook == "digest" && intent.Category != CategoryDecisionPending && available.Equal(now) {
		interval := r.policy.DigestEvery
		if interval <= 0 {
			interval = categoryPolicy.Window
		}
		available = now.Add(interval)
	}
	if available.Before(now) {
		available = now
	}
	desktopState := "pending"
	if categoryPolicy.Desktop == "off" {
		desktopState = "platform_unavailable"
	}
	webhookState := "pending"
	if categoryPolicy.Webhook == "off" || r.webhook == nil {
		webhookState = "skipped"
	}
	recordInput := Record{Intent: Intent{EventKind: intent.EventKind, Category: intent.Category, AggregateKey: intent.AggregateKey, Phase: intent.Phase, WindowStart: intent.WindowStart.UTC(), JobID: intent.JobID, BatchID: intent.BatchID, ScanID: intent.ScanID, HappenedAt: intent.HappenedAt.UTC(), Message: payload.Message, Detail: payload}, FirstAt: first, LastAt: intent.HappenedAt.UTC(), AvailableAt: available, Count: payload.Count, DesktopState: desktopState, WebhookState: webhookState}
	var record Record
	var err error
	if intent.Category == CategoryCompletionBatch && intent.Phase == PhaseFinal {
		if atomic, ok := r.ledger.(atomicFinalizer); ok {
			record, err = atomic.SupersedeAndUpsertCheckpoint(ctx, intent.AggregateKey, now, recordInput)
		} else {
			// Alternate ledgers pre-dating the atomic method retain the old
			// sequence; production StoreLedger always implements the transaction.
			if _, err = r.ledger.SupersedeCheckpoints(ctx, intent.AggregateKey, now); err == nil {
				record, err = r.ledger.Upsert(ctx, recordInput)
			}
		}
	} else {
		record, err = r.ledger.Upsert(ctx, recordInput)
	}
	if err != nil {
		return err
	}
	// Webhooks are automation: they are not subject to desktop focus, quiet
	// hours, supersession, or rate state. Immediate legs dispatch now; digest
	// legs are drained by RunDue.
	if webhookState == "pending" && categoryPolicy.Webhook != "digest" {
		r.sendEvent(ctx, r.webhook, payload)
		if err := r.ledger.SetWebhookState(ctx, record.ID, "attempted", now); err != nil {
			return err
		}
	}
	if intent.Category == CategoryDecisionPending && categoryPolicy.Desktop == "digest" {
		r.audit(ctx, "notify.digest", record, "decision_pending")
	}
	return nil
}

type atomicFinalizer interface {
	SupersedeAndUpsertCheckpoint(context.Context, string, time.Time, Record) (Record, error)
}

type webhookDueLedger interface {
	DueWebhook(context.Context, time.Time, int) ([]Record, error)
}

// RunDue drains every due desktop leg. It satisfies the daemon scheduler's
func (r *Router) RunDue(ctx context.Context) error {
	return r.RunDueAt(ctx, time.Time{})
}

func (r *Router) RunDueAt(ctx context.Context, now time.Time) error {
	if r == nil || r.ledger == nil {
		return fmt.Errorf("notification router ledger is unavailable")
	}
	if now.IsZero() {
		now = r.currentTime()
	}
	now = now.UTC()
	rows, err := r.ledger.DueDesktop(ctx, now, 100)
	if err != nil {
		return err
	}
	if dueWebhook, ok := r.ledger.(webhookDueLedger); ok && r.webhook != nil {
		webhooks, err := dueWebhook.DueWebhook(ctx, now, 100)
		if err != nil {
			return err
		}
		for _, row := range webhooks {
			if r.policy.For(row.Intent.Category).Webhook != "digest" {
				continue
			}
			event := row.Intent.Detail
			event.Count = row.Count
			event.Message = ComposeMessage(row.Intent.Category, row.Count, event, row.Intent.Message)
			r.sendEvent(ctx, r.webhook, event)
			if err := r.ledger.SetWebhookState(ctx, row.ID, "attempted", now); err != nil {
				return err
			}
		}
	}
	for _, row := range rows {
		if r.revalidate != nil {
			valid, err := r.revalidate(ctx, row)
			if err != nil {
				return err
			}
			if !valid {
				_ = r.ledger.SetDesktopState(ctx, row.ID, "superseded", now)
				r.audit(ctx, "notify.held", row, "superseded")
				continue
			}
		}
		categoryPolicy := r.policy.For(row.Intent.Category)
		if categoryPolicy.Desktop == "off" || r.desktop == nil {
			if err := r.ledger.SetDesktopState(ctx, row.ID, "platform_unavailable", now); err != nil {
				return err
			}
			continue
		}
		if r.policy.QuietHours.Contains(now) {
			if r.policy.QuietMode == "drop" {
				if err := r.ledger.SetDesktopState(ctx, row.ID, "dropped_quiet", now); err != nil {
					return err
				}
			} else {
				if err := r.ledger.SetDesktopState(ctx, row.ID, "held", now); err != nil {
					return err
				}
				r.deferDesktop(ctx, row.ID, r.nextQuietRelease(now))
				r.audit(ctx, "notify.held", row, "quiet_hours")
			}
			continue
		}
		if r.focusedSurface(now) {
			if err := r.ledger.SetDesktopState(ctx, row.ID, "suppressed_presence", now); err != nil {
				return err
			}
			continue
		}
		reserved, err := r.ledger.ReserveDesktop(ctx, row.ID, now, r.policy.MaxPerHour)
		if err != nil {
			return err
		}
		if !reserved {
			// ReserveDesktop may observe a concurrent supersession; its
			// conditional update is authoritative, so do not resurrect it.
			if current, ok := r.ledger.(interface {
				DueDesktop(context.Context, time.Time, int) ([]Record, error)
			}); ok {
				due, readErr := current.DueDesktop(ctx, now, 100)
				if readErr != nil {
					return readErr
				}
				stillPending := false
				for _, candidate := range due {
					if candidate.ID == row.ID && (candidate.DesktopState == "pending" || candidate.DesktopState == "held") {
						stillPending = true
						break
					}
				}
				if !stillPending {
					continue
				}
			}
			if err := r.ledger.SetDesktopState(ctx, row.ID, "held", now); err != nil {
				return err
			}
			r.deferDesktop(ctx, row.ID, now.Add(time.Minute))
			r.audit(ctx, "notify.held", row, "rate_limit")
			continue
		}
		// Reservation is terminal for replay safety. A process crash after this
		// point must not invoke the platform a second time.
		row.Intent.Detail.Message = ComposeMessage(row.Intent.Category, row.Count, row.Intent.Detail, row.Intent.Message)
		row.Intent.Detail.Count = row.Count
		r.sendDesktop(ctx, row.Intent.Detail, row.Intent.Message)
		if err := r.ledger.SetDesktopState(ctx, row.ID, "attempted", now); err != nil {
			return err
		}
		r.audit(ctx, "notify.attempted", row, "attempted")
	}
	return nil
}

type desktopAvailabilitySetter interface {
	SetDesktopAvailable(context.Context, int64, time.Time) error
}

func (r *Router) deferDesktop(ctx context.Context, id int64, at time.Time) {
	if setter, ok := r.ledger.(desktopAvailabilitySetter); ok {
		_ = setter.SetDesktopAvailable(ctx, id, at)
	}
}

func (r *Router) nextQuietRelease(now time.Time) time.Time {
	q := r.policy.QuietHours
	if !q.Enabled() {
		return now.Add(time.Minute)
	}
	local := now.In(q.Location)
	endHour := int(q.End / time.Hour)
	endMinute := int((q.End % time.Hour) / time.Minute)
	build := func(day time.Time) time.Time {
		candidate := time.Date(day.Year(), day.Month(), day.Day(), endHour, endMinute, 0, 0, q.Location)
		// time.Date normalizes nonexistent spring-forward wall times. Walk
		// forward to the first valid instant at or after the configured end.
		for range 180 {
			wall := candidate.In(q.Location)
			if wall.Hour()*60+wall.Minute() >= endHour*60+endMinute {
				break
			}
			candidate = candidate.Add(time.Minute)
		}
		return candidate
	}
	candidate := build(local)
	if !candidate.After(now) {
		// Advance by local calendar date, not 24 elapsed hours. This avoids
		// shifting release across DST transitions and does not release twice
		// during a fall-back overlap.
		nextDay := time.Date(local.Year(), local.Month(), local.Day()+1, 12, 0, 0, 0, q.Location)
		candidate = build(nextDay)
	}
	return candidate.UTC()
}

func (r *Router) sendDesktop(ctx context.Context, event Event, fallback string) {
	if event.Message == "" {
		event.Message = fallback
	}
	event.Message = stripTerminalControls(event.Message)
	r.sendEvent(ctx, r.desktop, event)
}

func (r *Router) sendEvent(ctx context.Context, sender Sender, event Event) {
	if sender == nil {
		return
	}
	if structured, ok := sender.(EventSender); ok {
		structured.SendEvent(ctx, event)
		return
	}
	sender.Send(ctx, event.Message)
}

func (r *Router) audit(ctx context.Context, kind string, row Record, reason string) {
	if r.activity == nil {
		return
	}
	_ = r.activity.RecordSystemEvent(ctx, kind, map[string]any{"category": row.Intent.Category, "event_kind": row.Intent.EventKind, "aggregate_key": row.Intent.AggregateKey, "phase": row.Intent.Phase, "count": row.Count, "reason": reason})
}

// ErrPreviewCountUnrepresentable reports a preview count a category can never
// produce. Operator surfaces map it to an invalid-argument response so the
// count is corrected rather than silently rendered as different copy.
var ErrPreviewCountUnrepresentable = errors.New("notification count is not representable for this category")

// Preview renders the exact copy one notification in this category would carry
// for count durable events, without sending anything.
//
// A category that always stands for exactly one event rejects count > 1: no
// such notification can exist, so rendering its one-event copy for a count of
// 27 would teach an operator a vocabulary papio never emits.
func (r *Router) Preview(category Category, count int) (string, error) {
	if !isKnownCategory(category) {
		return "", fmt.Errorf("unknown notification category %q", category)
	}
	if count < 1 {
		count = 1
	}
	if count > 1 && CategoryRepresentsOneEvent(category) {
		return "", fmt.Errorf("%w: %s stands for exactly one %s, so a count of %d can never occur; preview it with count 1%s",
			ErrPreviewCountUnrepresentable, category, singleEventNoun(category), count, aggregateAlternative(category))
	}
	return stripTerminalControls(ComposeMessage(category, count, Event{}, "")), nil
}

func singleEventNoun(category Category) string {
	if category == CategoryRequestOutcome {
		return "standalone request"
	}
	return "state episode"
}

func aggregateAlternative(category Category) string {
	if category == CategoryRequestOutcome {
		return ", or preview completion_batch for cohort totals"
	}
	return ""
}

// Test sends one explicit local notification without creating a durable
// intent. It is reserved for the operator's notify test command and does not
// apply quiet hours, focus suppression, or the normal desktop budget.
func (r *Router) Test(ctx context.Context, category Category) (string, error) {
	if r == nil || r.desktop == nil {
		return "", fmt.Errorf("desktop notifications are unavailable on this platform or not configured")
	}
	message, err := r.Preview(category, 1)
	if err != nil {
		return "", err
	}
	event := Event{Kind: string(category), Message: message, Count: 1}
	r.sendDesktop(ctx, event, message)
	return message, nil
}

// Table returns the router's effective category policy for read-only surfaces.
func (r *Router) Table() []RouteRow {
	if r == nil {
		return nil
	}
	return r.policy.Table()
}

var ansiCSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func stripTerminalControls(value string) string {
	value = ansiCSI.ReplaceAllString(value, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
}

func samePublicOutcome(a, b Event) bool {
	clean := func(event Event) []byte {
		data, _ := json.Marshal(event)
		var object map[string]any
		_ = json.Unmarshal(data, &object)
		for _, key := range []string{"message", "at", "sent_at", "timestamp", "happened_at", "window_start", "first_at", "last_at"} {
			delete(object, key)
		}
		data, _ = json.Marshal(object)
		return data
	}
	return string(clean(a)) == string(clean(b))
}
