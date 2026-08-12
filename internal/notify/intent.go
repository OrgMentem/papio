// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Category string

const (
	CategoryRequestOutcome  Category = "request_outcome"
	CategoryDecisionOpened  Category = "decision_opened"
	CategoryDecisionPending Category = "decision_pending"
	CategoryCompletionBatch Category = "completion_batch"
	CategoryDiscoveryNew    Category = "discovery_new"
	CategoryIntegrityNotice Category = "integrity_notice"
	CategorySystemDegraded  Category = "system_degraded"
)

func Categories() []Category {
	return []Category{CategoryRequestOutcome, CategoryDecisionOpened, CategoryDecisionPending, CategoryCompletionBatch, CategoryDiscoveryNew, CategoryIntegrityNotice, CategorySystemDegraded}
}

type Phase string

const (
	PhaseTerminal   Phase = "terminal"
	PhaseOpened     Phase = "opened"
	PhaseReminder   Phase = "reminder"
	PhaseCheckpoint Phase = "checkpoint"
	PhaseFinal      Phase = "final"
	PhaseDigest     Phase = "digest"
	PhaseScan       Phase = "scan"
	PhaseEpisode    Phase = "episode"
)

func PhasesFor(category Category) []Phase {
	switch category {
	case CategoryRequestOutcome:
		return []Phase{PhaseTerminal}
	case CategoryDecisionOpened:
		return []Phase{PhaseOpened}
	case CategoryDecisionPending:
		return []Phase{PhaseReminder}
	case CategoryCompletionBatch:
		return []Phase{PhaseCheckpoint, PhaseFinal}
	case CategoryDiscoveryNew:
		return []Phase{PhaseDigest}
	case CategoryIntegrityNotice:
		return []Phase{PhaseScan}
	case CategorySystemDegraded:
		return []Phase{PhaseEpisode}
	default:
		return nil
	}
}

func validCategoryPhase(category Category, phase Phase) bool {
	for _, allowed := range PhasesFor(category) {
		if allowed == phase {
			return true
		}
	}
	return false
}

type Intent struct {
	EventKind    string
	Category     Category
	AggregateKey string
	Phase        Phase
	WindowStart  time.Time
	JobID        string
	BatchID      string
	ScanID       string
	HappenedAt   time.Time
	Message      string
	Detail       Event
}

type Sink interface {
	Route(context.Context, Intent) error
}

type PresenceProvider interface{ AnyFocused(now time.Time) bool }

type ActivityRecorder interface {
	RecordSystemEvent(context.Context, string, map[string]any) error
}

type Record struct {
	ID                                                        int64
	Intent                                                    Intent
	FirstAt, LastAt, AvailableAt                              time.Time
	Count                                                     int
	DesktopState, WebhookState                                string
	DesktopReservedAt, DesktopAttemptedAt, WebhookAttemptedAt time.Time
}

type RouteRow struct {
	Category Category
	Desktop  string
	Webhook  string
	Window   time.Duration
	Source   string
}

func validateIntent(intent Intent) error {
	if _, ok := map[Category]bool{
		CategoryRequestOutcome: true, CategoryDecisionOpened: true, CategoryDecisionPending: true,
		CategoryCompletionBatch: true, CategoryDiscoveryNew: true, CategoryIntegrityNotice: true,
		CategorySystemDegraded: true,
	}[intent.Category]; !ok {
		return fmt.Errorf("unknown notification category %q", intent.Category)
	}
	if !validCategoryPhase(intent.Category, intent.Phase) {
		return fmt.Errorf("phase %q is not allowed for category %q", intent.Phase, intent.Category)
	}
	if intent.EventKind == "" {
		return fmt.Errorf("notification event kind is required")
	}
	if intent.AggregateKey == "" {
		return fmt.Errorf("notification aggregate key is required")
	}
	if intent.WindowStart.IsZero() {
		return fmt.Errorf("notification window start is required")
	}
	if intent.HappenedAt.IsZero() {
		return fmt.Errorf("notification happened at is required")
	}
	return nil
}

