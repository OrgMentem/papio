// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// retractionCLI models the zotio surface the retraction reconciler touches: an
// exact DOI lookup that can also return near matches, and automatic/manual tag
// semantics (add --automatic is a no_op over a manual tag; remove
// --automatic-only never removes a manual tag).
type retractionCLI struct {
	mu          sync.Mutex
	byDOI       map[string][]retractionFound // lookup result per queried DOI
	tags        map[string]map[string]string // item key -> tag -> "automatic" | "manual"
	mutation    []string
	findErr     error
	findCalls   int
	mutationErr error
}

func newRetractionCLI() *retractionCLI {
	return &retractionCLI{byDOI: map[string][]retractionFound{}, tags: map[string]map[string]string{}}
}

func (f *retractionCLI) addItem(key, doi string) {
	item := retractionFound{Key: key}
	item.Data.DOI = doi
	f.byDOI[doi] = append(f.byDOI[doi], item)
}

func (f *retractionCLI) tagType(key, tag string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tags[key][tag]
}

func (f *retractionCLI) setTag(key, tag, kind string) {
	if f.tags[key] == nil {
		f.tags[key] = map[string]string{}
	}
	f.tags[key][tag] = kind
}

func (f *retractionCLI) Preflight(context.Context) (*PreflightResult, error) {
	return &PreflightResult{Executable: "zotio", Version: tagsMinimumZotioVersion}, nil
}
func (f *retractionCLI) MissingPDF(context.Context, string, int) ([]MissingPDFItem, error) {
	return nil, errors.New("not used")
}
func (f *retractionCLI) GetItem(context.Context, string) (*Item, error) {
	return nil, errors.New("not used")
}
func (f *retractionCLI) Sync(context.Context) error { return nil }
func (f *retractionCLI) RunJSON(_ context.Context, args ...string) (json.RawMessage, error) {
	joined := strings.Join(args, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(joined, "items find --doi") {
		f.findCalls++
		if f.findErr != nil {
			return nil, f.findErr
		}
		if !strings.Contains(joined, "--group=") {
			return nil, fmt.Errorf("find must be scoped to the personal library: %q", joined)
		}
		raw, _ := json.Marshal(map[string]any{"results": f.byDOI[args[len(args)-1]]})
		return raw, nil
	}
	adding := strings.Contains(joined, "items tags add")
	removing := strings.Contains(joined, "items tags remove")
	if !adding && !removing {
		return nil, fmt.Errorf("unexpected RunJSON %q", args)
	}
	f.mutation = append(f.mutation, joined)
	if f.mutationErr != nil {
		return nil, f.mutationErr
	}
	key, tag := args[len(args)-1], args[len(args)-2]
	have := f.tags[key][tag]
	status := "applied"
	var reason any
	switch {
	case adding && !strings.Contains(joined, "--automatic"):
		return nil, errors.New("tag add must be automatic")
	case adding && have != "":
		status = "no_op"
		reason = map[string]any{"tag_types": map[string]int{tag: map[string]int{"automatic": 1, "manual": 0}[have]}}
	case adding:
		f.setTag(key, tag, "automatic")
	case removing && !strings.Contains(joined, "--automatic-only"):
		return nil, errors.New("tag remove must be automatic-only")
	case removing && have != "automatic":
		status = "no_op"
		reason = "tag not present"
	case removing:
		delete(f.tags[key], tag)
	}
	raw, _ := json.Marshal(map[string]any{"result": map[string]any{"items": []map[string]any{{"key": key, "status": status, "reason": reason}}}})
	return raw, nil
}

type fakeRetractions struct {
	dois []string
	err  error
}

func (f *fakeRetractions) RetractedDOIs(context.Context) ([]string, error) {
	return append([]string(nil), f.dois...), f.err
}

func retractionService(t *testing.T, cli CLI, source RetractedDOISource) *Service {
	t.Helper()
	service, _ := tagTestService(t, cli)
	service.ExceptionTags = false
	service.RetractionTags = true
	service.Retractions = source
	return service
}

func TestRetractionTagsTagOnlyExactDOIItems(t *testing.T) {
	cli := newRetractionCLI()
	cli.addItem("AAAAAAAA", "10.1234/retracted")
	// A lookup that also returns near matches and an attachment must not tag
	// them.
	near := retractionFound{Key: "BBBBBBBB"}
	near.Data.DOI = "10.1234/retracted-other"
	child := retractionFound{Key: "CCCCCCCC"}
	child.Data.DOI = "10.1234/retracted"
	child.Data.ParentItem = "AAAAAAAA"
	cli.byDOI["10.1234/retracted"] = append(cli.byDOI["10.1234/retracted"], near, child)
	cli.addItem("DDDDDDDD", "10.1234/unrelated")
	source := &fakeRetractions{dois: []string{"10.1234/retracted"}}
	service := retractionService(t, cli, source)

	result, err := service.ReconcileRetractionTags(context.Background(), true)
	if err != nil || result.Added != 1 || len(result.Add) != 1 || result.Add[0].ItemKey != "AAAAAAAA" {
		t.Fatalf("result = %+v, %v; want exactly AAAAAAAA tagged", result, err)
	}
	for key, want := range map[string]string{"AAAAAAAA": "automatic", "BBBBBBBB": "", "CCCCCCCC": "", "DDDDDDDD": ""} {
		if got := cli.tagType(key, TagRetracted); got != want {
			t.Errorf("tag type on %s = %q, want %q", key, got, want)
		}
	}
	// A second pass converges: no mutation.
	cli.mutation = nil
	if result, err := service.ReconcileRetractionTags(context.Background(), true); err != nil || result.Added != 0 || len(cli.mutation) != 0 {
		t.Fatalf("second pass = %+v, %v, mutations %v; want none", result, err, cli.mutation)
	}
}

func TestRetractionTagsPreviewDoesNotWrite(t *testing.T) {
	cli := newRetractionCLI()
	cli.addItem("AAAAAAAA", "10.1234/retracted")
	service := retractionService(t, cli, &fakeRetractions{dois: []string{"10.1234/retracted"}})

	result, err := service.ReconcileRetractionTags(context.Background(), false)
	if err != nil || result.Applied || len(result.Add) != 1 || result.Added != 0 {
		t.Fatalf("preview = %+v, %v", result, err)
	}
	if len(cli.mutation) != 0 || cli.tagType("AAAAAAAA", TagRetracted) != "" {
		t.Fatalf("preview mutated zotero: %v", cli.mutation)
	}
	// Preview of a disabled option still lists what enabling would add.
	service.RetractionTags = false
	result, err = service.ReconcileRetractionTags(context.Background(), false)
	if err != nil || result.Enabled || len(result.Add) != 1 {
		t.Fatalf("disabled preview = %+v, %v; want the adds enabling would make", result, err)
	}
	// Applying while disabled writes nothing.
	if result, err = service.ReconcileRetractionTags(context.Background(), true); err != nil || result.Added != 0 || len(cli.mutation) != 0 {
		t.Fatalf("disabled apply = %+v, %v, mutations %v", result, err, cli.mutation)
	}
}

func TestRetractionTagsWithdrawnNoticeAndDisableRemoveOnlyOwnedTags(t *testing.T) {
	for _, mode := range []string{"withdrawn", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			cli := newRetractionCLI()
			cli.addItem("AAAAAAAA", "10.1234/retracted")
			cli.addItem("MMMMMMMM", "10.1234/manual")
			cli.setTag("MMMMMMMM", TagRetracted, "manual")
			cli.setTag("AAAAAAAA", "my-own-tag", "manual")
			source := &fakeRetractions{dois: []string{"10.1234/retracted", "10.1234/manual"}}
			service := retractionService(t, cli, source)

			result, err := service.ReconcileRetractionTags(context.Background(), true)
			if err != nil || result.Added != 1 || result.Preserved != 1 {
				t.Fatalf("initial = %+v, %v; want 1 added and the manual tag preserved", result, err)
			}
			if cli.tagType("MMMMMMMM", TagRetracted) != "manual" {
				t.Fatal("manual tag was retyped or removed on add")
			}

			if mode == "withdrawn" {
				source.dois = nil
			} else {
				service.RetractionTags = false
				// The source is not consulted when disabled.
				source.err = errors.New("must not be read")
			}
			result, err = service.ReconcileRetractionTags(context.Background(), true)
			if err != nil || result.Removed != 1 {
				t.Fatalf("cleanup = %+v, %v; want exactly the owned tag removed", result, err)
			}
			if got := cli.tagType("AAAAAAAA", TagRetracted); got != "" {
				t.Errorf("owned tag still present: %q", got)
			}
			if got := cli.tagType("MMMMMMMM", TagRetracted); got != "manual" {
				t.Errorf("manual tag = %q, want untouched", got)
			}
			if got := cli.tagType("AAAAAAAA", "my-own-tag"); got != "manual" {
				t.Errorf("unrelated manual tag = %q, want untouched", got)
			}
			var rows int
			if err := service.Store.DB().QueryRow(`SELECT COUNT(*) FROM zotio_tag_state WHERE item_key LIKE 'retraction:%'`).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("ledger rows = %d, %v; want cleared", rows, err)
			}
		})
	}
}

