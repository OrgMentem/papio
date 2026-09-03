// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"papio/internal/artifact"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
	"papio/internal/zotio"
)

// fakeZotioCLI is the one Zotio process boundary every test in this file
// drives. The seven zotio.* handlers own no Zotio logic of their own — they
// decode params, guard the dependency, and classify the backend's error — so a
// single honest CLI stand-in covers all of them, and one fixture change stays
// visible in every test that depends on it.
type fakeZotioCLI struct {
	mu           sync.Mutex
	syncCalls    int
	preflightErr error
	missing      []zotio.MissingPDFItem
	missingErr   error
	syncErr      error
	// find answers "items find --<kind> <value>" keyed by "<kind>:<value>".
	find map[string]json.RawMessage
	// preview and apply are the mutation envelopes; the presence of "--yes"
	// in the argv is what distinguishes an apply from a preview, exactly as
	// zotio.Plan records the two argv forms.
	preview    json.RawMessage
	apply      json.RawMessage
	runJSONErr error
}

func (f *fakeZotioCLI) Preflight(context.Context) (*zotio.PreflightResult, error) {
	if f.preflightErr != nil {
		return nil, f.preflightErr
	}
	return &zotio.PreflightResult{Executable: "zotio-fake", Version: "1.0.0"}, nil
}

func (f *fakeZotioCLI) MissingPDF(context.Context, string, int) ([]zotio.MissingPDFItem, error) {
	if f.missingErr != nil {
		return nil, f.missingErr
	}
	return append([]zotio.MissingPDFItem(nil), f.missing...), nil
}

func (f *fakeZotioCLI) GetItem(_ context.Context, key string) (*zotio.Item, error) {
	return nil, fmt.Errorf("fake zotio holds no detail for %s", key)
}

func (f *fakeZotioCLI) Sync(context.Context) error {
	f.mu.Lock()
	f.syncCalls++
	f.mu.Unlock()
	return f.syncErr
}

func (f *fakeZotioCLI) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncCalls
}

func (f *fakeZotioCLI) RunJSON(_ context.Context, args ...string) (json.RawMessage, error) {
	if f.runJSONErr != nil {
		return nil, f.runJSONErr
	}
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "items find"):
		if len(args) < 5 {
			return nil, fmt.Errorf("unexpected find argv %q", joined)
		}
		if raw := f.find[strings.TrimPrefix(args[3], "--")+":"+args[4]]; raw != nil {
			return raw, nil
		}
		return json.RawMessage("[]"), nil
	case strings.Contains(joined, "--yes"):
		if f.apply == nil {
			return nil, fmt.Errorf("fake zotio has no apply envelope for %q", joined)
		}
		return f.apply, nil
	default:
		if f.preview == nil {
			return nil, fmt.Errorf("fake zotio has no preview envelope for %q", joined)
		}
		return f.preview, nil
	}
}

type fakeZotioSubmitter struct {
	mu       sync.Mutex
	requests []protocol.WorkRequest
}

func (f *fakeZotioSubmitter) Submit(_ context.Context, request protocol.WorkRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	return "job_" + request.ZotioItemKey, nil
}

