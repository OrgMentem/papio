// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"papio/internal/job"
	"papio/internal/store"
)

const ZotioPlanSchemaVersion = "papio-zotio-plan/1"

var planIDRE = regexp.MustCompile(`^zplan_[a-f0-9]{26}$`)

// Plan is papio's immutable confirmation object around one exact Zotio preview.
type Plan struct {
	SchemaVersion      string          `json:"schema_version"`
	ID                 string          `json:"id"`
	JobID              string          `json:"job_id"`
	Route              string          `json:"route"`
	BundlePath         string          `json:"bundle_path"`
	ArtifactPath       string          `json:"artifact_path"`
	ArtifactSHA256     string          `json:"artifact_sha256"`
	ManifestPath       string          `json:"manifest_path,omitempty"`
	ExpectedParentKey  string          `json:"expected_parent_key,omitempty"`
	DOI                string          `json:"doi,omitempty"`
	AttachmentMode     string          `json:"attachment_mode"`
	Collection         string          `json:"collection,omitempty"`
	PreviewArgs        []string        `json:"preview_args"`
	ApplyArgs          []string        `json:"apply_args"`
	Preview            json.RawMessage `json:"preview"`
	CreatedAt          string          `json:"created_at"`
	ConfirmationSHA256 string          `json:"confirmation_sha256"`
}

// ApplyResult is the durable outcome returned on both first apply and replay.
type ApplyResult struct {
	PlanID        string          `json:"plan_id"`
	JobID         string          `json:"job_id"`
	Status        string          `json:"status"`
	ParentKey     string          `json:"parent_key,omitempty"`
	AttachmentKey string          `json:"attachment_key,omitempty"`
	AppliedAt     string          `json:"applied_at"`
	Error         string          `json:"error,omitempty"`
	Zotio         json.RawMessage `json:"zotio"`
}

type mutationEnvelope struct {
	OK   bool   `json:"ok"`
	Mode string `json:"mode"`
	Plan struct {
		Summary struct {
			Planned int `json:"planned"`
			NoOp    int `json:"no_op"`
			Invalid int `json:"invalid"`
		} `json:"summary"`
	} `json:"plan"`
	Result *struct {
		Summary struct {
			Applied   int `json:"applied"`
			NoOp      int `json:"no_op"`
			Conflicts int `json:"conflicts"`
			Failed    int `json:"failed"`
		} `json:"summary"`
		Items []struct {
			Key    string `json:"key"`
			Status string `json:"status"`
			Reason any    `json:"reason"`
		} `json:"items"`
	} `json:"result"`
}

type importManifest struct {
	SchemaVersion int `json:"schema_version"`
	Entries       []struct {
		Classification string `json:"classification"`
		Action         string `json:"action"`
		MatchedKey     string `json:"matched_key"`
		Identifier     string `json:"identifier"`
		Status         string `json:"status"`
	} `json:"entries"`
}

