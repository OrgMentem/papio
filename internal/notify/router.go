// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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
	SetDesktopState(context.Context, int64, string, time.Time) (bool, error)
	// ClaimWebhook atomically moves a pending webhook leg to sending and
	// returns the claimed row's immutable snapshot with whether the claim
	// applied. The router POSTs only on a won claim, which keeps replay,
	// concurrent, and restart delivery to one POST per row and one POST per
	// delivery key. The returned row is the send source: a concurrent Route
	// can coalesce another event after this Route's Upsert returned but
	// before its claim won, so the Upsert snapshot can be stale. A merge
	// after the claim queues a new generation row under its own key instead
	// of rewriting the claimed snapshot. A deferred leg is not claimable
	// before its retry time (the time argument), so the claim itself
	// enforces the backoff. See the store ClaimWebhook for the full policy.
	ClaimWebhook(context.Context, int64, time.Time) (Record, bool, error)
	SetWebhookState(context.Context, int64, string, time.Time) error
	// DeferWebhook requeues a claimed leg as pending with a retry time, so a
	// later drain retries the same row under its stable delivery key. The
	// router calls it for provable non-delivery (dial, DNS, or certificate
	// failures, 429/503) instead of settling failed. It reports false, and
	// leaves the row for the caller to settle failed, once the window has
	// elapsed since the leg first failed. Both times are webhook-owned:
	// available_at stays the desktop leg's.
	DeferWebhook(ctx context.Context, id int64, failedAt, retryAt time.Time, window time.Duration) (bool, error)
	SupersedeCheckpoints(context.Context, string, time.Time) (int, error)
	LatestCheckpoint(context.Context, string) (Record, bool, error)
}

