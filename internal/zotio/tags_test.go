// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/work"
)

// tagCLI records one-key tag mutations and models exact-key missing-PDF reads.
type tagCLI struct {
	version       string
	preflights    int
	calls         [][]string
	failAdds      bool
	resolved      map[string]bool
	existingTypes map[string]int
	failKeys      map[string]error
	resultErrors  map[string]error
	removeNoOp    bool
	mu            sync.Mutex
}

func (f *tagCLI) Preflight(context.Context) (*PreflightResult, error) {
	f.preflights++
	version := f.version
	if version == "" {
		version = tagsMinimumZotioVersion
	}
	return &PreflightResult{Executable: "zotio", Version: version}, nil
}
func (f *tagCLI) MissingPDF(context.Context, string, int) ([]MissingPDFItem, error) {
	return nil, fmt.Errorf("not used")
}
func (f *tagCLI) MissingPDFKeys(_ context.Context, keys []string) ([]MissingPDFItem, error) {
	items := make([]MissingPDFItem, 0, len(keys))
	for _, key := range keys {
		if !f.resolved[key] {
			items = append(items, MissingPDFItem{Key: key})
		}
	}
	return items, nil
}
func (f *tagCLI) GetItem(context.Context, string) (*Item, error) { return nil, fmt.Errorf("not used") }
func (f *tagCLI) Sync(context.Context) error                     { return nil }
func (f *tagCLI) RunJSON(_ context.Context, args ...string) (json.RawMessage, error) {
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "items tags add") && !strings.Contains(joined, "items tags remove") {
		return nil, fmt.Errorf("unexpected RunJSON %q", args)
	}
	key := args[len(args)-1]
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()
	if err := f.failKeys[key]; err != nil {
		return nil, err
	}
	if f.failAdds && strings.Contains(joined, "items tags add") {
		return nil, fmt.Errorf("simulated zotio failure")
	}
	status := "applied"
	var reason any
	if tagType, exists := f.existingTypes[key]; exists && strings.Contains(joined, "items tags add") {
		status = "no_op"
		tag := args[len(args)-2]
		reason = map[string]any{"tag_types": map[string]int{tag: tagType}}
	}
	if f.removeNoOp && strings.Contains(joined, "items tags remove") {
		status = "no_op"
		reason = "tag not present"
	}
	raw, _ := json.Marshal(map[string]any{
		"result": map[string]any{"items": []map[string]any{{"key": key, "status": status, "reason": reason}}},
	})
	return raw, f.resultErrors[key]
}

type slowTagCLI struct {
	*tagCLI
	active atomic.Int32
	max    atomic.Int32
}