// PlanJobs previews one exact Zotio mutation per ready papio job and records it
// in the exports ledger. Existing equivalent plans are returned idempotently.
func (s *Service) PlanJobs(ctx context.Context, jobIDs []string) ([]*Plan, error) {
	if err := s.requirePlanServices(); err != nil {
		return nil, err
	}
	if len(jobIDs) == 0 || len(jobIDs) > 50 {
		return nil, fmt.Errorf("plan requires 1..50 job IDs")
	}
	if _, err := s.CLI.Preflight(ctx); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(jobIDs))
	plans := make([]*Plan, 0, len(jobIDs))
	for _, id := range jobIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return nil, fmt.Errorf("job IDs must be nonempty and unique")
		}
		seen[id] = true
		plan, err := s.planJob(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("planning job %s: %w", id, err)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func (s *Service) planJob(ctx context.Context, jobID string) (*Plan, error) {
	row, err := s.Bundle.Jobs.Get(ctx, jobID)
	if err != nil {
		return nil, err
	}
	bundlePath, acquisition, err := s.Bundle.Export(ctx, jobID, "")
	if err != nil {
		return nil, err
	}
	artifactPath := filepath.Join(filepath.Dir(bundlePath), filepath.FromSlash(acquisition.Artifact.Path))
	attachmentMode := s.attachmentMode()
	collection := strings.TrimSpace(row.Policy.Collection)
	idempotencyKey := "zotio_plan:" + jobID + ":" + acquisition.Artifact.SHA256 + ":" + attachmentMode + ":" + collection
	if existing, err := s.recordedPlan(ctx, idempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	plan := &Plan{
		SchemaVersion:  ZotioPlanSchemaVersion,
		ID:             job.NewID("zplan"),
		JobID:          jobID,
		BundlePath:     bundlePath,
		ArtifactPath:   artifactPath,
		ArtifactSHA256: acquisition.Artifact.SHA256,
		DOI:            acquisition.Identity.DOI,
		CreatedAt:      s.now().UTC().Format(time.RFC3339),
		AttachmentMode: attachmentMode,
		Collection:     collection,
	}
	if acquisition.ZotioItemKey != "" {
		plan.Route = "existing_item"
		plan.ExpectedParentKey = acquisition.ZotioItemKey
		plan.PreviewArgs = []string{"--agent", "attachments", "add", acquisition.ZotioItemKey, artifactPath, "--mode", attachmentMode}
		plan.ApplyArgs = []string{"--agent", "--yes", "attachments", "add", acquisition.ZotioItemKey, artifactPath, "--mode", attachmentMode}
	} else {
		if acquisition.Identity.DOI == "" {
			return nil, fmt.Errorf("new-item Zotio routing requires a DOI")
		}
		if err := s.CLI.Sync(ctx); err != nil {
			return nil, fmt.Errorf("refreshing Zotio library before deduplication: %w", err)
		}
		manifestPath, manifest, err := s.resolveManifest(ctx, plan)
		if err != nil {
			return nil, err
		}
		plan.ManifestPath = manifestPath
		plan.Route, plan.ExpectedParentKey, err = manifestRoute(manifest)
		if err != nil {
			return nil, err
		}
		plan.PreviewArgs = []string{"--agent", "--via", "web", "import", "apply", manifestPath, "--attach-mode", attachmentMode}
		plan.ApplyArgs = []string{"--agent", "--yes", "--via", "web", "import", "apply", manifestPath, "--attach-mode", attachmentMode}
	}

	preview, err := s.CLI.RunJSON(ctx, plan.PreviewArgs...)
	if err != nil {
		return nil, fmt.Errorf("previewing Zotio mutation: %w", err)
	}
	if err := validatePreview(preview); err != nil {
		return nil, err
	}
	plan.Preview = preview
	plan.ConfirmationSHA256, err = planDigest(plan)
	if err != nil {
		return nil, err
	}
	path, err := s.writePlan(plan)
	if err != nil {
		return nil, err
	}
	recorded, err := s.recordPlan(ctx, idempotencyKey, path, plan)
	if err != nil {
		return nil, err
	}
	return recorded, nil
}

// Apply verifies the immutable plan, artifact content address, and explicit
// confirmation digest before invoking Zotio with --yes. Replays return the
// recorded result without another Zotero write.
func (s *Service) Apply(ctx context.Context, planID, confirmation string) (*ApplyResult, error) {
	if err := s.requirePlanServices(); err != nil {
		return nil, err
	}
	if _, err := s.CLI.Preflight(ctx); err != nil {
		return nil, err
	}
	plan, err := s.LoadPlan(planID)
	if err != nil {
		return nil, err
	}
	if confirmation != plan.ConfirmationSHA256 {
		return nil, fmt.Errorf("confirmation SHA-256 does not match plan %s", plan.ID)
	}
	if err := verifyFileSHA256(plan.ArtifactPath, plan.ArtifactSHA256); err != nil {
		return nil, fmt.Errorf("verifying planned artifact: %w", err)
	}
	idempotencyKey := "zotio_apply:" + plan.ID + ":" + confirmation
	if existing, err := s.recordedApply(ctx, idempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		s.fileCollection(ctx, plan, existing)
		return existing, nil
	}
	claimed, err := s.claimApply(ctx, idempotencyKey, plan.JobID)
	if err != nil {
		return nil, err
	}
	if !claimed {
		result, recordErr := s.recordedApply(ctx, idempotencyKey)
		if recordErr != nil {
			return nil, recordErr
		}
		if result != nil {
			s.fileCollection(ctx, plan, result)
			return result, nil
		}
		// Another worker holds the reservation but has not recorded a result
		// yet: the mutation is in flight. Returning (nil,nil) would let
		// PlanAndApply synthesize a spurious 'failed' while the real Zotero
		// write is still running, which outer retry could turn into a duplicate
		// or contended mutation. Surface an explicit retryable conflict.
		return nil, WithErrorInfo(fmt.Errorf("Zotio apply reservation for plan %s is in progress: %w", plan.ID, job.ErrConflict))
	}

	out, commandErr := s.CLI.RunJSON(ctx, plan.ApplyArgs...)
	if commandErr != nil {
		applyErr := fmt.Errorf("applying Zotio mutation: %w", commandErr)
		if message, ok := mutationFailure(out); ok {
			applyErr = fmt.Errorf("applying Zotio mutation: %s", message)
		}
		return nil, s.recordFailedApplyAndInvalidatePlan(ctx, idempotencyKey, plan, out, applyErr)
	}
	envelope, err := decodeApply(out)
	if err != nil {
		applyErr := fmt.Errorf("decoding Zotio apply result: %w", err)
		return nil, s.recordFailedApplyAndInvalidatePlan(ctx, idempotencyKey, plan, out, applyErr)
	}
	result := &ApplyResult{
		PlanID:    plan.ID,
		JobID:     plan.JobID,
		Status:    "applied",
		ParentKey: plan.ExpectedParentKey,
		AppliedAt: s.now().UTC().Format(time.RFC3339),
		Zotio:     out,
	}
	if envelope.Result.Summary.Applied == 0 {
		result.Status = "no_op"
	}
	for _, item := range envelope.Result.Items {
		if result.ParentKey == "" {
			result.ParentKey = stringField(item.Reason, "parent_key")
		}
		if result.ParentKey == "" && item.Key != "" && plan.Route != "manifest_create" {
			result.ParentKey = item.Key
		}
		if result.AttachmentKey == "" {
			result.AttachmentKey = stringField(item.Reason, "attachment_key")
		}
		if result.AttachmentKey == "" {
			result.AttachmentKey = stringField(item.Reason, "item_key")
		}
	}
	if plan.Route != "manifest_duplicate" && result.ParentKey == "" {
		applyErr := errors.New("Zotio apply succeeded without returning a parent item key")
		return nil, s.recordFailedApplyAndInvalidatePlan(ctx, idempotencyKey, plan, out, applyErr)
	}
	if envelope.Result.Summary.Applied > 0 && result.AttachmentKey == "" {
		applyErr := errors.New("Zotio apply succeeded without returning an attachment key")
		return nil, s.recordFailedApplyAndInvalidatePlan(ctx, idempotencyKey, plan, out, applyErr)
	}
	if err := s.recordApply(ctx, idempotencyKey, result); err != nil {
		return nil, err
	}
	s.fileCollection(ctx, plan, result)
	s.enrichAutoImportedParent(ctx, plan, result)
	return result, nil
}

// fileCollection applies the optional policy filing after the import has been
// durably recorded. Filing is deliberately best-effort: an attachment/import
// must not be rolled back because a collection write cannot be completed.
func (s *Service) fileCollection(ctx context.Context, plan *Plan, result *ApplyResult) {
	if plan == nil || result == nil || result.ParentKey == "" {
		return
	}
	collection := strings.TrimSpace(plan.Collection)
	if collection == "" {
		return
	}
	if keyRE.MatchString(collection) {
		// Queue collection filters are keys; their existing parent already belongs
		// to that collection, while add-to-collection accepts names only.
		return
	}
	detail := map[string]any{"collection": collection}
	if _, err := s.CLI.RunJSON(ctx, "--agent", "--yes", "items", "add-to-collection", result.ParentKey, "--collection-name", collection); err != nil {
		info := ErrorInfoFrom(err)
		detail["status"] = "error"
		detail["error_type"] = fmt.Sprintf("%T", err)
		detail["error_class"] = info.Class
		if info.Hint != "" {
			detail["error_hint"] = info.Hint
		}
		if info.HTTPStatus != 0 {
			detail["error_http_status"] = info.HTTPStatus
		}
	} else {
		detail["status"] = "applied"
	}
	_ = s.Bundle.Jobs.RecordEvent(context.WithoutCancel(ctx), plan.JobID, "zotio.collection_filing", detail)
}

// enrichAutoImportedParent fills only missing DOI and abstract metadata after a
// successful policy-driven auto-import. It deliberately does not request any
// OA-PDF remediation or validation mode, and its failure cannot undo the import.
func (s *Service) enrichAutoImportedParent(ctx context.Context, plan *Plan, result *ApplyResult) {
	if !s.AutoEnrich || plan == nil || result == nil || result.Status != "applied" || result.ParentKey == "" {
		return
	}
	row, err := s.Bundle.Jobs.Get(ctx, plan.JobID)
	if err != nil || !row.Policy.AutoImport {
		return
	}
	detail := map[string]any{
		"parent_key": result.ParentKey,
		"summary":    "filled missing DOI and abstract metadata",
	}
	if _, err := s.CLI.RunJSON(ctx,
		"--agent", "--yes", "items", "enrich",
		"--missing-doi", "--missing-abstract", "--keys", result.ParentKey,
	); err != nil {
		info := ErrorInfoFrom(err)
		detail["status"] = "error"
		detail["summary"] = "metadata enrichment failed"
		detail["error_type"] = fmt.Sprintf("%T", err)
		detail["error_class"] = info.Class
		if info.Hint != "" {
			detail["error_hint"] = info.Hint
		}
		if info.HTTPStatus != 0 {
			detail["error_http_status"] = info.HTTPStatus
		}
	} else {
		detail["status"] = "applied"
	}
	_ = s.Bundle.Jobs.RecordEvent(context.WithoutCancel(ctx), plan.JobID, "zotio.enrich", detail)
}

// PlanAndApply creates an immutable plan for one ready job and immediately
// applies that exact plan. Both steps use the exports-ledger idempotency keys,
// so replays do not issue a second Zotero mutation.
func (s *Service) PlanAndApply(ctx context.Context, jobID string) (status, parentKey, attachmentKey string, err error) {
	plans, err := s.PlanJobs(ctx, []string{jobID})
	if err != nil {
		return "failed", "", "", err
	}
	if len(plans) != 1 || plans[0] == nil {
		return "failed", "", "", errors.New("planning Zotio auto-import returned no plan")
	}
	plan := plans[0]
	result, err := s.Apply(ctx, plan.ID, plan.ConfirmationSHA256)
	if result == nil {
		if err == nil {
			err = errors.New("applying Zotio auto-import returned no result")
		}
		return "failed", plan.ExpectedParentKey, "", err
	}
	return result.Status, result.ParentKey, result.AttachmentKey, err
}

// LoadPlan reads and verifies one private plan file by opaque ID.
func (s *Service) LoadPlan(planID string) (*Plan, error) {
	if !planIDRE.MatchString(planID) {
		return nil, fmt.Errorf("invalid Zotio plan ID %q", planID)
	}
	path := filepath.Join(s.DataDir, "zotio", "plans", planID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading Zotio plan: %w", err)
	}
	var plan Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("decoding Zotio plan: %w", err)
	}
	if plan.SchemaVersion != ZotioPlanSchemaVersion || plan.ID != planID {
		return nil, fmt.Errorf("Zotio plan identity mismatch")
	}
	digest, err := planDigest(&plan)
	if err != nil {
		return nil, err
	}
	if digest != plan.ConfirmationSHA256 {
		return nil, fmt.Errorf("Zotio plan confirmation digest mismatch")
	}
	return &plan, nil
}

func (s *Service) resolveManifest(ctx context.Context, plan *Plan) (string, importManifest, error) {
	stagingDir := filepath.Join(s.DataDir, "zotio", "staging", plan.JobID, plan.ArtifactSHA256)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return "", importManifest{}, err
	}
	name := url.PathEscape(strings.ToLower(plan.DOI)) + ".pdf"
	staged := filepath.Join(stagingDir, name)
	if err := materializePrivateFile(plan.ArtifactPath, staged, plan.ArtifactSHA256); err != nil {
		return "", importManifest{}, err
	}
	manifestJSON, err := s.CLI.RunJSON(ctx, "--agent", "import", "resolve", stagingDir)
	if err != nil {
		return "", importManifest{}, fmt.Errorf("resolving Zotio import manifest: %w", err)
	}
	var manifest importManifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return "", importManifest{}, fmt.Errorf("decoding Zotio import manifest: %w", err)
	}
	if len(manifest.Entries) != 1 {
		return "", importManifest{}, fmt.Errorf("Zotio resolver returned %d entries, want exactly one", len(manifest.Entries))
	}
	manifestDir := filepath.Join(s.DataDir, "zotio", "manifests")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		return "", importManifest{}, err
	}
	manifestPath := filepath.Join(manifestDir, plan.JobID+"-"+plan.ArtifactSHA256+".json")
	if err := atomicPrivateWrite(manifestPath, append(manifestJSON, '\n')); err != nil {
		return "", importManifest{}, err
	}
	return manifestPath, manifest, nil
}