func TestRetractionTagsLookupFailureNeverStripsOwnedTags(t *testing.T) {
	cli := newRetractionCLI()
	cli.addItem("AAAAAAAA", "10.1234/retracted")
	source := &fakeRetractions{dois: []string{"10.1234/retracted"}}
	service := retractionService(t, cli, source)
	if _, err := service.ReconcileRetractionTags(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	cli.findErr = errors.New("zotio down")
	if _, err := service.ReconcileRetractionTags(context.Background(), true); err == nil {
		t.Fatal("pass succeeded despite a failed lookup")
	}
	source.err = errors.New("cache unreadable")
	cli.findErr = nil
	if _, err := service.ReconcileRetractionTags(context.Background(), true); err == nil {
		t.Fatal("pass succeeded despite an unknown notice set")
	}
	if cli.tagType("AAAAAAAA", TagRetracted) != "automatic" {
		t.Fatal("owned tag was removed when the notice set was unknown")
	}
}

func TestRetractionTagsAreIndependentOfExceptionTags(t *testing.T) {
	cli := newRetractionCLI()
	cli.addItem("AAAAAAAA", "10.1234/retracted")
	service := retractionService(t, cli, &fakeRetractions{dois: []string{"10.1234/retracted"}})
	if _, err := service.ReconcileRetractionTags(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// The exception reconciler (enabled, nothing to tag) must neither see nor
	// remove the retraction ledger row.
	service.ExceptionTags = true
	cli.mutation = nil
	if result, err := service.ReconcileTags(context.Background()); err != nil || result.Removed != 0 || result.Checked != 0 || len(cli.mutation) != 0 {
		t.Fatalf("exception pass = %+v, %v, mutations %v; want it blind to retraction rows", result, err, cli.mutation)
	}
	var rows int
	if err := service.Store.DB().QueryRow(`SELECT COUNT(*) FROM zotio_tag_state WHERE item_key = 'retraction:AAAAAAAA' AND status = 'owned'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("retraction ledger rows after exception pass = %d, %v; want the owned row kept", rows, err)
	}
	if cli.tagType("AAAAAAAA", TagRetracted) != "automatic" {
		t.Fatal("exception reconciler removed the retraction tag")
	}
}

func TestRetractionTagReconcilerRunsOnlyWhenNoticesChange(t *testing.T) {
	cli := newRetractionCLI()
	cli.addItem("AAAAAAAA", "10.1234/retracted")
	source := &fakeRetractions{dois: []string{"10.1234/retracted"}}
	service := retractionService(t, cli, source)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	reconciler := service.RetractionTagReconciler()
	if reconciler == nil {
		t.Fatal("enabled option built no reconciler")
	}
	if err := reconciler.RunDue(context.Background()); err != nil || cli.tagType("AAAAAAAA", TagRetracted) != "automatic" {
		t.Fatalf("first run = %v, tag %q", err, cli.tagType("AAAAAAAA", TagRetracted))
	}
	cli.byDOI = map[string][]retractionFound{} // a rescan now would find nothing
	if err := reconciler.RunDue(context.Background()); err != nil || cli.tagType("AAAAAAAA", TagRetracted) != "automatic" {
		t.Fatalf("unchanged notice set caused a pass: %v", err)
	}
	source.dois = nil // withdrawn
	if err := reconciler.RunDue(context.Background()); err != nil || cli.tagType("AAAAAAAA", TagRetracted) != "" {
		t.Fatalf("withdrawal run = %v, tag %q", err, cli.tagType("AAAAAAAA", TagRetracted))
	}

	off := retractionService(t, newRetractionCLI(), source)
	off.RetractionTags = false
	if off.RetractionTagReconciler() != nil {
		t.Fatal("default-off option with an empty ledger built a reconciler")
	}
}

func TestRetractionTagReconcilerReportsMissingSource(t *testing.T) {
	ctx := context.Background()
	cli := newRetractionCLI()
	cli.setTag("AAAAAAAA", TagRetracted, "automatic")
	service := retractionService(t, cli, nil)
	if err := service.upsertTagState(ctx, retractionLedgerPrefix+"AAAAAAAA", TagRetracted, tagStatusOwned); err != nil {
		t.Fatal(err)
	}
	err := service.RetractionTagReconciler().RunDue(ctx)
	if err == nil || !strings.Contains(err.Error(), "retraction notices are not available: enable the retraction_watch source") {
		t.Fatalf("missing source = %v, want an actionable maintenance failure", err)
	}
	if cli.findCalls != 0 || len(cli.mutation) != 0 || cli.tagType("AAAAAAAA", TagRetracted) != "automatic" {
		t.Fatalf("unknown notices changed owned tags: finds %d, mutations %v", cli.findCalls, cli.mutation)
	}
	if _, status := tagStateStatus(t, service, retractionLedgerPrefix+"AAAAAAAA"); status != tagStatusOwned {
		t.Fatalf("ledger status = %q, want ownership preserved", status)
	}
}

func TestRetractionTagReconcilerBacksOffFailedWrites(t *testing.T) {
	ctx := context.Background()
	cli := newRetractionCLI()
	cli.addItem("AAAAAAAA", "10.1234/retracted")
	cli.mutationErr = WithErrorInfo(fmt.Errorf("Zotero HTTP 403"))
	service := retractionService(t, cli, &fakeRetractions{dois: []string{"10.1234/retracted"}})
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	service.Now = func() time.Time { return now }
	reconciler := service.RetractionTagReconciler()
	assertFailure := func() {
		t.Helper()
		if err := reconciler.RunDue(ctx); err == nil || ErrorInfoFrom(err).Class != ErrorClassZoteroHTTP4xx {
			t.Fatalf("RunDue = %v, want the tag write failure to remain visible", err)
		}
	}
	assertFailure()
	attempts := 1
	for _, delay := range []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute,
		80 * time.Minute, 160 * time.Minute, 320 * time.Minute, 6 * time.Hour, 6 * time.Hour,
	} {
		failedAt := now
		for _, elapsed := range []time.Duration{time.Minute, delay - time.Nanosecond} {
			now = failedAt.Add(elapsed)
			if err := reconciler.RunDue(ctx); err != nil {
				t.Fatalf("backoff pass returned a new error: %v", err)
			}
			if err := tagMaintenanceFailure(t, reconciler); err == nil || ErrorInfoFrom(err).Class != ErrorClassZoteroHTTP4xx {
				t.Fatalf("backoff health = %v, want the tag write failure", err)
			}
			if cli.findCalls != attempts || len(cli.mutation) != attempts {
				t.Fatalf("retry during %s delay: finds %d, mutations %d; want %d", delay, cli.findCalls, len(cli.mutation), attempts)
			}
		}
		now = failedAt.Add(delay)
		assertFailure()
		attempts++
		if cli.findCalls != attempts || len(cli.mutation) != attempts {
			t.Fatalf("due retry: finds %d, mutations %d; want %d", cli.findCalls, len(cli.mutation), attempts)
		}
	}
	if _, status := tagStateStatus(t, service, retractionLedgerPrefix+"AAAAAAAA"); status != tagStatusPending {
		t.Fatalf("failed write status = %q, want pending", status)
	}
	cli.mutationErr = nil
	now = now.Add(6 * time.Hour)
	if err := reconciler.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tagMaintenanceFailure(t, reconciler); err != nil {
		t.Fatalf("recovered maintenance health = %v, want no failure", err)
	}
	if cli.tagType("AAAAAAAA", TagRetracted) != "automatic" {
		t.Fatal("the due recovery pass did not tag the retracted item")
	}
	if _, status := tagStateStatus(t, service, retractionLedgerPrefix+"AAAAAAAA"); status != tagStatusOwned {
		t.Fatalf("recovered write status = %q, want owned", status)
	}
}