func (f *fakeZotioSubmitter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// fakeZotioImporter satisfies both app.AutoImporter and
// zotio.ImportBackfillImporter, which is what zotio.import_backfill needs on
// the apply branch.
type fakeZotioImporter struct{ calls int }

func (f *fakeZotioImporter) PlanAndApply(context.Context, string) (string, string, string, error) {
	f.calls++
	return "applied", "PA12RE34", "AT56CH90", nil
}

// installFakeZotio replaces the system's Zotio service with one wired to cli.
// config.Default sets zotio.executable to "zotio", so a default test system
// already holds a real zotio.Client pointed at whatever binary is on PATH.
// Every zotio.* success path in this package must displace it first, so no
// test ever drives the operator's real Zotero library.
func installFakeZotio(t *testing.T, system *bootstrap.System, cli *fakeZotioCLI) *fakeZotioSubmitter {
	t.Helper()
	submitter := &fakeZotioSubmitter{}
	system.Zotio = &zotio.Service{
		CLI:       cli,
		Submitter: submitter,
		Bundle:    system.Bundle,
		Store:     system.Store,
		DataDir:   system.Config.DataDir,
	}
	return submitter
}

// readyZotioJobSystem builds a system holding one ready job whose bundle can be
// exported and which already carries a Zotero parent item key. The key routes
// zotio.plan down the existing-item branch, which needs no import manifest, so
// the plan/apply pair is driven by the preview and apply envelopes alone.
func readyZotioJobSystem(t *testing.T, itemKey string) (*bootstrap.System, string) {
	t.Helper()
	ctx := context.Background()
	system := testSystem(t)
	id, err := system.Jobs.CreateRequest(ctx, "wr_zotio_handler",
		work.Work{DOI: "10.1000/zotio-handler", Title: "A Zotio Handler", Authors: []string{"Ada Lovelace"}, Year: 2026},
		itemKey, "", job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20},
		nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := system.Jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "unpaywall", URLRedacted: "https://example.test/paper.pdf", URLKey: "zotio-handler-url-key",
		LandingRedacted: "https://example.test/article", Version: "published", AccessBasis: "open_access",
		ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1, Rank: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, _ := system.Jobs.NextPendingCandidate(ctx, id)
	if candidate == nil {
		t.Fatal("candidate missing")
	}
	if err := system.Jobs.MarkCandidate(ctx, candidate.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	quarantine, err := system.Artifacts.QuarantineDir(id)
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(quarantine, "fixture.tmp")
	if err := os.WriteFile(temp, []byte("%PDF-1.4\nzotio handler fixture\n%%EOF"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, size, err := artifact.HashFile(temp)
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := system.Artifacts.Promote(temp, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.UpsertArtifact(ctx, job.Artifact{
		SHA256: sha, SizeBytes: size, MIME: "application/pdf", PageCount: 1,
		Path: path, IdentityResult: "pass", CreatedAt: "2026-08-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil,
		job.WithArtifact(sha), job.WithCandidate(candidate.ID)); err != nil {
		t.Fatal(err)
	}
	return system, id
}

// TestZotioHandlerRejectsUndecodableParams pins that every zotio.* method
// decodes strictly BEFORE it touches the dependency. The system here has its
// Zotio integration removed, and three of these seven handlers hold no nil
// guard, so a handler that reached for the backend first would answer
// something other than invalid_argument.
func TestZotioHandlerRejectsUndecodableParams(t *testing.T) {
	system := testSystem(t)
	system.Zotio = nil
	router := Router(system)
	for _, tc := range []struct {
		method string
		params any
	}{
		{"zotio.queue", map[string]any{"limit": "twelve"}},
		{"zotio.missing_count", map[string]any{"collection": 42}},
		{"zotio.lookup_works", map[string]any{"works": "everything"}},
		{"zotio.plan", map[string]any{"job_ids": "job_1"}},
		{"zotio.import_backfill", map[string]any{"apply": "yes"}},
		{"zotio.tags.reconcile", map[string]any{"unexpected": true}},
		{"zotio.apply", map[string]any{"plan_id": 1}},
	} {
		rpcErr := callMethod(t, router, tc.method, tc.params, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("%s with %v = %#v, want invalid_argument", tc.method, tc.params, rpcErr)
		}
	}
}

// TestZotioHandlerApplyRequiresPlanIDAndConfirmation covers the second
// invalid_argument path zotio.apply owns: a decodable body that omits either
// half of the confirmation pair. Apply is the only mutation in this family that
// writes to the user's Zotero library, so an empty confirmation must never
// reach the service.
func TestZotioHandlerApplyRequiresPlanIDAndConfirmation(t *testing.T) {
	system := testSystem(t)
	installFakeZotio(t, system, &fakeZotioCLI{})
	router := Router(system)
	for _, params := range []map[string]any{
		{},
		{"plan_id": "zplan_0123456789abcdef0123456789"},
		{"confirmation_sha256": "abc"},
		{"plan_id": "", "confirmation_sha256": "abc"},
	} {
		rpcErr := callMethod(t, router, "zotio.apply", params, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("zotio.apply %v = %#v, want invalid_argument", params, rpcErr)
		}
	}
}

// TestZotioHandlerReadsQueueMissingCountAndLookupWorks drives the three
// read-shaped handlers against the fake and asserts the response bodies the
// CLI, the extension, and MCP decode.
func TestZotioHandlerReadsQueueMissingCountAndLookupWorks(t *testing.T) {
	system := testSystem(t)
	cli := &fakeZotioCLI{
		missing: []zotio.MissingPDFItem{
			{Key: "AB12CD34", Title: "Queued paper", DOI: "https://doi.org/10.1000/Queued"},
			{Key: "EF56GH78", Title: "Owned paper", DOI: "10.1000/owned"},
		},
		find: map[string]json.RawMessage{
			"doi:10.1000/owned": json.RawMessage(`{"meta":{"total":1},"results":[{"key":"EF56GH78","data":{}}]}`),
		},
	}
	submitter := installFakeZotio(t, system, cli)
	router := Router(system)

	var queued zotio.QueueResult
	if rpcErr := callMethod(t, router, "zotio.queue", map[string]any{"limit": 5}, &queued); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if queued.Preflight == nil || queued.Preflight.Version != "1.0.0" {
		t.Fatalf("queue preflight = %+v", queued.Preflight)
	}
	if len(queued.Queued) != 2 || submitter.count() != 2 {
		t.Fatalf("queued = %+v, submitted = %d, want 2 and 2", queued.Queued, submitter.count())
	}
	if queued.Queued[0].ZotioItemKey != "AB12CD34" || queued.Queued[0].JobID != "job_AB12CD34" {
		t.Fatalf("first queued row = %+v", queued.Queued[0])
	}

	var counted struct {
		Missing int `json:"missing"`
	}
	if rpcErr := callMethod(t, router, "zotio.missing_count", map[string]any{"collection": "Reading"}, &counted); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if counted.Missing != 2 {
		t.Fatalf("missing count = %d, want 2", counted.Missing)
	}

	var lookup zotio.LookupWorksResult
	if rpcErr := callMethod(t, router, "zotio.lookup_works", map[string]any{
		"works": []map[string]string{{"doi": "10.1000/owned"}, {"doi": "10.1000/absent"}},
	}, &lookup); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if cli.syncCount() != 1 {
		t.Fatalf("sync calls = %d, want 1 (local_only is false)", cli.syncCount())
	}
	if len(lookup.Works) != 2 {
		t.Fatalf("lookup works = %+v, want 2 rows", lookup.Works)
	}
	// EF56GH78 is in the missing-PDF queue, which is the signal LookupWorks
	// trusts for "owned but no file".
	if lookup.Works[0].Status != zotio.OwnershipOwnedMissingPDF || lookup.Works[0].ItemKey != "EF56GH78" {
		t.Fatalf("owned row = %+v, want owned_missing_pdf EF56GH78", lookup.Works[0])
	}
	if lookup.Works[1].Status != zotio.OwnershipNotOwned || lookup.Works[1].ItemKey != "" {
		t.Fatalf("absent row = %+v, want not_owned", lookup.Works[1])
	}
}

// TestZotioHandlerReadPathsReportBackendFailureAsPreconditionFailed pins the
// classification the three read-shaped handlers use. They do NOT go through
// zotioFailure: a Zotio that cannot answer is reported as a precondition the
// caller can fix (start Zotero, fix the executable), not as a daemon fault.
func TestZotioHandlerReadPathsReportBackendFailureAsPreconditionFailed(t *testing.T) {
	system := testSystem(t)
	cli := &fakeZotioCLI{
		missingErr: errors.New("zotio mirror unavailable"),
		runJSONErr: errors.New("zotio items find failed"),
	}
	installFakeZotio(t, system, cli)
	router := Router(system)
	for _, tc := range []struct {
		method string
		params any
	}{
		{"zotio.queue", map[string]any{"limit": 5}},
		{"zotio.missing_count", map[string]any{}},
		{"zotio.lookup_works", map[string]any{"works": []map[string]string{{"doi": "10.1000/x"}}}},
	} {
		rpcErr := callMethod(t, router, tc.method, tc.params, nil)
		if rpcErr == nil || rpcErr.Code != "precondition_failed" {
			t.Fatalf("%s backend failure = %#v, want precondition_failed", tc.method, rpcErr)
		}
	}
}

// TestZotioHandlerMutationsClassifyBackendFailureAsInternalNotNotFound pins the
// divergence between the two failure helpers this package holds. The four
// mutation-shaped zotio handlers route every backend error through
// zotioFailure, which answers `internal` with a Zotio error class attached.
// The generic `failure` helper the rest of the RPC surface uses would answer
// `not_found` for the very same sql.ErrNoRows, so unifying the two helpers
// would silently change the wire contract of zotio.plan for a missing job.
func TestZotioHandlerMutationsClassifyBackendFailureAsInternalNotNotFound(t *testing.T) {
	// The divergence itself, stated where a future refactor would read it.
	if _, generic := failure(sql.ErrNoRows); generic == nil || generic.Code != "not_found" {
		t.Fatalf("generic failure(sql.ErrNoRows) = %#v, want not_found", generic)
	}
	if _, zotioErr := zotioFailure(sql.ErrNoRows); zotioErr == nil || zotioErr.Code != "internal" {
		t.Fatalf("zotioFailure(sql.ErrNoRows) = %#v, want internal", zotioErr)
	}

	system := testSystem(t)
	installFakeZotio(t, system, &fakeZotioCLI{})
	router := Router(system)

	// job.Store.Get returns a bare sql.ErrNoRows for an unknown job, so this
	// is exactly the error the generic helper would have called not_found.
	rpcErr := callMethod(t, router, "zotio.plan", map[string]any{"job_ids": []string{"job_does_not_exist"}}, nil)
	if rpcErr == nil || rpcErr.Code != "internal" {
		t.Fatalf("zotio.plan for a missing job = %#v, want internal (not not_found)", rpcErr)
	}
	if rpcErr.Detail == nil || rpcErr.Detail.ErrorClass == "" {
		t.Fatalf("zotio.plan error detail = %#v, want a Zotio error class", rpcErr.Detail)
	}

	// A syntactically valid plan ID with no plan file behind it.
	rpcErr = callMethod(t, router, "zotio.apply", map[string]any{
		"plan_id": "zplan_0123456789abcdef0123456789", "confirmation_sha256": "not-the-digest",
	}, nil)
	if rpcErr == nil || rpcErr.Code != "internal" || rpcErr.Detail == nil {
		t.Fatalf("zotio.apply for a missing plan = %#v, want internal with detail", rpcErr)
	}

	// import_backfill and tags.reconcile reach their service methods with a
	// half-configured service and must classify identically.
	system.Zotio.Bundle = nil
	rpcErr = callMethod(t, router, "zotio.import_backfill", map[string]any{"limit": 5}, nil)
	if rpcErr == nil || rpcErr.Code != "internal" || rpcErr.Detail == nil {
		t.Fatalf("zotio.import_backfill backend failure = %#v, want internal with detail", rpcErr)
	}
	system.Zotio.Store = nil
	rpcErr = callMethod(t, router, "zotio.tags.reconcile", map[string]any{}, nil)
	if rpcErr == nil || rpcErr.Code != "internal" || rpcErr.Detail == nil {
		t.Fatalf("zotio.tags.reconcile backend failure = %#v, want internal with detail", rpcErr)
	}
}

// TestZotioHandlerGuardsMissingZotioDependency records what each handler
// actually does when the daemon runs without a Zotio integration — the state
// of every user who sets zotio.executable to "" to switch the deep Zotero
// integration off.
//
// Four handlers hold an explicit nil guard and answer precondition_failed.
// zotio.plan, zotio.tags.reconcile, and zotio.apply hold NO guard: they call
// straight through the nil *zotio.Service. That does not panic, because every
// one of those service methods starts with its own `s == nil` check, but the
// answer an unconfigured caller gets is `internal` — a daemon fault — rather
// than the precondition_failed its four siblings return. This test pins the
// observed behaviour, divergence included.
func TestZotioHandlerGuardsMissingZotioDependency(t *testing.T) {
	system := testSystem(t)
	system.Zotio = nil
	router := Router(system)
	for _, tc := range []struct {
		method   string
		params   any
		wantCode string
	}{
		{"zotio.queue", map[string]any{"limit": 5}, "precondition_failed"},
		{"zotio.missing_count", map[string]any{}, "precondition_failed"},
		{"zotio.lookup_works", map[string]any{"works": []map[string]string{{"doi": "10.1000/x"}}}, "precondition_failed"},
		{"zotio.import_backfill", map[string]any{"limit": 5}, "precondition_failed"},
		// No nil guard below this line.
		{"zotio.plan", map[string]any{"job_ids": []string{"job_1"}}, "internal"},
		{"zotio.tags.reconcile", map[string]any{}, "internal"},
		{"zotio.apply", map[string]any{"plan_id": "zplan_0123456789abcdef0123456789", "confirmation_sha256": "d"}, "internal"},
	} {
		rpcErr := callMethod(t, router, tc.method, tc.params, nil)
		if rpcErr == nil || rpcErr.Code != tc.wantCode {
			t.Fatalf("%s without Zotio = %#v, want %s", tc.method, rpcErr, tc.wantCode)
		}
	}
}

// TestZotioHandlerImportBackfillGuardsTheImporter covers the second
// precondition zotio.import_backfill owns: an apply pass needs the
// auto-importer the application service holds, and a dry run does not.
func TestZotioHandlerImportBackfillGuardsTheImporter(t *testing.T) {
	system := testSystem(t)
	installFakeZotio(t, system, &fakeZotioCLI{})
	if system.App == nil {
		t.Fatal("test system has no application service")
	}
	system.App.AutoImporter = nil
	router := Router(system)

	rpcErr := callMethod(t, router, "zotio.import_backfill", map[string]any{"apply": true, "limit": 5}, nil)
	if rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("apply without an importer = %#v, want precondition_failed", rpcErr)
	}

	// The same request with no importer required must succeed as a dry run.
	var dry zotio.ImportBackfillResult
	if rpcErr := callMethod(t, router, "zotio.import_backfill", map[string]any{"limit": 5}, &dry); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !dry.DryRun {
		t.Fatalf("dry_run = false for a request without apply: %+v", dry)
	}

	// And the apply branch must reach the service once the importer exists.
	importer := &fakeZotioImporter{}
	system.App.AutoImporter = importer
	var applied zotio.ImportBackfillResult
	if rpcErr := callMethod(t, router, "zotio.import_backfill", map[string]any{"apply": true, "limit": 5}, &applied); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if applied.DryRun {
		t.Fatalf("dry_run = true for an apply request: %+v", applied)
	}
}

// TestZotioHandlerTagsReconcileReportsACount drives the tags reconciler
// success path: a configured Zotio with nothing to converge answers a zeroed
// ledger report rather than an error.
func TestZotioHandlerTagsReconcileReportsACount(t *testing.T) {
	system := testSystem(t)
	installFakeZotio(t, system, &fakeZotioCLI{})
	router := Router(system)
	var result zotio.TagReconcileResult
	if rpcErr := callMethod(t, router, "zotio.tags.reconcile", map[string]any{}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.Checked != 0 || result.Added != 0 || result.Removed != 0 {
		t.Fatalf("reconcile result = %+v, want a zeroed report", result)
	}
}

// TestZotioHandlerPlanThenApplyDrivesTheMutation is the mutation-shaped success
// path for both zotio.plan and zotio.apply. The plan the handler returns is the
// immutable confirmation object, and its confirmation digest is the only input
// zotio.apply accepts, so the two handlers are tested as the pair they are.
func TestZotioHandlerPlanThenApplyDrivesTheMutation(t *testing.T) {
	system, jobID := readyZotioJobSystem(t, "AB12CD34")
	cli := &fakeZotioCLI{
		preview: json.RawMessage(`{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`),
		apply:   json.RawMessage(`{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`),
	}
	installFakeZotio(t, system, cli)
	router := Router(system)

	var planned struct {
		Plans []zotio.Plan `json:"plans"`
	}
	if rpcErr := callMethod(t, router, "zotio.plan", map[string]any{"job_ids": []string{jobID}}, &planned); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(planned.Plans) != 1 {
		t.Fatalf("plans = %+v, want exactly one", planned.Plans)
	}
	plan := planned.Plans[0]
	if plan.JobID != jobID || plan.Route != "existing_item" || plan.ExpectedParentKey != "AB12CD34" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.ID == "" || plan.ConfirmationSHA256 == "" {
		t.Fatalf("plan is not a usable confirmation object: %+v", plan)
	}

	// A confirmation digest that does not match the plan must never mutate.
	rpcErr := callMethod(t, router, "zotio.apply", map[string]any{
		"plan_id": plan.ID, "confirmation_sha256": strings.Repeat("0", 64),
	}, nil)
	if rpcErr == nil || rpcErr.Code != "internal" {
		t.Fatalf("apply with a mismatched confirmation = %#v, want internal", rpcErr)
	}

	var result zotio.ApplyResult
	if rpcErr := callMethod(t, router, "zotio.apply", map[string]any{
		"plan_id": plan.ID, "confirmation_sha256": plan.ConfirmationSHA256,
	}, &result); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if result.Status != "applied" || result.PlanID != plan.ID || result.JobID != jobID {
		t.Fatalf("apply result = %+v", result)
	}
	if result.ParentKey != "AB12CD34" || result.AttachmentKey != "AT56CH90" {
		t.Fatalf("apply result keys = %+v", result)
	}
	// The mutation is durable: apply advances the acquisition to imported.
	row, err := system.Jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateImported {
		t.Fatalf("job state after apply = %q, want imported", row.State)
	}
}