func manifestRoute(manifest importManifest) (route, parent string, err error) {
	entry := manifest.Entries[0]
	if entry.Status != "resolved" {
		return "", "", fmt.Errorf("Zotio manifest entry is %q, not resolved", entry.Status)
	}
	switch {
	case entry.Action == "create" && entry.Classification == "new":
		return "manifest_create", "", nil
	case entry.Action == "attach" && entry.Classification == "attach_candidate" && keyRE.MatchString(entry.MatchedKey):
		return "manifest_attach", entry.MatchedKey, nil
	case entry.Action == "skip" && entry.Classification == "duplicate" && keyRE.MatchString(entry.MatchedKey):
		return "manifest_duplicate", entry.MatchedKey, nil
	default:
		return "", "", fmt.Errorf("unsupported Zotio manifest outcome action=%q classification=%q matched_key=%q", entry.Action, entry.Classification, entry.MatchedKey)
	}
}

func validatePreview(raw json.RawMessage) error {
	var envelope mutationEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decoding Zotio preview: %w", err)
	}
	if !envelope.OK || envelope.Mode != "preview" || envelope.Result != nil || envelope.Plan.Summary.Invalid != 0 {
		return fmt.Errorf("Zotio did not return a valid mutation preview")
	}
	if envelope.Plan.Summary.Planned+envelope.Plan.Summary.NoOp < 1 {
		return fmt.Errorf("Zotio preview contains no operation")
	}
	return nil
}

