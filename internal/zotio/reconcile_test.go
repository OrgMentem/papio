// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/job"
)

const (
	reconcilePreview = `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`
	// reconcileAttached answers an attachments-add apply on the existing item.
	reconcileAttached = `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`
	// reconcileCreated answers a manifest create.
	reconcileCreated  = `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"status":"applied","reason":{"via":"web","parent_key":"PA12RE34","attachment_key":"AT56CH90"}}]}}`
	reconcileManifest = `{"schema_version":2,"entries":[{"path":"paper.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"10.1002/example","status":"resolved","item":{"itemType":"journalArticle","title":"Example Paper","DOI":"10.1002/example"}}]}`
	reconcileFound    = `[{"key":"AB12CD34","data":{}}]`
)

func isUnconfirmed(err error) bool {
	return err != nil && ErrorInfoFrom(err).Class == ErrorClassZotioApplyUnconfirmed
}

func applyKey(plan *Plan) string {
	return "zotio_apply:" + plan.ID + ":" + plan.ConfirmationSHA256
}

func recordedStatus(t *testing.T, service *Service, plan *Plan) string {
	t.Helper()
	var raw string
	if err := service.Store.DB().QueryRow(
		`SELECT result_json FROM exports WHERE kind = 'zotio_apply' AND idempotency_key = ?`, applyKey(plan)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var result ApplyResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	return result.Status
}

func jobState(t *testing.T, service *Service, jobID string) string {
	t.Helper()
	row, err := service.Bundle.Jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return row.State
}

// cancelledApply plans a job and applies it with a context that dies during
// the zotio call, leaving the exports row ambiguous.
func cancelledApply(t *testing.T, service *Service, cli *planCLI, jobID string) *Plan {
	t.Helper()
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli.applyFn = func(ctx context.Context) (json.RawMessage, error) {
		cancel()
		return nil, ctx.Err()
	}
	if _, err := service.Apply(ctx, plan.ID, plan.ConfirmationSHA256); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled apply err = %v, want context.Canceled", err)
	}
	cli.applyFn = nil
	if status := recordedStatus(t, service, plan); status != "ambiguous" {
		t.Fatalf("cancelled apply status = %q, want ambiguous", status)
	}
	return plan
}

// The settle window measures from the interrupted attempt's claim, not from
// the expiry pass: the lease alone outlasts it, so the first pass after expiry
// reconciles instead of reporting unconfirmed for another settle window.
func TestExpiredApplyClaimSettlesFromClaimTime(t *testing.T) {
	cli := &planCLI{preview: reconcilePreview, apply: reconcileAttached}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	ctx := context.Background()
	if claimed, err := service.claimApply(ctx, applyKey(plan), jobID); err != nil || !claimed {
		t.Fatalf("initial claim = %t, %v", claimed, err)
	}
	advance(service, applyClaimLease+time.Minute)
	cli.missing = func() ([]MissingPDFItem, error) { return []MissingPDFItem{{Key: "AB12CD34"}}, nil }
	applied, err := service.Apply(ctx, plan.ID, plan.ConfirmationSHA256)
	if err != nil || applied.Status != "applied" || cli.applyCalls != 1 {
		t.Fatalf("first apply after expiry = %+v, %v applyCalls = %d, want one applied run", applied, err, cli.applyCalls)
	}
}

func advance(service *Service, d time.Duration) {
	base := service.Now()
	service.Now = func() time.Time { return base.Add(d) }
}

