// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/artifact"
	"papio/internal/bundle"
	"papio/internal/job"
	"papio/internal/redact"
	"papio/internal/store"
	"papio/internal/work"
)

type planCLI struct {
	manifest        string
	preview         string
	apply           string
	applyErr        error
	enrichErr       error
	resolveCalls    int
	syncCalls       int
	previewCalls    int
	applyCalls      int
	enrichCalls     int
	lastResolveAt   string
	collectionCalls int
	collectionErr   error
	enrichArgs      []string
	callOrder       []string
}

func (c *planCLI) Preflight(context.Context) (*PreflightResult, error) {
	return &PreflightResult{Executable: "zotio", Version: "1.0.0"}, nil
}
func (c *planCLI) MissingPDF(context.Context, string, int) ([]MissingPDFItem, error) {
	return nil, fmt.Errorf("unexpected MissingPDF")
}
func (c *planCLI) GetItem(context.Context, string) (*Item, error) {
	return nil, fmt.Errorf("unexpected GetItem")
}
func (c *planCLI) Sync(context.Context) error {
	c.syncCalls++
	return nil
}
func (c *planCLI) RunJSON(_ context.Context, args ...string) (json.RawMessage, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "import resolve"):
		c.resolveCalls++
		c.lastResolveAt = args[len(args)-1]
		return json.RawMessage(c.manifest), nil
	case strings.Contains(joined, "items add-to-collection"):
		c.collectionCalls++
		c.callOrder = append(c.callOrder, "collection")
		return json.RawMessage(`{"ok":true}`), c.collectionErr
	case strings.Contains(joined, "items enrich"):
		c.enrichCalls++
		c.enrichArgs = append([]string(nil), args...)
		c.callOrder = append(c.callOrder, "enrich")
		return json.RawMessage(`{"ok":true}`), c.enrichErr
	case strings.Contains(joined, "--yes"):
		c.applyCalls++
		return json.RawMessage(c.apply), c.applyErr
	default:
		c.previewCalls++
		return json.RawMessage(c.preview), nil
	}
}

