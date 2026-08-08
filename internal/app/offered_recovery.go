// Copyright 2026 OrgMentem. Licensed under MIT.
package app

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"papio/internal/delivery"
	"papio/internal/illiad"
	"papio/internal/job"
)

const (
	offeredRecoveryBatchSize      = 100
	offeredRecoveryMaxAttempts    = 3
	offeredRecoveryInitialBackoff = time.Minute
	offeredRecoveryPassTimeout    = 45 * time.Second
	offeredRecoveryCallTimeout    = 10 * time.Second
	offeredRecoveryLease          = 30 * time.Second
	offeredRecoveryOwnerPrefix    = "maintenance-offered-delivery-recovery"
)

var offeredRecoveryRunID atomic.Uint64

// OfferedDeliveryRecovery retries durable offered rows through the ordinary
// deliveryRoute gate. It is a daemon maintenance runner: startup and the
// scheduler's existing maintenance cadence both call RunDue.
type OfferedDeliveryRecovery struct {
	svc   *Service
	owner string
}

// OfferedDeliveryRecovery returns the offered-row maintenance runner.
func (s *Service) OfferedDeliveryRecovery() *OfferedDeliveryRecovery {
	runID := offeredRecoveryRunID.Add(1)
	owner := offeredRecoveryOwnerPrefix + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(runID, 10)
	return &OfferedDeliveryRecovery{svc: s, owner: owner}
}