func (f *slowTagCLI) RunJSON(ctx context.Context, args ...string) (json.RawMessage, error) {
	active := f.active.Add(1)
	for {
		seen := f.max.Load()
		if active <= seen || f.max.CompareAndSwap(seen, active) {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
	defer f.active.Add(-1)
	return f.tagCLI.RunJSON(ctx, args...)
}

func tagTestService(t *testing.T, cli CLI) (*Service, *job.Store) {
	t.Helper()
	db, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Service{CLI: cli, Store: db, ExceptionTags: true}, &job.Store{S: db}
}

func createJob(t *testing.T, jobs *job.Store, requestID, itemKey string, states ...string) string {
	t.Helper()
	ctx := context.Background()
	jobID, err := jobs.CreateRequest(ctx, requestID, work.Work{
		DOI: "10.1000/" + strings.ToLower(itemKey), Title: "Paper " + itemKey,
	}, itemKey, "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.S.DB().ExecContext(ctx,
		`INSERT INTO zotio_item_scope(item_key, scope, observed_at) VALUES (?, 'personal', ?) ON CONFLICT(item_key) DO NOTHING`,
		itemKey, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	from := job.StateQueued
	for _, to := range states {
		if err := jobs.Transition(ctx, jobID, from, to, nil); err != nil {
			t.Fatalf("transition %s -> %s: %v", from, to, err)
		}
		from = to
	}
	return jobID
}

func tagStateRows(t *testing.T, s *Service) map[string]string {
	t.Helper()
	rows, err := s.Store.DB().Query(`SELECT item_key, tag FROM zotio_tag_state`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, tag string
		if err := rows.Scan(&key, &tag); err != nil {
			t.Fatal(err)
		}
		out[key] = tag
	}
	return out
}

func tagStateStatus(t *testing.T, s *Service, key string) (string, string) {
	t.Helper()
	var tag, status string
	if err := s.Store.DB().QueryRow(`SELECT tag, status FROM zotio_tag_state WHERE item_key = ?`, key).Scan(&tag, &status); err != nil {
		t.Fatal(err)
	}
	return tag, status
}

func TestReconcileTagsConvergesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{}
	service, jobs := tagTestService(t, cli)

	needsID := createJob(t, jobs, "request_zotio_AAAA1111", "AAAA1111", job.StateResolving, job.StateAwaitingHuman)
	createJob(t, jobs, "request_zotio_BBBB2222", "BBBB2222", job.StateResolving, job.StateUnavailable)
	createJob(t, jobs, "request_zotio_CCCC3333", "CCCC3333", job.StateResolving) // live: no tag

	result, err := service.ReconcileTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Checked != 3 || result.Added != 2 || result.Removed != 0 {
		t.Fatalf("first pass = %+v", result)
	}
	if len(cli.calls) != 2 {
		t.Fatalf("calls = %q", cli.calls)
	}
	for _, call := range cli.calls {
		joined := strings.Join(call, " ")
		if !strings.HasPrefix(joined, "--agent --yes --group= items tags add --automatic --tag papio:") {
			t.Fatalf("unexpected mutation shape: %q", joined)
		}
	}
	if state := tagStateRows(t, service); state["AAAA1111"] != TagNeedsAction || state["BBBB2222"] != TagUnavailable || len(state) != 2 {
		t.Fatalf("ledger = %v", state)
	}

	// Converged: the steady-state pass may re-check attachments, but must
	// not preflight or issue a tag mutation.
	cli.calls = nil
	preflights := cli.preflights
	result, err = service.ReconcileTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Added+result.Removed != 0 || len(cli.calls) != 0 || cli.preflights != preflights {
		t.Fatalf("steady state = %+v calls=%q preflights=%d", result, cli.calls, cli.preflights)
	}

	// The human action resolves and the job resumes: tag clears, row deleted.
	if err := jobs.Transition(ctx, needsID, job.StateAwaitingHuman, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	cli.calls = nil
	result, err = service.ReconcileTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.Added != 0 || len(cli.calls) != 1 {
		t.Fatalf("clear pass = %+v calls=%q", result, cli.calls)
	}
	if joined := strings.Join(cli.calls[0], " "); !strings.HasPrefix(joined, "--agent --yes --group= items tags remove --automatic-only --tag "+TagNeedsAction+" AAAA1111") {
		t.Fatalf("remove call = %q", joined)
	}
	if state := tagStateRows(t, service); len(state) != 1 || state["BBBB2222"] != TagUnavailable {
		t.Fatalf("ledger after clear = %v", state)
	}
}

func TestReconcileTagsTransitionRemovesOldTagFirst(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{}
	service, jobs := tagTestService(t, cli)

	jobID := createJob(t, jobs, "request_zotio_DDDD4444", "DDDD4444", job.StateResolving, job.StateAwaitingHuman)
	if _, err := service.ReconcileTags(ctx); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(ctx, jobID, job.StateAwaitingHuman, job.StateUnavailable, nil); err != nil {
		t.Fatal(err)
	}
	cli.calls = nil
	result, err := service.ReconcileTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.Added != 1 || len(cli.calls) != 2 {
		t.Fatalf("transition pass = %+v calls=%q", result, cli.calls)
	}
	if !strings.Contains(strings.Join(cli.calls[0], " "), "items tags remove --automatic-only --tag "+TagNeedsAction) {
		t.Fatalf("first call must remove the old tag: %q", cli.calls[0])
	}
	if !strings.Contains(strings.Join(cli.calls[1], " "), "items tags add --automatic --tag "+TagUnavailable) {
		t.Fatalf("second call must add the new tag: %q", cli.calls[1])
	}
	if state := tagStateRows(t, service); state["DDDD4444"] != TagUnavailable || len(state) != 1 {
		t.Fatalf("ledger = %v", state)
	}
}

func TestReconcileTagsLatestJobPerItemWins(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{}
	service, jobs := tagTestService(t, cli)

	// Older attempt exhausted; a newer manual attempt for the same item is
	// live. The item is being worked on, so no tag is wanted.
	createJob(t, jobs, "request_zotio_EEEE5555", "EEEE5555", job.StateResolving, job.StateUnavailable)
	time.Sleep(2 * time.Millisecond) // distinct created_at (RFC3339Nano)
	createJob(t, jobs, "request_manual_1", "EEEE5555", job.StateResolving)

	result, err := service.ReconcileTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 0 || len(cli.calls) != 0 {
		t.Fatalf("live retry must suppress the stale verdict: %+v calls=%q", result, cli.calls)
	}
}

func TestReconcileTagsVersionGate(t *testing.T) {
	cli := &tagCLI{version: "0.12.0"}
	service, jobs := tagTestService(t, cli)
	createJob(t, jobs, "request_zotio_FFFF6666", "FFFF6666", job.StateResolving, job.StateUnavailable)

	_, err := service.ReconcileTags(context.Background())
	if err == nil || !strings.Contains(err.Error(), tagsMinimumZotioVersion) {
		t.Fatalf("err = %v", err)
	}
	if len(cli.calls) != 0 {
		t.Fatalf("no mutation may run on an old zotio: %q", cli.calls)
	}
}

func TestReconcileTagsFailedWriteRetriesNextPass(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{failAdds: true}
	service, jobs := tagTestService(t, cli)
	createJob(t, jobs, "request_zotio_GGGG7777", "GGGG7777", job.StateResolving, job.StateUnavailable)

	if _, err := service.ReconcileTags(ctx); err == nil {
		t.Fatal("expected simulated failure")
	}
	var status string
	if err := service.Store.DB().QueryRow(`SELECT status FROM zotio_tag_state WHERE item_key = 'GGGG7777'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != tagStatusPending {
		t.Fatalf("failed write ledger status = %q, want pending", status)
	}
	cli.failAdds = false
	result, err := service.ReconcileTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 1 {
		t.Fatalf("retry pass = %+v", result)
	}
}

func TestReconcileTagsRecoversOwnedAutomaticTagAfterCrash(t *testing.T) {
	cli := &tagCLI{existingTypes: map[string]int{"RECOVER1": 1}}
	service, jobs := tagTestService(t, cli)
	createJob(t, jobs, "request_zotio_RECOVER1", "RECOVER1", job.StateResolving, job.StateUnavailable)
	if _, err := service.Store.DB().Exec(
		`INSERT INTO zotio_tag_state (item_key, tag, status, updated_at) VALUES (?, ?, ?, ?)`,
		"RECOVER1", TagUnavailable, tagStatusPending, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}

	result, err := service.ReconcileTags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 0 {
		t.Fatalf("recovery no-op must not count as a new add: %+v", result)
	}
	if _, status := tagStateStatus(t, service, "RECOVER1"); status != tagStatusOwned {
		t.Fatalf("recovered status = %q, want owned", status)
	}
}

func TestReconcileTagsPreservesPreexistingManualTag(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{existingTypes: map[string]int{"MANUAL01": 0}}
	service, jobs := tagTestService(t, cli)
	jobID := createJob(t, jobs, "request_zotio_MANUAL01", "MANUAL01", job.StateResolving, job.StateAwaitingHuman)

	if _, err := service.ReconcileTags(ctx); err != nil {
		t.Fatal(err)
	}
	if tag, status := tagStateStatus(t, service, "MANUAL01"); tag != TagNeedsAction || status != tagStatusForeign {
		t.Fatalf("manual-tag ledger = (%q,%q), want foreign %q", tag, status, TagNeedsAction)
	}
	if err := jobs.Transition(ctx, jobID, job.StateAwaitingHuman, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	cli.calls = nil
	if _, err := service.ReconcileTags(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 0 {
		t.Fatalf("manual tag must not be removed: calls=%q", cli.calls)
	}
	if state := tagStateRows(t, service); len(state) != 0 {
		t.Fatalf("foreign ledger should clear locally: %v", state)
	}
}

func TestReconcileTagsClearsAfterManualPDFAttachment(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{}
	service, jobs := tagTestService(t, cli)
	createJob(t, jobs, "request_zotio_RESOLVED", "RESOLVED", job.StateResolving, job.StateUnavailable)
	if _, err := service.ReconcileTags(ctx); err != nil {
		t.Fatal(err)
	}

	cli.calls = nil
	cli.resolved = map[string]bool{"RESOLVED": true}
	result, err := service.ReconcileTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || len(cli.calls) != 1 || !strings.Contains(strings.Join(cli.calls[0], " "), "--automatic-only") {
		t.Fatalf("manual attachment clear = %+v calls=%q", result, cli.calls)
	}
}

func TestReconcileTagsAcceptsAlreadyRemovedAutomaticTag(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{}
	service, jobs := tagTestService(t, cli)
	createJob(t, jobs, "request_zotio_REMOVED1", "REMOVED1", job.StateResolving, job.StateUnavailable)
	if _, err := service.ReconcileTags(ctx); err != nil {
		t.Fatal(err)
	}
	createJob(t, jobs, "request_zotio_REMOVED2", "REMOVED1", job.StateResolving)
	cli.removeNoOp = true
	if _, err := service.ReconcileTags(ctx); err != nil {
		t.Fatal(err)
	}
	if state := tagStateRows(t, service); len(state) != 0 {
		t.Fatalf("already-removed tag must clear ledger: %v", state)
	}
}

func TestReconcileTagsDisableClearsOwnedTags(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{}
	service, jobs := tagTestService(t, cli)
	createJob(t, jobs, "request_zotio_DISABLE1", "DISABLE1", job.StateResolving, job.StateUnavailable)
	if _, err := service.ReconcileTags(ctx); err != nil {
		t.Fatal(err)
	}

	service.ExceptionTags = false
	cli.calls = nil
	result, err := service.ReconcileTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || len(tagStateRows(t, service)) != 0 {
		t.Fatalf("disabled clear = %+v ledger=%v", result, tagStateRows(t, service))
	}
}

func TestReconcileTagsExcludesUnknownLibraryScope(t *testing.T) {
	cli := &tagCLI{}
	service, jobs := tagTestService(t, cli)
	createJob(t, jobs, "request_zotio_UNKNOWN1", "UNKNOWN1", job.StateResolving, job.StateUnavailable)
	if _, err := service.Store.DB().Exec(`DELETE FROM zotio_item_scope WHERE item_key = 'UNKNOWN1'`); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReconcileTags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Checked != 0 || len(cli.calls) != 0 {
		t.Fatalf("unknown-scope result = %+v calls=%q", result, cli.calls)
	}
}

func TestReconcileTagsMissingKeyDoesNotBlockOtherItems(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{failKeys: map[string]error{"AAAA4040": fmt.Errorf("Zotero HTTP 404")}}
	service, jobs := tagTestService(t, cli)
	createJob(t, jobs, "request_zotio_AAAA4040", "AAAA4040", job.StateResolving, job.StateUnavailable)
	createJob(t, jobs, "request_zotio_BBBBOKAY", "BBBBOKAY", job.StateResolving, job.StateUnavailable)

	if _, err := service.ReconcileTags(ctx); err == nil {
		t.Fatal("expected isolated 404 to remain reportable")
	}
	if _, status := tagStateStatus(t, service, "AAAA4040"); status != tagStatusMissing {
		t.Fatalf("missing-key status = %q", status)
	}
	if _, status := tagStateStatus(t, service, "BBBBOKAY"); status != tagStatusOwned {
		t.Fatalf("valid-key status = %q, want owned", status)
	}
	cli.calls = nil
	if _, err := service.ReconcileTags(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cli.calls) != 0 {
		t.Fatalf("settled missing target must not poison later passes: %q", cli.calls)
	}
}

func TestReconcileTagsPersistsAppliedOutcomeDespiteCommandError(t *testing.T) {
	ctx := context.Background()
	cli := &tagCLI{resultErrors: map[string]error{"PARTIAL1": fmt.Errorf("journal failed after mutation")}}
	service, jobs := tagTestService(t, cli)
	jobID := createJob(t, jobs, "request_zotio_PARTIAL1", "PARTIAL1", job.StateResolving, job.StateAwaitingHuman)

	if _, err := service.ReconcileTags(ctx); err == nil {
		t.Fatal("expected aggregate command error")
	}
	if _, status := tagStateStatus(t, service, "PARTIAL1"); status != tagStatusOwned {
		t.Fatalf("applied outcome status = %q, want owned", status)
	}
	delete(cli.resultErrors, "PARTIAL1")
	if err := jobs.Transition(ctx, jobID, job.StateAwaitingHuman, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReconcileTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 {
		t.Fatalf("clear after partial outcome = %+v", result)
	}
}

func TestReconcileTagsSerializesConcurrentPasses(t *testing.T) {
	base := &tagCLI{}
	cli := &slowTagCLI{tagCLI: base}
	service, jobs := tagTestService(t, cli)
	createJob(t, jobs, "request_zotio_SERIAL01", "SERIAL01", job.StateResolving, job.StateUnavailable)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := service.ReconcileTags(context.Background())
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := cli.max.Load(); got != 1 {
		t.Fatalf("max concurrent mutations = %d, want 1", got)
	}
	if len(base.calls) != 1 {
		t.Fatalf("mutation calls = %d, want one converging add", len(base.calls))
	}
}

func TestDesiredTagMapping(t *testing.T) {
	cases := map[string]string{
		job.StateAwaitingHuman: TagNeedsAction,
		job.StateNeedsReview:   TagNeedsAction,
		job.StateUnavailable:   TagUnavailable,
		job.StateQueued:        "",
		job.StateResolving:     "",
		job.StateReady:         "",
		job.StateImported:      "",
		job.StateFailed:        "",
		job.StateCancelled:     "",
		job.StateRetryWait:     "",
	}
	for state, want := range cases {
		if got := desiredTag(state); got != want {
			t.Fatalf("desiredTag(%s) = %q, want %q", state, got, want)
		}
	}
}

func TestTagReconcilerDisabledOnlyWhileCleanupRemains(t *testing.T) {
	service, _ := tagTestService(t, &tagCLI{})
	service.ExceptionTags = false
	if service.TagReconciler() != nil {
		t.Fatal("fresh disabled reconciler must add no maintenance work")
	}
	if _, err := service.Store.DB().Exec(
		`INSERT INTO zotio_tag_state (item_key, tag, status, updated_at) VALUES (?, ?, ?, ?)`,
		"CLEANUP1", TagUnavailable, tagStatusOwned, time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	if service.TagReconciler() == nil {
		t.Fatal("disabled reconciler must remain active while cleanup state exists")
	}
	service.CLI = nil
	if service.TagReconciler() != nil {
		t.Fatal("reconciler must be nil without a zotio CLI")
	}
}