// An apply claim whose owner died is never reclaimed for a blind re-run. It
// becomes ambiguous, reconciliation waits for the library to show what the
// dead owner did, and only an empty, freshly synced library lets the plan run
// again - exactly once.
func TestExpiredApplyClaimIsReconciledNotRerun(t *testing.T) {
	cli := &planCLI{preview: reconcilePreview, apply: reconcileAttached}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	ctx := context.Background()
	if claimed, err := service.claimApply(ctx, applyKey(plan), jobID); err != nil || !claimed {
		t.Fatalf("initial claim = %t, %v", claimed, err)
	}
	advance(service, applyClaimLease+time.Minute)
	if claimed, err := service.claimApply(ctx, applyKey(plan), jobID); err != nil || claimed {
		t.Fatalf("claim of an expired reservation = %t, %v; want it never reclaimed", claimed, err)
	}

	// An unreadable queue cannot show what the dead owner did, so the
	// expired claim stays ambiguous and the mutation does not re-run.
	if _, err := service.Apply(ctx, plan.ID, plan.ConfirmationSHA256); !isUnconfirmed(err) {
		t.Fatalf("apply of an expired claim err = %v, want unconfirmed", err)
	}
	if cli.applyCalls != 0 {
		t.Fatalf("expired claim re-ran the mutation: applyCalls = %d", cli.applyCalls)
	}
	if status := recordedStatus(t, service, plan); status != "ambiguous" {
		t.Fatalf("expired claim status = %q, want ambiguous", status)
	}

	// Time passing does not make an unreadable queue evidence.
	advance(service, ambiguousSettleWindow+time.Minute)
	if _, err := service.Apply(ctx, plan.ID, plan.ConfirmationSHA256); !isUnconfirmed(err) || cli.applyCalls != 0 {
		t.Fatalf("apply with an unreadable library err = %v applyCalls = %d, want unconfirmed and no run", err, cli.applyCalls)
	}

	cli.missing = func() ([]MissingPDFItem, error) { return []MissingPDFItem{{Key: "AB12CD34"}}, nil }
	applied, err := service.Apply(ctx, plan.ID, plan.ConfirmationSHA256)
	if err != nil || applied.Status != "applied" || cli.applyCalls != 1 {
		t.Fatalf("apply after the settle window = %+v, %v applyCalls = %d, want one applied run", applied, err, cli.applyCalls)
	}
	if state := jobState(t, service, jobID); state != job.StateImported {
		t.Fatalf("job state = %s, want imported", state)
	}
	if replay, err := service.Apply(ctx, plan.ID, plan.ConfirmationSHA256); err != nil || replay.Status != "applied" || cli.applyCalls != 1 {
		t.Fatalf("replay = %+v, %v applyCalls = %d", replay, err, cli.applyCalls)
	}
}

func TestAmbiguousCreateReconcilesAgainstTheLibrary(t *testing.T) {
	newService := func(t *testing.T) (*Service, *planCLI, string) {
		cli := &planCLI{manifest: reconcileManifest, preview: reconcilePreview, apply: reconcileCreated}
		service, jobID := readyPlanService(t, "", cli)
		return service, cli, jobID
	}

	t.Run("empty library re-runs once after the settle window", func(t *testing.T) {
		service, cli, jobID := newService(t)
		plan := cancelledApply(t, service, cli, jobID)
		ctx := context.Background()
		if _, err := service.Apply(ctx, plan.ID, plan.ConfirmationSHA256); !isUnconfirmed(err) {
			t.Fatalf("apply inside the settle window err = %v, want unconfirmed", err)
		}
		if cli.applyCalls != 1 {
			t.Fatalf("apply inside the settle window re-ran: applyCalls = %d", cli.applyCalls)
		}
		advance(service, ambiguousSettleWindow+time.Minute)
		applied, err := service.Apply(ctx, plan.ID, plan.ConfirmationSHA256)
		if err != nil || applied.Status != "applied" || applied.ParentKey != "PA12RE34" || cli.applyCalls != 2 {
			t.Fatalf("apply after the window = %+v, %v applyCalls = %d, want one new applied run", applied, err, cli.applyCalls)
		}
		if state := jobState(t, service, jobID); state != job.StateImported {
			t.Fatalf("job state = %s, want imported", state)
		}
	})

	t.Run("item with its PDF is recorded without a run", func(t *testing.T) {
		service, cli, jobID := newService(t)
		plan := cancelledApply(t, service, cli, jobID)
		cli.found = reconcileFound
		cli.missing = func() ([]MissingPDFItem, error) { return nil, nil }
		result, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256)
		if err != nil || result.Status != "duplicate" || result.ParentKey != "AB12CD34" || cli.applyCalls != 1 {
			t.Fatalf("apply = %+v, %v applyCalls = %d, want a duplicate record and no new run", result, err, cli.applyCalls)
		}
		if state := jobState(t, service, jobID); state != job.StateImported {
			t.Fatalf("job state = %s, want imported", state)
		}
		if status := recordedStatus(t, service, plan); status != "duplicate" {
			t.Fatalf("recorded status = %q, want duplicate", status)
		}
	})

	t.Run("item without its PDF takes the attach route, not a second create", func(t *testing.T) {
		service, cli, jobID := newService(t)
		// The cancel lands during the first PlanAndApply.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cli.applyFn = func(ctx context.Context) (json.RawMessage, error) {
			cancel()
			return nil, ctx.Err()
		}
		if _, _, _, err := service.PlanAndApply(ctx, jobID); err == nil {
			t.Fatal("cancelled PlanAndApply returned no error")
		}
		cli.applyFn = nil
		cli.found = reconcileFound
		cli.missing = func() ([]MissingPDFItem, error) { return []MissingPDFItem{{Key: "AB12CD34"}}, nil }
		cli.apply = reconcileAttached
		status, parent, attachment, err := service.PlanAndApply(context.Background(), jobID)
		if err != nil || status != "applied" || parent != "AB12CD34" || attachment != "AT56CH90" {
			t.Fatalf("PlanAndApply = %q %q %q, %v; want the PDF attached to the saved item", status, parent, attachment, err)
		}
		if cli.applyCalls != 2 || !strings.Contains(strings.Join(cli.applyArgs, " "), "attachments add AB12CD34") {
			t.Fatalf("applyCalls = %d args = %v, want one attach run on AB12CD34 and no second create", cli.applyCalls, cli.applyArgs)
		}
		if state := jobState(t, service, jobID); state != job.StateImported {
			t.Fatalf("job state = %s, want imported", state)
		}
	})
}