// RunDue performs one bounded, best-effort recovery pass. Attempt evidence is
// stored in the job's append-only events stream, so the cap and backoff survive
// daemon restarts without another delivery_requests column or migration.
func (r *OfferedDeliveryRecovery) RunDue(ctx context.Context) error {
	if r == nil || r.svc == nil || r.svc.Delivery == nil || r.svc.Jobs == nil {
		return nil
	}
	passCtx, cancel := context.WithTimeout(ctx, offeredRecoveryPassTimeout)
	defer cancel()
	var cursor int64
	for {
		if err := passCtx.Err(); err != nil {
			return nil
		}
		rows, err := r.svc.Delivery.ListRecoverableAfter(passCtx, cursor, offeredRecoveryBatchSize)
		if err != nil {
			if passCtx.Err() != nil {
				return nil
			}
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, request := range rows {
			cursor = request.ID
			callCtx, callCancel := context.WithTimeout(passCtx, offeredRecoveryCallTimeout)
			err := r.recoverOne(callCtx, request)
			callCancel()
			if err != nil {
				log.Printf("papio: offered delivery recovery row %d failed: %v", request.ID, err)
			}
		}
		if len(rows) < offeredRecoveryBatchSize {
			return nil
		}
	}
}

func (r *OfferedDeliveryRecovery) recoverOne(ctx context.Context, request *delivery.Request) error {
	row, err := r.svc.Jobs.Get(ctx, request.JobID)
	if err != nil {
		return err
	}
	if row == nil {
		log.Printf("papio: offered delivery recovery row %d skipped: job %q is missing", request.ID, request.JobID)
		return nil
	}
	attempts, lastAttempt, err := r.attemptState(ctx, row.ID, request.ID)
	if err != nil {
		return err
	}
	createdAuto, err := r.createdAutoCapable(ctx, row.ID, request)
	if err != nil {
		return err
	}
	if !createdAuto {
		return nil
	}
	profileName := deliveryProfileName(row.Policy.Resolver)
	claimed, err := r.svc.Jobs.Claim(ctx, row.ID, r.owner, offeredRecoveryLease)
	if err != nil {
		return err
	}
	if claimed == nil {
		return nil
	}
	row = claimed
	defer func() { _ = r.svc.Jobs.Release(context.Background(), row.ID, r.owner) }()

	if attempts >= offeredRecoveryMaxAttempts {
		log.Printf("papio: offered delivery recovery row %d capped after %d attempts; opening human reconciliation", request.ID, attempts)
		return r.holdForHuman(ctx, row, request, "recovery attempts exhausted")
	}
	if profileName != request.InstitutionProfile {
		log.Printf("papio: offered delivery recovery row %d profile mismatch job=%q request=%q; opening human reconciliation", request.ID, profileName, request.InstitutionProfile)
		return r.holdForHuman(ctx, row, request, "job/request profile mismatch")
	}
	profile, err := r.svc.Delivery.ResolveGateProfileFor(ctx, profileName)
	if err != nil {
		return err
	}
	if profile.Digest() != request.GateProfileDigest {
		if err := r.holdForHuman(ctx, row, request, "stale:"+profile.Digest()); err != nil {
			log.Printf("papio: offered delivery recovery row %d hold failed: %v", request.ID, err)
			return err
		}
		log.Printf("papio: offered delivery recovery row %d held for human: gate profile digest changed", request.ID)
		return nil
	}
	class, classified, err := r.svc.submissionFailureClass(ctx, row.ID, request.ID)
	if err != nil {
		return err
	}
	if !classified || class != illiad.FailurePreSend {
		if err := r.holdForHuman(ctx, row, request, "ambiguous provider outcome"); err != nil {
			return err
		}
		log.Printf("papio: offered delivery recovery row %d held for human: provider outcome is ambiguous", request.ID)
		return nil
	}
	now := r.now()
	if !lastAttempt.IsZero() && now.Before(lastAttempt.Add(offeredRecoveryBackoff(attempts))) {
		return nil
	}
	_, dd, configured := r.svc.deliveryConfigured(row)
	if !configured {
		if err := r.holdForHuman(ctx, row, request, "delivery configuration missing"); err != nil {
			return err
		}
		log.Printf("papio: offered delivery recovery row %d held for human: delivery configuration missing", request.ID)
		return nil
	}
	if dd.Kind != request.Provider {
		if err := r.holdForHuman(ctx, row, request, "provider kind changed"); err != nil {
			return err
		}
		log.Printf("papio: offered delivery recovery row %d held for human: provider kind changed", request.ID)
		return nil
	}
	from := row.State
	if from == job.StateQueued || from == job.StateRetryWait {
		if err := r.svc.Jobs.Transition(ctx, row.ID, from, job.StateResolving, map[string]any{
			"reason":              "offered_delivery_recovery",
			"delivery_request_id": request.ID,
		}); err != nil {
			return err
		}
		from = job.StateResolving
	}
	if from != job.StateResolving {
		return nil
	}
	if err := r.svc.Jobs.RecordEvent(ctx, row.ID, "delivery.offered_recovery_attempt", map[string]any{
		"delivery_request_id": request.ID,
		"attempt":             attempts + 1,
		"attempted_at":        now.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return err
	}
	log.Printf("papio: offered delivery recovery row %d attempt %d", request.ID, attempts+1)
	result, err := r.svc.deliveryRoute(ctx, row, from)
	outcome := map[string]any{"delivery_request_id": request.ID, "attempt": attempts + 1}
	if err != nil {
		outcome["outcome"], outcome["error"] = "error", err.Error()
	} else if result.Request != nil {
		outcome["outcome"] = string(result.Request.State)

	} else {
		outcome["outcome"] = "no_request"
	}
	if eventErr := r.svc.Jobs.RecordEvent(ctx, row.ID, "delivery.offered_recovery_outcome", outcome); eventErr != nil && err == nil {
		err = eventErr
	}
	if err != nil {
		log.Printf("papio: offered delivery recovery row %d attempt %d failed: %v", request.ID, attempts+1, err)
	} else {
		log.Printf("papio: offered delivery recovery row %d attempt %d outcome %s", request.ID, attempts+1, outcome["outcome"])
	}
	return err
}
func (r *OfferedDeliveryRecovery) createdAutoCapable(ctx context.Context, jobID string, request *delivery.Request) (bool, error) {
	event, err := r.svc.Delivery.LatestGateEvent(ctx, jobID)
	if err != nil || event == nil {
		return false, err
	}
	return event.ProfileClass == delivery.GateClassAutoCapable &&
		event.ProfileDigest == request.GateProfileDigest &&
		event.Decision.Action == delivery.ActionSubmit, nil
}
func (r *OfferedDeliveryRecovery) holdForHuman(ctx context.Context, row *job.Row, request *delivery.Request, marker string) error {
	actions, err := r.svc.Jobs.ListOpenHumanActionsForJobs(ctx, []string{row.ID})
	if err != nil {
		return err
	}
	hasDeliveryAction := false
	for _, action := range actions {
		if action.Kind == job.ActionKindDocumentDelivery {
			hasDeliveryAction = true
			break
		}
	}
	if hasDeliveryAction {
		if marker == "ambiguous provider outcome" {
			return nil
		}
		if row.State == job.StateAwaitingHuman {
			return nil
		}
		events, eventErr := r.svc.Jobs.Events(ctx, row.ID)
		if eventErr != nil {
			return eventErr
		}
		for _, event := range events {
			if event["kind"] != "delivery.offered_recovery_hold" {
				continue
			}
			detail, _ := event["detail"].(map[string]any)
			if detail["marker"] == marker {
				return nil
			}
		}
	}
	if row.State == job.StateQueued || row.State == job.StateRetryWait {
		if err := r.svc.Jobs.Transition(ctx, row.ID, row.State, job.StateResolving, map[string]any{
			"reason":              "offered_delivery_recovery_hold",
			"delivery_request_id": request.ID,
		}); err != nil {
			return err
		}
		row.State = job.StateResolving
	}
	if row.State != job.StateResolving && row.State != job.StateAwaitingHuman {
		return nil
	}
	if _, err := r.svc.Jobs.OpenHumanAction(ctx, row.ID, job.ActionKindDocumentDelivery,
		DeliveryReconciliationActionDetail(request), job.Access(false, "")); err != nil {
		return err
	}
	if err := r.svc.Jobs.RecordEvent(ctx, row.ID, "delivery.offered_recovery_hold", map[string]any{
		"delivery_request_id": request.ID,
		"marker":              marker,
	}); err != nil {
		return err
	}
	if row.State == job.StateResolving {
		return r.svc.Jobs.Transition(ctx, row.ID, row.State, job.StateAwaitingHuman,
			map[string]any{"reason": "offered_delivery_recovery_hold"})
	}
	return nil
}

func (r *OfferedDeliveryRecovery) attemptState(ctx context.Context, jobID string, requestID int64) (int, time.Time, error) {
	events, err := r.svc.Jobs.Events(ctx, jobID)
	if err != nil {
		return 0, time.Time{}, err
	}
	count := 0
	var last time.Time
	for _, event := range events {
		if event["kind"] != "delivery.offered_recovery_attempt" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		deliveryID, _ := detail["delivery_request_id"].(float64)
		if int64(deliveryID) != requestID {
			continue
		}
		count++
		at, _ := detail["attempted_at"].(string)
		if parsed, parseErr := time.Parse(time.RFC3339Nano, at); parseErr == nil && parsed.After(last) {
			last = parsed
		}
	}
	return count, last, nil
}

func (r *OfferedDeliveryRecovery) now() time.Time {
	if r != nil && r.svc != nil && r.svc.Now != nil {
		return r.svc.Now()
	}
	return time.Now()
}

func offeredRecoveryBackoff(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	return offeredRecoveryInitialBackoff << (attempts - 1)
}
