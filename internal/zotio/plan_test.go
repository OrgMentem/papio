// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	manifest      string
	preview       string
	apply         string
	applyErr      error
	resolveCalls  int
	syncCalls     int
	previewCalls  int
	applyCalls    int
	lastResolveAt string
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