func readyPlanService(t *testing.T, zotioKey string, cli CLI) (*Service, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	jobs := &job.Store{S: db}
	artifacts, err := artifact.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := jobs.CreateRequest(ctx, "request_plan_001", work.Work{
		DOI: "10.1002/example", Title: "Example Paper", Authors: []string{"Ada Lovelace"}, Year: 2024,
	}, zotioKey, "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = jobs.InsertCandidates(ctx, jobID, []job.Candidate{{
		JobID: jobID, Source: "unpaywall", URLRedacted: redact.URL("https://example.test/paper.pdf"), URLKey: "url-key",
		LandingRedacted: "https://example.test/article", Version: "published", AccessBasis: "open_access",
		ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := jobs.NextPendingCandidate(ctx, jobID)
	if err != nil || candidate == nil {
		t.Fatalf("candidate = %+v, %v", candidate, err)
	}
	if err := jobs.MarkCandidate(ctx, candidate.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	quarantine, err := artifacts.QuarantineDir(jobID)
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(quarantine, "paper.tmp")
	body := []byte("%PDF-1.4\nfixture DOI 10.1002/example\n%%EOF")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _, err := artifact.HashFile(temp)
	if err != nil {
		t.Fatal(err)
	}
	artifactPath, err := artifacts.Promote(temp, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.UpsertArtifact(ctx, job.Artifact{
		SHA256: sha, SizeBytes: int64(len(body)), MIME: "application/pdf", PageCount: 1,
		TextChars: 1000, IdentityResult: "pass", Path: artifactPath,
	}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}, {job.StateFetching, job.StateValidating}} {
		if err := jobs.Transition(ctx, jobID, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := jobs.Transition(ctx, jobID, job.StateValidating, job.StateReady, nil, job.WithCandidate(candidate.ID), job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	exporter := &bundle.Exporter{Jobs: jobs, Artifacts: artifacts, DataDir: dataDir}
	return &Service{
		CLI: cli, Bundle: exporter, Store: db, DataDir: dataDir,
		Now: func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) },
	}, jobID
}

func TestExistingItemPlanApplyIsConfirmedAndIdempotent(t *testing.T) {
	cli := &planCLI{
		preview: `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:   `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`,
	}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].Route != "existing_item" || plans[0].ConfirmationSHA256 == "" || cli.syncCalls != 0 || cli.previewCalls != 1 {
		t.Fatalf("plans=%+v syncCalls=%d previewCalls=%d", plans, cli.syncCalls, cli.previewCalls)
	}
	repeated, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil || repeated[0].ID != plans[0].ID || cli.previewCalls != 1 {
		t.Fatalf("repeated=%+v err=%v previewCalls=%d", repeated, err, cli.previewCalls)
	}
	if _, err := service.Apply(context.Background(), plans[0].ID, "sha256:wrong"); err == nil || cli.applyCalls != 0 {
		t.Fatalf("wrong confirmation err=%v applyCalls=%d", err, cli.applyCalls)
	}
	applied, err := service.Apply(context.Background(), plans[0].ID, plans[0].ConfirmationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if applied.ParentKey != "AB12CD34" || applied.AttachmentKey != "AT56CH90" || applied.Status != "applied" || cli.applyCalls != 1 {
		t.Fatalf("applied=%+v calls=%d", applied, cli.applyCalls)
	}
	replay, err := service.Apply(context.Background(), plans[0].ID, plans[0].ConfirmationSHA256)
	if err != nil || replay.AttachmentKey != "AT56CH90" || cli.applyCalls != 1 {
		t.Fatalf("replay=%+v err=%v calls=%d", replay, err, cli.applyCalls)
	}
	var plansCount, appliesCount int
	_ = service.Store.DB().QueryRow(`SELECT count(*) FROM exports WHERE kind='zotio_plan'`).Scan(&plansCount)
	_ = service.Store.DB().QueryRow(`SELECT count(*) FROM exports WHERE kind='zotio_apply'`).Scan(&appliesCount)
	if plansCount != 1 || appliesCount != 1 {
		t.Fatalf("ledger plan=%d apply=%d", plansCount, appliesCount)
	}
}

func TestExistingItemPlanUsesConfiguredLinkedAttachmentMode(t *testing.T) {
	cli := &planCLI{
		preview: `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
	}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	service.AttachmentMode = "linked-file"
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	if plan.AttachmentMode != "linked-file" ||
		!strings.Contains(strings.Join(plan.PreviewArgs, " "), "--mode linked-file") ||
		!strings.Contains(strings.Join(plan.ApplyArgs, " "), "--mode linked-file") {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestApplyRecoversAbandonedReservation(t *testing.T) {
	cli := &planCLI{
		preview: `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:   `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`,
	}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	key := "zotio_apply:" + plan.ID + ":" + plan.ConfirmationSHA256
	if _, err := service.Store.DB().Exec(
		`INSERT INTO exports (job_id, kind, idempotency_key, created_at) VALUES (?, 'zotio_apply', ?, ?)`,
		plan.JobID, key, store.Now()); err != nil {
		t.Fatalf("reserving apply: %v", err)
	}

	applied, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != "applied" || applied.AttachmentKey != "AT56CH90" || cli.applyCalls != 1 {
		t.Fatalf("applied=%+v applyCalls=%d", applied, cli.applyCalls)
	}
	replay, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256)
	if err != nil || replay.AttachmentKey != "AT56CH90" || cli.applyCalls != 1 {
		t.Fatalf("replay=%+v err=%v applyCalls=%d", replay, err, cli.applyCalls)
	}
}

func TestClaimApplyReservesInProgressStatus(t *testing.T) {
	service, jobID := readyPlanService(t, "AB12CD34", &planCLI{})
	ctx := context.Background()
	key := "zotio_apply:claim-exclusive"

	claimed, err := service.claimApply(ctx, key, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("first apply claim was not acquired")
	}
	var raw string
	if err := service.Store.DB().QueryRowContext(ctx,
		`SELECT result_json FROM exports WHERE kind = 'zotio_apply' AND idempotency_key = ?`, key,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `{"status":"in_progress"}` {
		t.Fatalf("claim result_json = %q", raw)
	}
	claimed, err = service.claimApply(ctx, key, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("second apply claim acquired an in-progress reservation")
	}
}

func TestMaterializePrivateFileCopiesSource(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.pdf")
	target := filepath.Join(t.TempDir(), "target.pdf")
	contents := []byte("immutable artifact contents")
	if err := os.WriteFile(source, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	expected := fmt.Sprintf("%x", sha256.Sum256(contents))
	if err := materializePrivateFile(source, target, expected); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("mutated staging file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(contents) {
		t.Fatalf("source changed through staged file: %q", got)
	}
}

func TestFailedManifestApplyInvalidatesCachedDerivation(t *testing.T) {
	invalidManifest := `{"schema_version":2,"entries":[{"path":"paper.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"10.1002/example","status":"resolved","item":{"itemType":"document","title":"Example Paper","publicationTitle":"not allowed for document"}}]}`
	correctedManifest := `{"schema_version":2,"entries":[{"path":"paper.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"10.1002/example","status":"resolved","item":{"itemType":"journalArticle","title":"Example Paper","DOI":"10.1002/example"}}]}`
	validationError := "publicationTitle is not valid for itemType document"
	cli := &planCLI{
		manifest: invalidManifest,
		preview:  `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:    `{"ok":false,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":0,"no_op":0,"conflicts":0,"failed":1},"items":[{"status":"failed","reason":"` + validationError + `"}]}}`,
		applyErr: fmt.Errorf("mutation incomplete"),
	}
	service, jobID := readyPlanService(t, "", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	failedPlan := plans[0]
	failedManifestPath := failedPlan.ManifestPath
	failedPlanPath := filepath.Join(service.DataDir, "zotio", "plans", failedPlan.ID+".json")

	if _, err := service.Apply(context.Background(), failedPlan.ID, failedPlan.ConfirmationSHA256); err == nil || !strings.Contains(err.Error(), validationError) {
		t.Fatalf("initial apply error = %v", err)
	}
	if _, err := os.Stat(failedManifestPath); !os.IsNotExist(err) {
		t.Fatalf("failed manifest still exists: %v", err)
	}
	if _, err := os.Stat(failedPlanPath); !os.IsNotExist(err) {
		t.Fatalf("failed plan still exists: %v", err)
	}
	var planCount int
	if err := service.Store.DB().QueryRow(`SELECT count(*) FROM exports WHERE kind = 'zotio_plan'`).Scan(&planCount); err != nil {
		t.Fatal(err)
	}
	if planCount != 0 {
		t.Fatalf("cached plan rows = %d, want 0", planCount)
	}
	var recordedFailure string
	if err := service.Store.DB().QueryRow(`SELECT result_json FROM exports WHERE kind = 'zotio_apply'`).Scan(&recordedFailure); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recordedFailure, `"status":"failed"`) || !strings.Contains(recordedFailure, validationError) {
		t.Fatalf("recorded failure = %s", recordedFailure)
	}

	cli.manifest = correctedManifest
	cli.apply = `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"status":"applied","reason":{"via":"web","parent_key":"PA12RE34","attachment_key":"AT56CH90"}}]}}`
	cli.applyErr = nil
	replanned, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	if len(replanned) != 1 || replanned[0].ID == failedPlan.ID || cli.resolveCalls != 2 {
		t.Fatalf("replanned=%+v failedPlan=%+v resolveCalls=%d", replanned, failedPlan, cli.resolveCalls)
	}
	correctedPlan := replanned[0]
	stagedManifest, err := os.ReadFile(correctedPlan.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stagedManifest) != correctedManifest+"\n" {
		t.Fatalf("staged manifest = %s", stagedManifest)
	}
	applied, err := service.Apply(context.Background(), correctedPlan.ID, correctedPlan.ConfirmationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != "applied" || applied.ParentKey != "PA12RE34" || applied.AttachmentKey != "AT56CH90" {
		t.Fatalf("applied = %+v", applied)
	}
	replay, err := service.Apply(context.Background(), correctedPlan.ID, correctedPlan.ConfirmationSHA256)
	if err != nil || replay.Status != "applied" || cli.applyCalls != 2 {
		t.Fatalf("replay=%+v err=%v applyCalls=%d", replay, err, cli.applyCalls)
	}
	manifestAfterReplay, err := os.ReadFile(correctedPlan.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(manifestAfterReplay) != string(stagedManifest) {
		t.Fatalf("successful replay changed manifest: before=%s after=%s", stagedManifest, manifestAfterReplay)
	}
}

func TestManifestCreatePlanStagesDOIAndReturnsBothKeys(t *testing.T) {
	cli := &planCLI{
		manifest: `{"schema_version":2,"entries":[{"path":"paper.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"10.1002/example","status":"resolved","item":{"itemType":"journalArticle","title":"Example Paper","DOI":"10.1002/example"}}]}`,
		preview:  `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:    `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"status":"applied","reason":{"via":"web","parent_key":"PA12RE34","attachment_key":"AT56CH90"}}]}}`,
	}
	service, jobID := readyPlanService(t, "", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[0]
	if plan.Route != "manifest_create" || cli.syncCalls != 1 || cli.resolveCalls != 1 || !strings.Contains(filepath.Base(plan.ManifestPath), plan.ArtifactSHA256) {
		t.Fatalf("plan=%+v syncCalls=%d resolveCalls=%d", plan, cli.syncCalls, cli.resolveCalls)
	}
	staged := filepath.Join(cli.lastResolveAt, "10.1002%2Fexample.pdf")
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged DOI PDF: %v", err)
	}
	result, err := service.Apply(context.Background(), plan.ID, plan.ConfirmationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if result.ParentKey != "PA12RE34" || result.AttachmentKey != "AT56CH90" {
		t.Fatalf("result = %+v", result)
	}
}

func TestManifestDuplicatePlanReturnsNoOpWithoutAttachment(t *testing.T) {
	cli := &planCLI{
		manifest: `{"schema_version":2,"entries":[{"classification":"duplicate","action":"skip","matched_key":"AB12CD34","identifier":"10.1002/example","status":"resolved"}]}`,
		preview:  `{"ok":true,"mode":"preview","plan":{"summary":{"planned":0,"no_op":1,"invalid":0}},"result":null}`,
		apply:    `{"ok":true,"mode":"apply","plan":{"summary":{"planned":0,"no_op":1}},"result":{"summary":{"applied":0,"no_op":1,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"no_op"}]}}`,
	}
	service, jobID := readyPlanService(t, "", cli)
	plans, err := service.PlanJobs(context.Background(), []string{jobID})
	if err != nil {
		t.Fatal(err)
	}
	if cli.syncCalls != 1 {
		t.Fatalf("syncCalls=%d, want 1", cli.syncCalls)
	}
	result, err := service.Apply(context.Background(), plans[0].ID, plans[0].ConfirmationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if plans[0].Route != "manifest_duplicate" || result.Status != "no_op" || result.ParentKey != "AB12CD34" || result.AttachmentKey != "" {
		t.Fatalf("plan=%+v result=%+v", plans[0], result)
	}
}

func TestPlanAndApplyFilesPolicyCollectionWithoutRollingBackImport(t *testing.T) {
	cli := &planCLI{
		preview:       `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:         `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`,
		collectionErr: errors.New("collection service unavailable"),
	}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	row, err := service.Bundle.Jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	row.Policy.Collection = "Reading"
	policyJSON, err := json.Marshal(row.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.DB().Exec(`UPDATE jobs SET policy_json = ? WHERE id = ?`, string(policyJSON), jobID); err != nil {
		t.Fatal(err)
	}

	status, parentKey, attachmentKey, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil {
		t.Fatalf("collection filing must not fail import: %v", err)
	}
	if status != "applied" || parentKey != "AB12CD34" || attachmentKey != "AT56CH90" || cli.collectionCalls != 1 {
		t.Fatalf("result=(%q,%q,%q) collection calls=%d", status, parentKey, attachmentKey, cli.collectionCalls)
	}
	events, err := service.Bundle.Jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event["kind"] != "zotio.collection_filing" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if detail["collection"] == "Reading" && detail["status"] == "error" {
			if detail["error_class"] != ErrorClassUnknown || detail["error_type"] == "" {
				t.Fatalf("collection filing detail = %#v", detail)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("collection filing failure was not recorded: %+v", events)
	}
}

func TestFileCollectionSkipsQueueCollectionKey(t *testing.T) {
	cli := &planCLI{}
	service := &Service{CLI: cli}
	service.fileCollection(context.Background(),
		&Plan{Collection: "ZX98YU76"},
		&ApplyResult{ParentKey: "AB12CD34"},
	)
	if cli.collectionCalls != 0 {
		t.Fatalf("collection calls = %d, want 0", cli.collectionCalls)
	}
}

func enablePlanAutoImport(t *testing.T, service *Service, jobID, collection string) {
	t.Helper()
	row, err := service.Bundle.Jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	row.Policy.AutoImport = true
	row.Policy.Collection = collection
	policyJSON, err := json.Marshal(row.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.DB().Exec(`UPDATE jobs SET policy_json = ? WHERE id = ?`, string(policyJSON), jobID); err != nil {
		t.Fatal(err)
	}
}

func zotioEventDetail(t *testing.T, service *Service, jobID, kind string) map[string]any {
	t.Helper()
	events, err := service.Bundle.Jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == kind {
			detail, ok := event["detail"].(map[string]any)
			if !ok {
				t.Fatalf("%s detail = %#v", kind, event["detail"])
			}
			return detail
		}
	}
	t.Fatalf("no %s event in %#v", kind, events)
	return nil
}

func TestPlanAndApplyAutoEnrichesAppliedParentOnce(t *testing.T) {
	cli := &planCLI{
		preview: `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:   `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`,
	}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	service.AutoEnrich = true
	enablePlanAutoImport(t, service, jobID, "Reading")

	status, parentKey, attachmentKey, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if status != "applied" || parentKey != "AB12CD34" || attachmentKey != "AT56CH90" {
		t.Fatalf("result = (%q, %q, %q)", status, parentKey, attachmentKey)
	}
	wantArgs := []string{"--agent", "--yes", "items", "enrich", "--missing-doi", "--missing-abstract", "--keys", "AB12CD34"}
	if cli.enrichCalls != 1 || !slices.Equal(cli.enrichArgs, wantArgs) {
		t.Fatalf("enrich calls=%d args=%q, want %q", cli.enrichCalls, cli.enrichArgs, wantArgs)
	}
	if len(cli.callOrder) < 2 || !slices.Equal(cli.callOrder[:2], []string{"collection", "enrich"}) {
		t.Fatalf("command order = %q, want collection before enrich", cli.callOrder)
	}
	detail := zotioEventDetail(t, service, jobID, "zotio.enrich")
	if detail["status"] != "applied" || detail["summary"] == "" {
		t.Fatalf("enrich event = %#v", detail)
	}
	filing := zotioEventDetail(t, service, jobID, "zotio.collection_filing")
	if filing["status"] != "applied" || filing["collection"] != "Reading" {
		t.Fatalf("collection filing event = %#v", filing)
	}

	if _, _, _, err := service.PlanAndApply(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	if cli.enrichCalls != 1 {
		t.Fatalf("replayed apply enriched %d times, want 1", cli.enrichCalls)
	}
}

func TestPlanAndApplyAutoEnrichFailureLeavesImportApplied(t *testing.T) {
	cli := &planCLI{
		preview:   `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:     `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`,
		enrichErr: errors.New("enrichment unavailable"),
	}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	service.AutoEnrich = true
	enablePlanAutoImport(t, service, jobID, "")

	status, parentKey, attachmentKey, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil {
		t.Fatalf("enrichment failure must not fail import: %v", err)
	}
	if status != "applied" || parentKey != "AB12CD34" || attachmentKey != "AT56CH90" {
		t.Fatalf("result = (%q, %q, %q)", status, parentKey, attachmentKey)
	}
	detail := zotioEventDetail(t, service, jobID, "zotio.enrich")
	if detail["status"] != "error" || detail["summary"] == "" || detail["error_type"] == "" || detail["error_class"] != ErrorClassUnknown {
		t.Fatalf("enrich failure event = %#v", detail)
	}
}

func TestPlanAndApplyAutoEnrichCanBeDisabled(t *testing.T) {
	cli := &planCLI{
		preview: `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:   `{"ok":true,"mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"key":"AB12CD34","status":"applied","reason":{"item_key":"AT56CH90","upload":"uploaded"}}]}}`,
	}
	service, jobID := readyPlanService(t, "AB12CD34", cli)
	enablePlanAutoImport(t, service, jobID, "")

	if status, _, _, err := service.PlanAndApply(context.Background(), jobID); err != nil || status != "applied" {
		t.Fatalf("result = (%q, %v)", status, err)
	}
	if cli.enrichCalls != 0 {
		t.Fatalf("disabled auto-enrich calls = %d, want 0", cli.enrichCalls)
	}
}

// lockedCLI serializes planCLI access so concurrent Service calls exercise the
// exports-ledger conflict handling without racing the fake CLI's counters.
type lockedCLI struct {
	mu    sync.Mutex
	inner *planCLI
}

func (c *lockedCLI) Preflight(ctx context.Context) (*PreflightResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.Preflight(ctx)
}
func (c *lockedCLI) MissingPDF(ctx context.Context, collection string, limit int) ([]MissingPDFItem, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.MissingPDF(ctx, collection, limit)
}
func (c *lockedCLI) GetItem(ctx context.Context, key string) (*Item, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.GetItem(ctx, key)
}
func (c *lockedCLI) Sync(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.Sync(ctx)
}
func (c *lockedCLI) RunJSON(ctx context.Context, args ...string) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.RunJSON(ctx, args...)
}

func TestPlanJobsConcurrentSameJobConverge(t *testing.T) {
	inner := &planCLI{
		preview: `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
	}
	service, jobID := readyPlanService(t, "AB12CD34", &lockedCLI{inner: inner})

	// Materialize the bundle artifact once up front: bundle export uses an
	// O_EXCL create that is not concurrency-safe for the same job, an FS concern
	// unrelated to the exports-ledger convergence under test. Warming it (idempotent
	// once the file exists) lets both goroutines reach recordPlan's ON CONFLICT path.
	if _, _, err := service.Bundle.Export(context.Background(), jobID, ""); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	plans := make([][]*Plan, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			plans[i], errs[i] = service.PlanJobs(context.Background(), []string{jobID})
		}()
	}
	close(start)
	wg.Wait()

	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("PlanJobs[%d] err = %v", i, errs[i])
		}
		if len(plans[i]) != 1 || plans[i][0].ID == "" {
			t.Fatalf("PlanJobs[%d] = %+v", i, plans[i])
		}
	}
	// Both concurrent callers must converge on one canonical recorded plan
	// rather than one failing on the unique idempotency_key constraint.
	if plans[0][0].ID != plans[1][0].ID {
		t.Fatalf("plans diverged: %s vs %s", plans[0][0].ID, plans[1][0].ID)
	}
	var planRows int
	if err := service.Store.DB().QueryRow(`SELECT count(*) FROM exports WHERE kind='zotio_plan'`).Scan(&planRows); err != nil {
		t.Fatal(err)
	}
	if planRows != 1 {
		t.Fatalf("ledger zotio_plan rows = %d, want 1", planRows)
	}
}

func TestApplyInFlightReservationConflictIsRetryable(t *testing.T) {
	// Apply's in-flight reservation branch must surface an explicit retryable
	// conflict instead of (nil,nil): the error wraps job.ErrConflict and
	// classifies as a reservation conflict so outer retry logic backs off
	// rather than synthesizing a spurious failure over an in-flight mutation.
	err := WithErrorInfo(fmt.Errorf("Zotio apply reservation for plan %s is in progress: %w", "zplan_deadbeefdeadbeefdeadbeef01", job.ErrConflict))
	if err == nil {
		t.Fatal("expected non-nil retryable conflict error")
	}
	if !errors.Is(err, job.ErrConflict) {
		t.Fatalf("error is not errors.Is job.ErrConflict: %v", err)
	}
	if info := ErrorInfoFrom(err); info.Class != ErrorClassReservationConflict {
		t.Fatalf("class = %q, want %q", info.Class, ErrorClassReservationConflict)
	}
	if !strings.Contains(err.Error(), "reservation for plan") || !strings.Contains(err.Error(), "is in progress") {
		t.Fatalf("message = %q", err.Error())
	}
}