// CategoryRepresentsOneEvent reports whether one notification in this category
// can only ever stand for a single durable event, so an aggregate count is not
// merely unusual but impossible.
//
// request_outcome is the terminal result of one explicitly submitted standalone
// request, and cohort members are summarized by completion_batch instead; its
// aggregate key is one job. system_degraded is one nameable state episode; its
// aggregate key is one episode. Both coalesce only with their own replays, so
// their copy carries no number at all.
func CategoryRepresentsOneEvent(category Category) bool {
	switch category {
	case CategoryRequestOutcome, CategorySystemDegraded:
		return true
	default:
		return false
	}
}

// ComposeMessage is the shared public-copy boundary for notification
// producers and operator preview/test commands. The router also applies it
// after coalescing so aggregate counts cannot leave stale singular copy.
//
// count is the number of durable domain facts the notification stands for —
// papers, watch hits, or library notices. Every form is composed for its exact
// count; this package never renders a "(s)" placeholder, and categories that
// always stand for one event ignore count entirely so a replayed intent
// coalescing into the same ledger row cannot inflate it.
func ComposeMessage(category Category, count int, detail Event, fallback string) string {
	if count < 1 {
		count = 1
	}
	if detail.Message != "" && category != CategoryDecisionOpened {
		return detail.Message
	}
	switch category {
	case CategoryRequestOutcome:
		// "Finished" reports that the request reached a terminal state without
		// claiming the paper is accessible; the inbox holds the real outcome.
		return "Request finished — open the papio inbox"
	case CategoryDecisionOpened:
		if count == 1 {
			return "1 paper needs you — open the papio inbox"
		}
		return fmt.Sprintf("%d papers need you — open the papio inbox", count)
	case CategoryDecisionPending:
		return DecisionPendingMessage(count, 0, nil)
	case CategoryCompletionBatch:
		// A cohort notice never calls an active cohort complete; producers add
		// the acquired/needs-you breakdown when they have one.
		if count == 1 {
			return "Batch update · 1 paper — open the papio inbox"
		}
		return fmt.Sprintf("Batch update · %d papers — open the papio inbox", count)
	case CategoryDiscoveryNew:
		if count == 1 {
			return "1 new watch hit — open the papio inbox"
		}
		return fmt.Sprintf("%d new watch hits — open the papio inbox", count)
	case CategoryIntegrityNotice:
		if count == 1 {
			return "1 library integrity notice — open the papio inbox"
		}
		return fmt.Sprintf("%d library integrity notices — open the papio inbox", count)
	case CategorySystemDegraded:
		// One notification is one state episode. Producers name the condition;
		// doctor is the recoverable surface when they cannot.
		return "papio cannot make progress — run: papio doctor"
	}
	return fallback
}

// ReminderClass is one recovery class in a pending-decision digest. Callers
// pass classes in their own priority order and the digest preserves it.
type ReminderClass struct {
	Name  string
	Count int
}

// DecisionPendingMessage composes the reminder digest copy shared by the
// action-reminder producer and notify preview/test. Classes are optional for
// callers that only have an aggregate count, and an unknown oldest age
// (oldestSeconds <= 0) omits the age clause instead of inventing one.
func DecisionPendingMessage(total int, oldestSeconds int64, classes []ReminderClass) string {
	if total < 1 {
		total = 1
	}
	subject := fmt.Sprintf("%d papers need your attention", total)
	if total == 1 {
		subject = "1 paper needs your attention"
	}
	clauses := make([]string, 0, 2)
	if len(classes) > 0 {
		names := make([]string, 0, len(classes))
		for _, class := range classes {
			names = append(names, fmt.Sprintf("%s: %d", class.Name, class.Count))
		}
		clauses = append(clauses, strings.Join(names, ", "))
	}
	if oldestSeconds > 0 {
		clauses = append(clauses, "oldest waiting "+reminderAge(oldestSeconds))
	}
	if len(clauses) == 0 {
		return subject + " — run: papio actions list"
	}
	return subject + " — " + strings.Join(clauses, "; ") + " — run: papio actions list"
}

// reminderAge renders a coarse waiting age using the same rounding the
// action-reminder producer has always used. It is called only for a known
// positive age.
func reminderAge(seconds int64) string {
	switch {
	case seconds >= 24*3600:
		return fmt.Sprintf("%dd", seconds/(24*3600))
	case seconds >= 3600:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		minutes := seconds / 60
		if minutes < 1 {
			minutes = 1
		}
		return fmt.Sprintf("%dm", minutes)
	}
}
