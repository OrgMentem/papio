// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"fmt"
	"sort"
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

// ComposeMessage is the shared public-copy boundary for notification
// producers and operator preview/test commands. The router also applies it
// after coalescing so aggregate counts cannot leave stale singular copy.
func ComposeMessage(category Category, count int, detail Event, fallback string) string {
	if count < 1 {
		count = 1
	}
	if detail.Message != "" && category != CategoryDecisionOpened {
		return detail.Message
	}
	switch category {
	case CategoryDecisionOpened:
		if count == 1 {
			return "1 paper needs you — open the papio inbox"
		}
		return fmt.Sprintf("%d papers need you — open the papio inbox", count)
	case CategoryRequestOutcome:
		if detail.Message != "" {
			return detail.Message
		}
		return fmt.Sprintf("%d request outcome(s) — open the papio inbox", count)
	case CategoryDecisionPending:
		if detail.Message != "" {
			return detail.Message
		}
		return DecisionPendingMessage(count, 0, nil)
	case CategoryCompletionBatch:
		if detail.Message != "" {
			return detail.Message
		}
		return fmt.Sprintf("%d papers in the batch reached a checkpoint — open the papio inbox", count)
	case CategoryDiscoveryNew:
		if detail.Message != "" {
			return detail.Message
		}
		return fmt.Sprintf("%d new works discovered — open the papio inbox", count)
	case CategoryIntegrityNotice:
		if detail.Message != "" {
			return detail.Message
		}
		return fmt.Sprintf("%d integrity notices — open the papio inbox", count)
	case CategorySystemDegraded:
		if detail.Message != "" {
			return detail.Message
		}
		return fmt.Sprintf("%d system conditions need attention — open the papio inbox", count)
	}
	return fallback
}

// DecisionPendingMessage composes the reminder copy shared by action-reminder
// producers and notify preview/test. Classes are optional for callers that
// only have an aggregate count; producer detail remains additive.
func DecisionPendingMessage(total int, oldestSeconds int64, classes map[string]int) string {
	if total < 1 {
		total = 1
	}
	age := "now"
	switch {
	case oldestSeconds >= 36*3600:
		age = fmt.Sprintf("%dd", (oldestSeconds+18*3600)/(36*3600))
	case oldestSeconds >= 90*60:
		age = fmt.Sprintf("%dh", (oldestSeconds+1800)/(3600))
	case oldestSeconds >= 90:
		age = fmt.Sprintf("%dm", (oldestSeconds+30)/60)
	case oldestSeconds > 0:
		age = fmt.Sprintf("%ds", oldestSeconds)
	}
	if len(classes) == 0 {
		return fmt.Sprintf("%d papers need your attention — oldest waiting %s — run: papio actions list", total, age)
	}
	names := make([]string, 0, len(classes))
	for name, count := range classes {
		names = append(names, fmt.Sprintf("%s: %d", name, count))
	}
	sort.Strings(names)
	return fmt.Sprintf("%d papers need your attention — %s; oldest waiting %s — run: papio actions list", total, strings.Join(names, ", "), age)
}
