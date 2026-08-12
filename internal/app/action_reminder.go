// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"papio/internal/job"
	"papio/internal/notify"
)

const (
	actionReminderEvent       = "action.reminder"
	defaultActionReminderWait = 30 * time.Minute
	maxActionReminderWait     = 24 * time.Hour
)

// ActionReminder re-notifies people about open human actions before expiring
// handoffs and pending reviews are forgotten. Its durable event state prevents
// a daemon restart from resetting the backoff and turning routine maintenance
// into notification spam.
type ActionReminder struct {
	svc *Service
}

// ActionReminder returns a maintenance runner that reminds people about open
// actions. It satisfies daemon.MaintenanceRunner without importing that package.
func (s *Service) ActionReminder() *ActionReminder { return &ActionReminder{svc: s} }

// reminderActionKindDefaultOK is the disposition record for named action
// kinds whose reminder behavior intentionally uses the generic review bucket.
// A kind belongs here only after its reminder behavior has been considered;
// adding an ActionKind requires either a real reminder path or an entry here.
var reminderActionKindDefaultOK = map[string]struct{}{
	job.ActionKindDocumentDelivery:        {},
	job.ActionKindDownloadsAccessRequired: {},
}

// RunDue performs one bounded reminder pass over active human-action jobs.
//
// Each reminder is recorded before delivery because a failed process restart
// must not repeat a notice that was already handed to the configured sinks.
// The event also gives each action its own exponentially growing schedule, so
// a long-lived handoff remains visible without becoming a fixed-rate alert.
func (r *ActionReminder) RunDue(ctx context.Context) error {
	if r == nil || r.svc == nil || r.svc.Jobs == nil || r.svc.Notifier == nil {
		return nil
	}
	s := r.svc
	rows, err := s.Jobs.ListOldest(ctx, []string{job.StateAwaitingHuman, job.StateNeedsReview}, job.ListLimitMax)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	actions, err := s.Jobs.ListOpenHumanActionsForJobs(ctx, ids)
	if err != nil {
		return err
	}

	now := r.now()
	base := actionReminderWait(s.Config.Browser.ActionExpirySeconds)
	eventsByJob := make(map[string][]map[string]any)
	eventErrs := make(map[string]error)
	loadedEvents := make(map[string]bool)
	waiting := [actionReminderClassCount]actionReminderBatch{}
	due := [actionReminderClassCount]bool{}
	window := 4 * time.Hour
	if policy, policyErr := notify.ResolvePolicy(s.Config.Notify); policyErr == nil {
		if configured := policy.For(notify.CategoryDecisionPending).Window; configured > 0 {
			window = configured
		}
	}
	var digestWindow time.Time
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	loadEvents := func(jobID string) ([]map[string]any, error) {
		if loadedEvents[jobID] {
			return eventsByJob[jobID], eventErrs[jobID]
		}
		loadedEvents[jobID] = true
		events, err := s.Jobs.Events(ctx, jobID)
		if err != nil {
			eventErrs[jobID] = err
			return nil, err
		}
		eventsByJob[jobID] = events
		return events, nil
	}

	for _, action := range actions {
		createdAt, err := time.Parse(time.RFC3339Nano, action.CreatedAt)
		if err != nil {
			record(fmt.Errorf("parse creation time for human action %d: %w", action.ID, err))
			continue
		}
		age := now.Sub(createdAt)
		if age < base {
			continue
		}
		if action.Quiesced(now) {
			continue
		}
		events, err := loadEvents(action.JobID)
		if err != nil {
			record(fmt.Errorf("load events for human action %d: %w", action.ID, err))
			continue
		}
		state, found := latestActionReminder(events, action.ID)
		if found && state.RemindedAt.After(now) {
			state.RemindedAt = now.Add(-actionReminderBackoff(base, state.Count))
		}
		dueAt := createdAt.Add(base)
		if found {
			dueAt = state.RemindedAt.Add(actionReminderBackoff(base, state.Count))
		}
		if found && now.Before(dueAt) {
			continue
		}
		count := state.Count + 1
		windowStart := dueAt.UTC().Truncate(window)
		if digestWindow.IsZero() || windowStart.Before(digestWindow) {
			digestWindow = windowStart
		}
		if err := s.Jobs.RecordEvent(ctx, action.JobID, actionReminderEvent, map[string]any{
			"action_id": action.ID, "count": count,
			"age_seconds":  int64(age / time.Second),
			"reminded_at":  now.UTC().Format(time.RFC3339Nano),
			"window_start": windowStart.Format(time.RFC3339Nano),
		}); err != nil {
			record(err)
			continue
		}
		class := reminderBatchIndex(action)
		waiting[class].add(action.JobID, age)
		due[class] = true
	}

	total := 0
	for class := actionReminderClass(0); class < actionReminderClassCount; class++ {
		if due[class] {
			total += len(waiting[class].jobs)
		}
	}
	if total > 0 {
		message := digestReminderMessage(waiting, due)
		eventDetail := map[string]any{"classes": map[string]int{}, "oldest_age_seconds": int64(0)}
		var oldest time.Duration
		for class := actionReminderClass(0); class < actionReminderClassCount; class++ {
			if !due[class] {
				continue
			}
			eventDetail["classes"].(map[string]int)[reminderClassName(class)] = len(waiting[class].jobs)
			if waiting[class].oldestAge > oldest {
				oldest = waiting[class].oldestAge
			}
		}
		eventDetail["oldest_age_seconds"] = int64(oldest / time.Second)
		event := notify.Event{Kind: actionReminderEvent, Message: message, Count: total, Detail: eventDetail}
		if digestWindow.IsZero() {
			digestWindow = now.UTC().Truncate(window)
		}
		intent := notify.Intent{
			EventKind: actionReminderEvent, Category: notify.CategoryDecisionPending,
			AggregateKey: "actions:pending", Phase: notify.PhaseReminder,
			WindowStart: digestWindow, HappenedAt: now.UTC(),
			Message: message, Detail: event,
		}
		if err := s.Notifier.Route(context.WithoutCancel(ctx), intent); err != nil {
			log.Printf("papio: routing action reminder: %v", err)
		}
	}
	return firstErr
}