// webhookResultSender is optionally implemented by webhook senders that
// report delivery outcome. The production Webhook implements it; legacy
// senders that only satisfy Sender or EventSender keep the old
// fire-and-forget behavior and always settle as attempted.
type webhookResultSender interface {
	SendEventResult(context.Context, Event) error
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
		if found && previous.DesktopState == "attempted" {
			// Compare against what the desktop leg actually delivered, not the
			// live row: a still-pending webhook digest keeps merging into it.
			sent, _ := previous.DesktopSent()
			ok, err := samePublicOutcome(sent, intent.Detail)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
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
	} else if intent.Category == CategoryDecisionPending && intent.Phase == PhaseReminder {
		// The reminder producer routes an absolute total snapshot each pass.
		// A crash between Route and its markers retries the identical total,
		// so replace the count instead of adding it. Production StoreLedger
		// implements the snapshot; alternate ledgers keep additive Upsert.
		// When the current generation's webhook leg is already claimed or
		// settled, the store redirects the retry to a fresh pending
		// generation; the router then claims and POSTs that generation.
		if replacer, ok := r.ledger.(pendingDigestReplacer); ok {
			record, err = replacer.UpsertPendingDigest(ctx, recordInput)
		} else {
			record, err = r.ledger.Upsert(ctx, recordInput)
		}
	} else if redirector, ok := r.ledger.(webhookGenerationRedirector); ok {
		// Immediate-webhook events merge only into a pending webhook
		// generation; a merge after the claim redirects to a fresh
		// generation row with its own delivery key. Categories without a
		// webhook leg use plain desktop coalescing below, which is the
		// same path ledgers without the method take.
		if webhookState == "pending" {
			record, err = redirector.UpsertWebhookGeneration(ctx, recordInput)
		} else {
			record, err = r.ledger.Upsert(ctx, recordInput)
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
	//
	// Webhook delivery is at-most-once per row and per delivery key. The
	// router claims the pending leg before its POST, so a replayed intent, a
	// concurrent Route, or a restart after the claim cannot POST the same row
	// twice. A retryable failure (a dial, DNS, or certificate error or a
	// 429/503 answer, all provable non-delivery) requeues the row as pending
	// with a backoff, so the drain retries it under the same key; any other
	// failure — an EOF or reset after the request went out, any other
	// endpoint answer — settles the leg as failed instead. A crash between
	// claim and settle leaves sending, which never becomes due again: the
	// delivery may be lost but is never duplicated. The claimed snapshot is
	// immutable: a concurrent Route that lands after the claim queues its
	// event under a new generation row with its own delivery key, so the
	// second POST never reuses the first key. Local ledger state cannot give
	// exactly-once across a lost HTTP response, so every POST carries the
	// stable per-row delivery key for receiver correlation. A row still
	// undelivered a day after its first failure settles failed, so an outage
	// that never clears stays visible instead of retrying forever.
	//
	// A failed immediate POST is the caller's error to handle: producers that
	// retry from durable state (the watch alert path, the action reminder)
	// treat it as not delivered and route again, so a lost alert never reads
	// as alerted. A deferred row is the drain's to retry once its backoff
	// passes: Route does not POST it early, and reports the deferral so the
	// producer does not mark it delivered either. A replay that finds its
	// row settled failed reports the failure again for the same reason.
	if webhookState == "pending" && categoryPolicy.Webhook != "digest" && r.webhook != nil {
		switch {
		case record.WebhookState == "failed":
			return errWebhookFailedReplay
		case record.WebhookState != "pending":
			// Claimed or delivered by a concurrent Route: nothing to
			// dispatch from this snapshot.
		case record.WebhookAttemptedAt.After(now):
			return errWebhookRetryDeferred
		default:
			claimedRow, claimed, err := r.ledger.ClaimWebhook(ctx, record.ID, now)
			if err != nil {
				return err
			}
			if !claimed {
				// The row moved between the snapshot and the claim: a
				// concurrent Route or drain claimed, deferred, or settled
				// it. Only a confirmed delivery may read as success.
				return r.lostClaimOutcome(ctx, record.ID)
			}
			if err := r.sendWebhookLeg(ctx, claimedRow, now); err != nil {
				return err
			}
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

// webhookGenerationRedirector stores one event under the current pending
// webhook generation, redirecting to a fresh generation row when the
// identity row's webhook leg is already claimed or settled. Production
// StoreLedger implements it; test ledgers mirror it in Upsert.
type webhookGenerationRedirector interface {
	UpsertWebhookGeneration(context.Context, Record) (Record, error)
}

// pendingDigestReplacer stores an absolute digest snapshot instead of adding
// to the row. Production StoreLedger implements it for the reminder retry
// contract; test ledgers may keep additive Upsert.
type pendingDigestReplacer interface {
	UpsertPendingDigest(context.Context, Record) (Record, error)
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
	var deliveryErr error
	if dueWebhook, ok := r.ledger.(webhookDueLedger); ok && r.webhook != nil {
		webhooks, err := dueWebhook.DueWebhook(ctx, now, 100)
		if err != nil {
			return err
		}
		for _, row := range webhooks {
			if r.policy.For(row.Intent.Category).Webhook == "off" {
				continue
			}
			// A cancelled drain claims nothing: DueWebhook on the store
			// already fails on a dead context, and this keeps ledgers that
			// ignore cancellation to the same contract.
			if err := ctx.Err(); err != nil {
				return err
			}
			// Same at-most-once claim as immediate legs: only the drain that
			// wins the pending-to-sending claim POSTs. A crash between claim
			// and settle leaves sending behind, which DueWebhook never
			// returns, so a restart cannot duplicate the POST. The claimed
			// snapshot is immutable: a concurrent Route after the claim
			// queues a new generation row under its own key.
			//
			// The drain also retries deferred immediate legs: request
			// outcomes and publication notices have no producer retry, so
			// leaving a deferred row to Route would strand it. A recorded
			// delivery failure does not stop the pass, because the row is
			// already requeued or settled; it is returned after the desktop
			// drain. A ledger write failure stops the pass at once.
			claimedRow, claimed, err := r.ledger.ClaimWebhook(ctx, row.ID, now)
			if err != nil {
				return err
			}
			if !claimed {
				continue
			}
			if err := r.sendWebhookLeg(ctx, claimedRow, now); err != nil {
				if !deliveryFailed(err) {
					return err
				}
				if deliveryErr == nil {
					deliveryErr = err
				}
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
				applied, err := r.ledger.SetDesktopState(ctx, row.ID, "superseded", now)
				if err != nil {
					return err
				}
				if applied {
					r.audit(ctx, "notify.held", row, "superseded")
				}
				continue
			}
		}
		categoryPolicy := r.policy.For(row.Intent.Category)
		if categoryPolicy.Desktop == "off" || r.desktop == nil {
			if _, err := r.ledger.SetDesktopState(ctx, row.ID, "platform_unavailable", now); err != nil {
				return err
			}
			continue
		}
		if r.policy.QuietHours.Contains(now) {
			if r.policy.QuietMode == "drop" {
				if _, err := r.ledger.SetDesktopState(ctx, row.ID, "dropped_quiet", now); err != nil {
					return err
				}
				continue
			}
			held, err := r.ledger.SetDesktopState(ctx, row.ID, "held", now)
			if err != nil {
				return err
			}
			if !held {
				// A terminal transition won this row between the due read and
				// now. Holding it would resurrect a superseded checkpoint,
				// which a later drain would then deliver after its final
				// notification.
				continue
			}
			if err := r.deferDesktop(ctx, row.ID, r.nextQuietRelease(now)); err != nil {
				return err
			}
			r.audit(ctx, "notify.held", row, "quiet_hours")
			continue
		}
		if r.focusedSurface(now) {
			if _, err := r.ledger.SetDesktopState(ctx, row.ID, "suppressed_presence", now); err != nil {
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
			held, err := r.ledger.SetDesktopState(ctx, row.ID, "held", now)
			if err != nil {
				return err
			}
			if !held {
				continue
			}
			if err := r.deferDesktop(ctx, row.ID, now.Add(time.Minute)); err != nil {
				return err
			}
			r.audit(ctx, "notify.held", row, "rate_limit")
			continue
		}
		// Reservation is terminal for replay safety. A process crash after this
		// point must not invoke the platform a second time.
		row.Intent.Detail.Message = ComposeMessage(row.Intent.Category, row.Count, row.Intent.Detail, row.Intent.Message)
		row.Intent.Detail.Count = row.Count
		r.sendDesktop(ctx, row.Intent.Detail, row.Intent.Message)
		if _, err := r.ledger.SetDesktopState(ctx, row.ID, "attempted", now); err != nil {
			return err
		}
		r.audit(ctx, "notify.attempted", row, "attempted")
	}
	return deliveryErr
}

type desktopAvailabilitySetter interface {
	SetDesktopAvailable(context.Context, int64, time.Time) error
}

func (r *Router) deferDesktop(ctx context.Context, id int64, at time.Time) error {
	if setter, ok := r.ledger.(desktopAvailabilitySetter); ok {
		return setter.SetDesktopAvailable(ctx, id, at)
	}
	return nil
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

// deliverWebhook POSTs one claimed webhook event and reports the endpoint
// outcome. Senders that implement SendEventResult give a real signal, so a
// transport error or non-2xx status settles the leg instead of attempted.
// Legacy senders keep fire-and-forget and always report success. A context
// that is already done skips the dispatch and reports errWebhookNotDispatched.
func (r *Router) deliverWebhook(ctx context.Context, event Event) error {
	if r == nil || r.webhook == nil {
		return fmt.Errorf("notification webhook is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", errWebhookNotDispatched, err)
	}
	if reporter, ok := r.webhook.(webhookResultSender); ok {
		return reporter.SendEventResult(ctx, event)
	}
	r.sendEvent(ctx, r.webhook, event)
	return nil
}

// errWebhookNotDispatched reports a webhook send that stopped before any
// request went out, because its context was already done. Nothing reached
// the endpoint, so the claim is released rather than settled failed.
var errWebhookNotDispatched = errors.New("webhook not dispatched")

// webhookRowReader re-reads one ledger row. Production StoreLedger and the
// test ledgers implement it.
type webhookRowReader interface {
	GetByID(context.Context, int64) (Record, error)
}

// lostClaimOutcome reports a Route whose claim lost to a concurrent change.
// It returns success only when the row's webhook leg is confirmed delivered;
// a leg still in flight, deferred, or failed is a delivery failure, so a
// producer that retries from durable state does not record a receipt.
func (r *Router) lostClaimOutcome(ctx context.Context, id int64) error {
	reader, ok := r.ledger.(webhookRowReader)
	if !ok {
		return nil
	}
	row, err := reader.GetByID(ctx, id)
	if err != nil {
		return err
	}
	switch row.WebhookState {
	case "attempted", "delivered":
		return nil
	case "failed":
		return errWebhookFailedReplay
	default:
		return fmt.Errorf("%w: the webhook leg is %s, not delivered", errWebhookDelivery, row.WebhookState)
	}
}

// webhookRetryBackoff paces drain retries of one provably undelivered row:
// long enough to let a restarting endpoint recover, short enough that a
// digest still arrives the same day.
const webhookRetryBackoff = 5 * time.Minute

// webhookRetryExpiry caps the retry loop: a row still undelivered a day
// after its first failure settles failed, so an outage that never clears
// stays visible instead of retrying forever.
const webhookRetryExpiry = 24 * time.Hour

// retryableWebhookError reports provable non-delivery: the request never
// reached the endpoint, so retrying under the same delivery key cannot
// duplicate. Only dial, DNS, and certificate failures (the request was never
// sent) and 429/503 answers (explicitly retryable) qualify. Any other
// failure — an EOF or reset after the request went out, any other endpoint
// answer — may mean the receiver processed the POST, so the leg settles
// terminal failed instead. Context errors never qualify: a POST cancelled
// in flight may have arrived.
func retryableWebhookError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var status *WebhookStatusError
	if errors.As(err, &status) {
		return status.StatusCode == http.StatusTooManyRequests || status.StatusCode == http.StatusServiceUnavailable
	}
	return provableNonDelivery(err)
}

// provableNonDelivery reports transport failures where the request was never
// sent: dial errors (connection refused, unreachable network, DNS failures)
// and certificate verification failures, which end the TLS handshake before
// any request byte is written. Anything later in the exchange — a TLS alert
// or an EOF or reset after the request went out, a stalled response — may
// have reached the endpoint.
func provableNonDelivery(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// webhookCause names a delivery failure for the audit record without quoting
// the endpoint URL, whose path can carry a secret. A non-2xx answer carries
// its status; a dial, DNS, or certificate failure carries its category; any
// other transport failure stays a generic transport error.
func webhookCause(err error) string {
	var status *WebhookStatusError
	if errors.As(err, &status) {
		return fmt.Sprintf("endpoint answered HTTP %d", status.StatusCode)
	}
	if errors.Is(err, context.Canceled) {
		return "the request was cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "the request timed out"
	}
	if provableNonDelivery(err) {
		return webhookTransportCause(err)
	}
	return "transport error"
}

// webhookTransportCause names a provable non-delivery without the URL that
// the underlying error quotes.
func webhookTransportCause(err error) string {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "DNS lookup failed"
	}
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		return "TLS certificate rejected"
	}
	return "connection failed"
}

// errWebhookDelivery is the delivery failure sendWebhookLeg reports. Route
// surfaces it to producers that retry from durable state; the drain returns
// it so the scheduler sees the failed pass. The row is already requeued or
// settled either way; a ledger write failure is returned unwrapped.
var errWebhookDelivery = errors.New("delivering webhook notification")

// errWebhookRetryDeferred reports a Route for a deferred row: only the drain
// POSTs it, once its backoff passes, so the producer must not read the
// notification as delivered. It wraps errWebhookDelivery.
var errWebhookRetryDeferred = fmt.Errorf("%w: retry deferred until the backoff passes", errWebhookDelivery)

// errWebhookFailedReplay reports a Route whose event is already stored in a
// row whose webhook leg settled failed: the notification was never confirmed
// delivered, so the producer must not read it as delivered. It wraps
// errWebhookDelivery.
var errWebhookFailedReplay = fmt.Errorf("%w: the webhook leg already failed", errWebhookDelivery)

// deliveryFailed reports whether err is a recorded delivery failure rather
// than a ledger write failure the scheduler must see.
func deliveryFailed(err error) bool {
	return errors.Is(err, errWebhookDelivery)
}

// webhookSettleTimeout bounds each detached ledger write that records a
// webhook outcome, so a shutdown cannot hang on it.
var webhookSettleTimeout = 5 * time.Second

// detachedSettle returns the context that records a webhook outcome: it
// survives the caller's cancellation and has its own short deadline. Callers
// create it after the POST returns, so a slow send cannot spend it.
func detachedSettle(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), webhookSettleTimeout)
}

// sendWebhookLeg POSTs one claimed webhook event and records the outcome. It
// reports the delivery error wrapped in errWebhookDelivery so Route surfaces
// it to producers that retry from durable state and the drain fails the
// pass for the scheduler: the row is already requeued or settled, and a
// ledger write failure is returned unwrapped so the two stay apart.
//
// A send that stopped before any request went out (errWebhookNotDispatched:
// the context was already done when the dispatch began, in the router or in
// the sender) releases the claim to pending on a detached context with a
// short timeout, so the next drain resends the row: nothing was delivered,
// so this cannot duplicate. A cancellation that lands while the POST is in
// flight is ambiguous and settles failed. Settles and the audit row run
// detached with a short timeout that starts after the POST returns, so a
// shutdown after a real POST still records the outcome without hanging the
// drain.
func (r *Router) sendWebhookLeg(ctx context.Context, claimedRow Record, now time.Time) error {
	event := claimedRow.Intent.Detail
	event.Count = claimedRow.Count
	event.Message = ComposeMessage(claimedRow.Intent.Category, claimedRow.Count, event, claimedRow.Intent.Message)
	event.DeliveryKey = WebhookDeliveryKey(claimedRow.ID)
	sendErr := r.deliverWebhook(ctx, event)
	settle, cancel := detachedSettle(ctx)
	defer cancel()
	if sendErr == nil {
		return r.ledger.SetWebhookState(settle, claimedRow.ID, "attempted", now)
	}
	if errors.Is(sendErr, errWebhookNotDispatched) {
		if rerr := r.ledger.SetWebhookState(settle, claimedRow.ID, "pending", now); rerr != nil {
			return rerr
		}
		return sendErr
	}
	if ctx.Err() != nil {
		// The POST may have gone out before the cancellation landed, so
		// at-most-once wins over retry: settle the ambiguous leg as failed
		// on the detached context instead of stranding it in sending.
		if serr := r.ledger.SetWebhookState(settle, claimedRow.ID, "failed", now); serr != nil {
			return serr
		}
		r.audit(settle, "notify.webhook_failed", claimedRow, "webhook_failed", webhookCause(sendErr))
		return fmt.Errorf("%w: %w", errWebhookDelivery, sendErr)
	}
	cause := webhookCause(sendErr)
	if retryableWebhookError(sendErr) {
		// Provable non-delivery: requeue the row so the drain retries it
		// under the same key after the backoff, unless the retry window
		// since the first failure has run out.
		deferred, derr := r.ledger.DeferWebhook(settle, claimedRow.ID, now, now.Add(webhookRetryBackoff), webhookRetryExpiry)
		if derr != nil {
			return derr
		}
		if deferred {
			r.audit(settle, "notify.webhook_failed", claimedRow, "webhook_failed", cause)
			return fmt.Errorf("%w: %w", errWebhookDelivery, sendErr)
		}
		cause += "; retry window expired"
	}
	if serr := r.ledger.SetWebhookState(settle, claimedRow.ID, "failed", now); serr != nil {
		return serr
	}
	r.audit(settle, "notify.webhook_failed", claimedRow, "webhook_failed", cause)
	return fmt.Errorf("%w: %w", errWebhookDelivery, sendErr)
}

func (r *Router) audit(ctx context.Context, kind string, row Record, reason string, cause ...string) {
	if r.activity == nil {
		return
	}
	// The audit trail is advisory: it explains a routing decision after the
	// fact. A failed write must not change the decision itself, so the error is
	// discarded here deliberately rather than propagated into delivery.
	detail := map[string]any{"category": row.Intent.Category, "event_kind": row.Intent.EventKind, "aggregate_key": row.Intent.AggregateKey, "phase": row.Intent.Phase, "count": row.Count, "reason": reason}
	if len(cause) > 0 && cause[0] != "" {
		detail["cause"] = cause[0]
	}
	_ = r.activity.RecordSystemEvent(ctx, kind, detail)
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

// WebhookFormat reports the request format of the configured webhook, or ""
// when no webhook is configured. Operator surfaces use it to show which shape
// the receiver gets.
func (r *Router) WebhookFormat() string {
	if r == nil {
		return ""
	}
	if sender, ok := r.webhook.(interface{ FormatName() string }); ok {
		return sender.FormatName()
	}
	return ""
}

// ErrWebhookNotConfigured reports a webhook test with no webhook configured.
var ErrWebhookNotConfigured = errors.New("no notification webhook is configured")

// TestWebhook posts one explicit test event through the webhook leg without
// creating a durable intent, so the receiver sees the same request shape a
// real notification has. Unlike the desktop test it learns the outcome: a
// transport error or non-2xx answer is returned.
func (r *Router) TestWebhook(ctx context.Context, category Category) (string, error) {
	if r == nil || r.webhook == nil {
		return "", ErrWebhookNotConfigured
	}
	message, err := r.Preview(category, 1)
	if err != nil {
		return "", err
	}
	if err := r.deliverWebhook(ctx, Event{Kind: string(category), Message: message, Count: 1}); err != nil {
		return message, err
	}
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

func samePublicOutcome(a, b Event) (bool, error) {
	clean := func(event Event) ([]byte, error) {
		data, err := json.Marshal(event)
		if err != nil {
			return nil, err
		}
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			return nil, err
		}
		for _, key := range []string{"message", "at", "sent_at", "timestamp", "happened_at", "window_start", "first_at", "last_at"} {
			delete(object, key)
		}
		data, err = json.Marshal(object)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	aClean, err := clean(a)
	if err != nil {
		return false, err
	}
	bClean, err := clean(b)
	if err != nil {
		return false, err
	}
	return string(aClean) == string(bClean), nil
}