// An older zotio that cannot report desktop presence can never supply the
// desktop evidence reconcile waits for — but an empty mirror still proves
// nothing, because a connector save may have succeeded locally before the
// timeout and not synced yet. The row holds past the settle window without
// reopening: a second create would duplicate the item.
func TestAmbiguousCreateStaysAmbiguousWhenDesktopPresenceIsUnsupported(t *testing.T) {
	cli := &planCLI{manifest: reconcileManifest, preview: reconcilePreview, apply: reconcileCreated}
	service, jobID := readyPlanService(t, "", cli)
	plan := cancelledApply(t, service, cli, jobID)
	cli.desktopUnsupported = true
	advance(service, ambiguousSettleWindow+time.Minute)
	if _, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256); !isUnconfirmed(err) {
		t.Fatalf("apply with unsupported presence err = %v, want unconfirmed", err)
	}
	if cli.applyCalls != 1 {
		t.Fatalf("apply re-ran the create without desktop evidence: applyCalls = %d", cli.applyCalls)
	}
	if status := recordedStatus(t, service, plan); status != "ambiguous" {
		t.Fatalf("recorded status = %q, want ambiguous", status)
	}
	if state := jobState(t, service, jobID); state == job.StateImported {
		t.Fatal("job imported although the earlier save was never confirmed")
	}
}

// The uncloseable gap must not hold forever either. Past the presence hold
// the row settles into a countable human-action error — the operator checks
// Zotero and upgrades zotio — instead of another unconfirmed outcome that
// retries every five minutes without spending the import budget.
func TestAmbiguousUnsupportedPresenceAgesIntoHumanAction(t *testing.T) {
	cli := &planCLI{manifest: reconcileManifest, preview: reconcilePreview, apply: reconcileCreated}
	service, jobID := readyPlanService(t, "", cli)
	plan := cancelledApply(t, service, cli, jobID)
	cli.desktopUnsupported = true
	advance(service, ambiguousPresenceHoldMax+time.Hour)
	_, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256)
	if err == nil || isUnconfirmed(err) {
		t.Fatalf("apply past the presence hold err = %v, want a countable human-action error", err)
	}
	// The app's retry pass skips only the interrupted-apply classes when it
	// counts the import budget; any other class spends an attempt, which is
	// what ends the five-minute loop.
	if class := ErrorInfoFrom(err).Class; class == ErrorClassZotioApplyUnconfirmed || class == ErrorClassReservationConflict {
		t.Fatalf("human-action error class = %q, want a class that spends the import budget", class)
	}
	// The stored hint is what the event and the batch report show, and it is
	// cut at maxErrorHintBytes: the instruction must survive whole.
	service.recordAutoImportEvent(context.Background(), jobID, "failed", "", "", err)
	detail, _ := latestZotioEvent(t, service, jobID, "zotio.auto_import")
	hint, _ := detail["error_hint"].(string)
	if len(hint) > maxErrorHintBytes {
		t.Fatalf("stored hint is %d bytes, over the %d-byte cap: %q", len(hint), maxErrorHintBytes, hint)
	}
	for _, want := range []string{"check Zotero", "upgrade zotio"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("stored hint = %q, want it to name %q", hint, want)
		}
	}
	if class, _ := detail["error_class"].(string); class == ErrorClassZotioApplyUnconfirmed || class == ErrorClassReservationConflict {
		t.Fatalf("stored error_class = %q, want a class that spends the import budget", class)
	}
	if cli.applyCalls != 1 {
		t.Fatalf("settling the hold re-ran the create: applyCalls = %d", cli.applyCalls)
	}
	if status := recordedStatus(t, service, plan); status != "ambiguous" {
		t.Fatalf("recorded status = %q, want the row left ambiguous", status)
	}
	if state := jobState(t, service, jobID); state == job.StateImported {
		t.Fatal("job imported although the earlier save was never confirmed")
	}
}