type actionReminderState struct {
	Count      int
	RemindedAt time.Time
}

// latestActionReminder returns the most recent complete reminder state for one
// action. A partial event is ignored so an interrupted old write cannot pin an
// action behind an unreadable schedule forever.
func latestActionReminder(events []map[string]any, actionID int64) (actionReminderState, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event["kind"] != actionReminderEvent {
			continue
		}
		detail, ok := event["detail"].(map[string]any)
		if !ok || eventInt64(detail["action_id"]) != actionID {
			continue
		}
		count := eventInt(detail["count"])
		if count <= 0 {
			continue
		}
		remindedAt, ok := detail["reminded_at"].(string)
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, remindedAt)
		if err != nil {
			continue
		}
		return actionReminderState{Count: count, RemindedAt: at}, true
	}
	return actionReminderState{}, false
}

func eventInt64(value any) int64 {
	switch value := value.(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return 0
	}
}

func eventInt(value any) int {
	switch value := value.(type) {
	case float64:
		return int(value)
	case int64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

func actionReminderWait(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultActionReminderWait
	}
	return time.Duration(seconds) * time.Second
}

func actionReminderBackoff(base time.Duration, reminderCount int) time.Duration {
	wait := base
	for range reminderCount {
		if wait >= maxActionReminderWait/2 {
			return maxActionReminderWait
		}
		wait *= 2
	}
	return wait
}

type actionReminderClass int

const (
	institutionalReminder actionReminderClass = iota
	openHandoffReminder
	manualDownloadAfterLoginReminder
	manualDownloadReminder
	loginReminder
	reviewReminder
	actionReminderClassCount
)

type actionReminderBatch struct {
	jobs      map[string]struct{}
	oldestAge time.Duration
}

func (b *actionReminderBatch) add(jobID string, age time.Duration) {
	if b.jobs == nil {
		b.jobs = make(map[string]struct{})
	}
	b.jobs[jobID] = struct{}{}
	if age > b.oldestAge {
		b.oldestAge = age
	}
}

func reminderBatchIndex(action job.HumanAction) actionReminderClass {
	next := HumanActionNextStepFor(action)
	switch {
	case next.Instruction != "" && next.RequiresInstitutionalLogin:
		return manualDownloadAfterLoginReminder
	case next.Instruction != "":
		return manualDownloadReminder
	case next.Command == actionsOpenCommand && next.RequiresInstitutionalLogin:
		return institutionalReminder
	case next.Command == actionsOpenCommand:
		return openHandoffReminder
	case next.RequiresInstitutionalLogin:
		return loginReminder
	default:
		if _, ok := reminderActionKindDefaultOK[action.Kind]; ok {
			return reviewReminder
		}
		// Unknown kinds remain in the generic bucket until coverage requires
		// an explicit reminder disposition.
		return reviewReminder
	}
}

func (b actionReminderBatch) message(class actionReminderClass) string {
	count := len(b.jobs)
	paper, verb := "paper", "has"
	if count != 1 {
		paper, verb = "papers", "have"
	}
	recovery, command := "for your review", "papio actions list"
	switch class {
	case institutionalReminder:
		recovery, command = "for your institution sign-in", actionsOpenCommand
	case openHandoffReminder:
		recovery, command = "for you to open it", actionsOpenCommand
		if count != 1 {
			recovery = "for you to open them"
		}
	case manualDownloadAfterLoginReminder:
		recovery, command = manualDownloadReminderRecovery(count, true), ""
	case manualDownloadReminder:
		recovery, command = manualDownloadReminderRecovery(count, false), ""
	case loginReminder:
		recovery, command = "for you to sign in to your institution", ""
	}
	if command == "" {
		return fmt.Sprintf("%d %s %s been waiting %s %s", count, paper, verb, actionReminderAge(b.oldestAge), recovery)
	}
	return fmt.Sprintf("%d %s %s been waiting %s %s — run: %s", count, paper, verb, actionReminderAge(b.oldestAge), recovery, command)
}
func digestReminderMessage(waiting [actionReminderClassCount]actionReminderBatch, due [actionReminderClassCount]bool) string {
	classes := make([]string, 0, actionReminderClassCount)
	total := 0
	var oldest time.Duration
	var only actionReminderClass
	classCount := 0
	for class := actionReminderClass(0); class < actionReminderClassCount; class++ {
		if !due[class] {
			continue
		}
		count := len(waiting[class].jobs)
		total += count
		if waiting[class].oldestAge > oldest {
			oldest = waiting[class].oldestAge
		}
		only = class
		classCount++
		classes = append(classes, fmt.Sprintf("%s: %d", reminderClassName(class), count))
	}
	if classCount == 1 {
		return waiting[only].message(only)
	}
	return fmt.Sprintf("%d papers need your attention — %s; oldest waiting %s — run: papio actions list",
		total, strings.Join(classes, ", "), actionReminderAge(oldest))
}

func reminderClassName(class actionReminderClass) string {
	switch class {
	case institutionalReminder:
		return "institution sign-in"
	case openHandoffReminder:
		return "open handoff"
	case manualDownloadAfterLoginReminder:
		return "download after sign-in"
	case manualDownloadReminder:
		return "manual download"
	case loginReminder:
		return "sign-in"
	default:
		return "review"
	}
}

func manualDownloadReminderRecovery(count int, requiresInstitutionalLogin bool) string {
	if count == 1 {
		if requiresInstitutionalLogin {
			return "for you to sign in to your institution, then download the PDF yourself — papio will adopt it"
		}
		return "for you to download the PDF yourself — papio will adopt it"
	}
	if requiresInstitutionalLogin {
		return "for you to sign in to your institution, then download the PDFs yourself — papio will adopt them"
	}
	return "for you to download the PDFs yourself — papio will adopt them"
}

func actionReminderAge(age time.Duration) string {
	if age < time.Hour {
		minutes := int(age / time.Minute)
		if minutes < 1 {
			minutes = 1
		}
		return fmt.Sprintf("%dm", minutes)
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
}

func (r *ActionReminder) now() time.Time {
	if r != nil && r.svc != nil && r.svc.Now != nil {
		return r.svc.Now()
	}
	return time.Now()
}