func decodeApply(raw json.RawMessage) (*mutationEnvelope, error) {
	var envelope mutationEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decoding Zotio apply result: %w", err)
	}
	if !envelope.OK || envelope.Mode != "apply" || envelope.Result == nil {
		return nil, fmt.Errorf("Zotio did not return a successful apply result")
	}
	if envelope.Result.Summary.Failed != 0 || envelope.Result.Summary.Conflicts != 0 {
		return nil, fmt.Errorf("Zotio apply reported %d failed and %d conflicted operations", envelope.Result.Summary.Failed, envelope.Result.Summary.Conflicts)
	}
	return &envelope, nil
}

// mutationFailure extracts Zotio's structured, known mutation outcome. A
// non-zero Zotio exit can still carry the exact safe reason (quota, conflict,
// validation, and so on); persisting it distinguishes a completed failure from
// an ambiguous transport loss.
func mutationFailure(raw json.RawMessage) (string, bool) {
	var envelope mutationEnvelope
	if len(raw) == 0 || json.Unmarshal(raw, &envelope) != nil || envelope.Mode != "apply" || envelope.Result == nil {
		return "", false
	}
	summary := envelope.Result.Summary
	if envelope.OK && summary.Failed == 0 && summary.Conflicts == 0 {
		return "", false
	}
	var reasons []string
	for _, item := range envelope.Result.Items {
		if item.Status != "failed" && item.Status != "conflict" {
			continue
		}
		if reason := mutationReason(item.Reason); reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if len(reasons) != 0 {
		return strings.Join(reasons, "; "), true
	}
	return fmt.Sprintf("Zotio reported %d failed and %d conflicted operations", summary.Failed, summary.Conflicts), true
}

func mutationReason(value any) string {
	switch reason := value.(type) {
	case string:
		return strings.TrimSpace(reason)
	case map[string]any:
		for _, key := range []string{"message", "error", "reason"} {
			if text, ok := reason[key].(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		encoded, err := json.Marshal(reason)
		if err == nil && string(encoded) != "{}" {
			return string(encoded)
		}
	}
	return ""
}

func (s *Service) requirePlanServices() error {
	if s == nil || s.CLI == nil || s.Bundle == nil || s.Store == nil || strings.TrimSpace(s.DataDir) == "" {
		return fmt.Errorf("Zotio plan/apply integration is not configured")
	}
	return nil
}

func (s *Service) attachmentMode() string {
	if strings.TrimSpace(s.AttachmentMode) == "linked-file" {
		return "linked-file"
	}
	return "stored"
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) writePlan(plan *Plan) (string, error) {
	dir := filepath.Join(s.DataDir, "zotio", "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, plan.ID+".json")
	data, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return "", err
	}
	return path, atomicPrivateWrite(path, append(data, '\n'))
}

func planDigest(plan *Plan) (string, error) {
	copy := *plan
	copy.ConfirmationSHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (s *Service) recordedPlan(ctx context.Context, key string) (*Plan, error) {
	var path string
	err := s.Store.DB().QueryRowContext(ctx,
		`SELECT path FROM exports WHERE kind = 'zotio_plan' AND idempotency_key = ?`, key).Scan(&path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if filepath.Dir(path) != filepath.Join(s.DataDir, "zotio", "plans") {
		return nil, fmt.Errorf("recorded Zotio plan path is outside the private plan directory")
	}
	return s.LoadPlan(strings.TrimSuffix(filepath.Base(path), ".json"))
}

func (s *Service) recordPlan(ctx context.Context, key, path string, plan *Plan) (*Plan, error) {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	if _, err := s.Store.DB().ExecContext(ctx,
		`INSERT INTO exports (job_id, kind, idempotency_key, path, result_json, created_at)
		 VALUES (?, 'zotio_plan', ?, ?, ?, ?)
		 ON CONFLICT(idempotency_key) DO NOTHING`,
		plan.JobID, key, path, string(encoded), store.Now()); err != nil {
		return nil, err
	}
	// A concurrent PlanJobs for the same key may have inserted first; the plain
	// INSERT would otherwise fail on the unique idempotency_key. Re-read and
	// return the canonical recorded plan so duplicate callers converge on one
	// plan instead of erroring or diverging on plan ID.
	recorded, err := s.recordedPlan(ctx, key)
	if err != nil {
		return nil, err
	}
	if recorded == nil {
		return plan, nil
	}
	return recorded, nil
}

func (s *Service) recordFailedApplyAndInvalidatePlan(ctx context.Context, key string, plan *Plan, zotio json.RawMessage, applyErr error) error {
	info := ClassifyError(applyErr, zotio)
	result := &ApplyResult{
		PlanID:    plan.ID,
		JobID:     plan.JobID,
		Status:    "failed",
		ParentKey: plan.ExpectedParentKey,
		AppliedAt: s.now().UTC().Format(time.RFC3339),
		Error:     info.Hint,
		Zotio:     zotio,
	}
	if err := s.recordApply(ctx, key, result); err != nil {
		return WithErrorInfo(fmt.Errorf("recording failed Zotio apply: %w", applyErr), zotio)
	}
	if err := s.invalidatePlan(ctx, plan); err != nil {
		return WithErrorInfo(fmt.Errorf("%w (invalidating cached Zotio plan: %w)", applyErr, err), zotio)
	}
	return WithErrorInfo(applyErr, zotio)
}

func (s *Service) invalidatePlan(ctx context.Context, plan *Plan) error {
	planPath := filepath.Join(s.DataDir, "zotio", "plans", plan.ID+".json")
	manifestPath := plan.ManifestPath
	if manifestPath != "" && filepath.Dir(manifestPath) != filepath.Join(s.DataDir, "zotio", "manifests") {
		return fmt.Errorf("Zotio manifest path is outside the private manifest directory")
	}

	tx, err := s.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM exports WHERE kind = 'zotio_plan' AND path = ?`, planPath); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	for _, path := range []string{planPath, manifestPath} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Service) recordedApply(ctx context.Context, key string) (*ApplyResult, error) {
	var raw sql.NullString
	err := s.Store.DB().QueryRowContext(ctx,
		`SELECT result_json FROM exports WHERE kind = 'zotio_apply' AND idempotency_key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		// A legacy reservation may have been recorded before claims carried an
		// explicit in-progress status. claimApply replaces it below.
		return nil, nil
	}
	var result ApplyResult
	if err := json.Unmarshal([]byte(raw.String), &result); err != nil {
		return nil, fmt.Errorf("decoding recorded Zotio apply: %w", err)
	}
	if result.Status == "in_progress" || result.Status == "failed" {
		// A failed apply is replayable; an in-progress apply remains owned by
		// another worker and is surfaced as a conflict by Apply.
		return nil, nil
	}
	return &result, nil
}

func (s *Service) claimApply(ctx context.Context, key, jobID string) (bool, error) {
	result, err := s.Store.DB().ExecContext(ctx, `
		INSERT INTO exports (job_id, kind, idempotency_key, result_json, created_at)
		VALUES (?, 'zotio_apply', ?, '{"status":"in_progress"}', ?)
		ON CONFLICT(idempotency_key) DO UPDATE SET
			job_id = excluded.job_id,
			kind = excluded.kind,
			path = NULL,
			result_json = excluded.result_json,
			created_at = excluded.created_at
		WHERE exports.kind = 'zotio_apply' AND (
			exports.result_json IS NULL OR json_extract(exports.result_json, '$.status') = 'failed'
		)`,
		jobID, key, store.Now())
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *Service) recordApply(ctx context.Context, key string, result *ApplyResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	updated, err := s.Store.DB().ExecContext(ctx,
		`UPDATE exports SET result_json = ? WHERE kind = 'zotio_apply' AND idempotency_key = ?
			AND json_extract(result_json, '$.status') = 'in_progress'`,
		string(raw), key)
	if err != nil {
		return err
	}
	changed, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("Zotio apply reservation was not finalized")
	}
	return nil
}

func atomicPrivateWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".papio-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func materializePrivateFile(source, target, expectedSHA string) error {
	if err := verifyFileSHA256(target, expectedSHA); err == nil {
		return nil
	}
	_ = os.Remove(target)
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	copyErr := func() error {
		if _, err := io.Copy(out, in); err != nil {
			return err
		}
		return out.Sync()
	}()
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(target)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return closeErr
	}
	return verifyFileSHA256(target, expectedSHA)
}

func verifyFileSHA256(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("SHA-256 %s, want %s", actual, expected)
	}
	return nil
}

func stringField(values any, key string) string {
	fields, _ := values.(map[string]any)
	value, _ := fields[key].(string)
	return value
}
