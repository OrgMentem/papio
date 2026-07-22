// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/work"
)

// tagCLI records tag mutations; every other RunJSON shape is unexpected.
type tagCLI struct {
	version    string
	preflights int
	calls      [][]string
	failAdds   bool
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
func (f *tagCLI) GetItem(context.Context, string) (*Item, error) { return nil, fmt.Errorf("not used") }
func (f *tagCLI) Sync(context.Context) error                     { return nil }
func (f *tagCLI) RunJSON(_ context.Context, args ...string) (json.RawMessage, error) {
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "items tags add") && !strings.Contains(joined, "items tags remove") {
		return nil, fmt.Errorf("unexpected RunJSON %q", args)
	}
	if f.failAdds && strings.Contains(joined, "items tags add") {
		return nil, fmt.Errorf("simulated zotio failure")
	}
	f.calls = append(f.calls, args)
	return json.RawMessage(`{}`), nil
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

	// Converged: the steady-state pass must not preflight or invoke zotio.
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
	if joined := strings.Join(cli.calls[0], " "); !strings.HasPrefix(joined, "--agent --yes --group= items tags remove --tag "+TagNeedsAction+" AAAA1111") {
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
	if !strings.Contains(strings.Join(cli.calls[0], " "), "items tags remove --tag "+TagNeedsAction) {
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
	if state := tagStateRows(t, service); len(state) != 0 {
		t.Fatalf("failed write must not be recorded as applied: %v", state)
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

func TestTagReconcilerDisabled(t *testing.T) {
	service, _ := tagTestService(t, &tagCLI{})
	service.ExceptionTags = false
	if service.TagReconciler() != nil {
		t.Fatal("reconciler must be nil when exception tags are disabled")
	}
	service.ExceptionTags = true
	service.CLI = nil
	if service.TagReconciler() != nil {
		t.Fatal("reconciler must be nil without a zotio CLI")
	}
}