func TestAmbiguousCreateStaysUnconfirmedWithoutDesktopEvidence(t *testing.T) {
	cli := &planCLI{manifest: reconcileManifest, preview: reconcilePreview, apply: reconcileCreated}
	service, jobID := readyPlanService(t, "", cli)
	plan := cancelledApply(t, service, cli, jobID)
	cli.desktopDown = true
	advance(service, ambiguousSettleWindow+time.Minute)
	if _, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256); !isUnconfirmed(err) {
		t.Fatalf("apply with the desktop down err = %v, want unconfirmed", err)
	}
	if cli.applyCalls != 1 {
		t.Fatalf("apply re-ran the create without desktop evidence: applyCalls = %d", cli.applyCalls)
	}
	if status := recordedStatus(t, service, plan); status != "ambiguous" {
		t.Fatalf("recorded status = %q, want ambiguous", status)
	}
}

// A deleted parent is absent from the missing-PDF queue like an item that
// holds its PDF. The job must not be imported on that inference.
func TestAmbiguousAttachDoesNotInferPDFForDeletedParent(t *testing.T) {
	cli := &planCLI{preview: reconcilePreview, apply: reconcileAttached}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	plan := cancelledApply(t, service, cli, jobID)
	cli.missing = func() ([]MissingPDFItem, error) { return nil, nil }
	cli.parentGone = true
	if _, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256); !isUnconfirmed(err) {
		t.Fatalf("apply for a deleted parent err = %v, want unconfirmed", err)
	}
	if state := jobState(t, service, jobID); state == job.StateImported {
		t.Fatal("job imported although its parent item no longer exists")
	}
	if status := recordedStatus(t, service, plan); status != "ambiguous" {
		t.Fatalf("recorded status = %q, want ambiguous", status)
	}
}

// An apply that reports nothing readable - zotio killed by a signal - may have
// saved the item. The plan stays and a second pass must not create again.
func TestUnreadableApplyResultKeepsPlanAndBlocksSecondCreate(t *testing.T) {
	cli := &planCLI{manifest: reconcileManifest, preview: reconcilePreview, apply: reconcileCreated}
	cli.applyFn = func(context.Context) (json.RawMessage, error) {
		return nil, errors.New("zotio import: signal: killed")
	}
	service, jobID := readyPlanService(t, "", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	if _, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256); err == nil {
		t.Fatal("killed apply returned no error")
	}
	if status := recordedStatus(t, service, plan); status != "ambiguous" {
		t.Fatalf("killed apply status = %q, want ambiguous", status)
	}
	if _, err := os.Stat(filepath.Join(service.DataDir, "zotio", "plans", plan.ID+".json")); err != nil {
		t.Fatalf("killed apply dropped its plan: %v", err)
	}
	if _, _, _, err := service.PlanAndApply(context.Background(), jobID); !isUnconfirmed(err) {
		t.Fatalf("second PlanAndApply err = %v, want unconfirmed", err)
	}
	if cli.applyCalls != 1 {
		t.Fatalf("second PlanAndApply created again: applyCalls = %d, want 1", cli.applyCalls)
	}
}

// A zotio that stopped of its own accord with no result changed nothing: the
// plan is dropped so the next pass derives a fresh one.
func TestDefiniteApplyFailureWithoutResultStillInvalidatesPlan(t *testing.T) {
	cli := &planCLI{manifest: reconcileManifest, preview: reconcilePreview}
	cli.applyFn = func(context.Context) (json.RawMessage, error) {
		return nil, &definiteFailure{errors.New("zotio import apply: refused")}
	}
	service, jobID := readyPlanService(t, "", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	if _, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256); err == nil {
		t.Fatal("refused apply returned no error")
	}
	if status := recordedStatus(t, service, plan); status != "failed" {
		t.Fatalf("refused apply status = %q, want failed", status)
	}
	if _, err := os.Stat(filepath.Join(service.DataDir, "zotio", "plans", plan.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("refused apply kept its plan: %v", err)
	}
}

// zotio finished and printed its envelope, then the daemon began shutting
// down. The import is already in Zotero, so the record and the job's
// transition must still land.
func TestApplyRecordsResultWhenCallerIsCancelledAfterSuccess(t *testing.T) {
	cli := &planCLI{preview: reconcilePreview, apply: reconcileAttached}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cli.applyFn = func(context.Context) (json.RawMessage, error) {
		cancel()
		return json.RawMessage(reconcileAttached), nil
	}
	result, err := service.Apply(ctx, plan.ID, plan.ConfirmationSHA256)
	if err != nil || result.Status != "applied" {
		t.Fatalf("apply = %+v, %v; want the applied result", result, err)
	}
	if status := recordedStatus(t, service, plan); status != "applied" {
		t.Fatalf("recorded status = %q, want applied", status)
	}
	if state := jobState(t, service, jobID); state != job.StateImported {
		t.Fatalf("job state = %s, want imported", state)
	}
}
