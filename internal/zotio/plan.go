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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"papio/internal/job"
	"papio/internal/store"
	"papio/internal/work"
)

const (
	ZotioPlanSchemaVersion = "papio-zotio-plan/1"
	applyClaimLease        = 15 * time.Minute
)

var planIDRE = regexp.MustCompile(`^zplan_[a-f0-9]{26}$`)

var (
	materializeLocksMu sync.Mutex
	materializeLocks   = make(map[string]*sync.Mutex)
)

func pathLock(path string) *sync.Mutex {
	materializeLocksMu.Lock()
	defer materializeLocksMu.Unlock()
	if mu, ok := materializeLocks[path]; ok {
		return mu
	}
	mu := &sync.Mutex{}
	materializeLocks[path] = mu
	return mu
}

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
	CollectionIsKey    bool            `json:"collection_is_key,omitempty"`
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
	// The route belongs in the key, and the two branches below ask for different
	// ones, so it is resolved before the lookup rather than inside them.
	planRoute := newItemRoute
	if acquisition.ZotioItemKey != "" {
		planRoute = existingItemRoute(attachmentMode)
	}
	idempotencyKey := planIdempotencyKey(jobID, acquisition.Artifact.SHA256, attachmentMode, collection, planRoute)
	if existing, err := s.recordedPlan(ctx, idempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		stale, err := s.planManifestUnresolved(existing)
		if err != nil {
			return nil, err
		}
		if stale {
			// Safe to invalidate: an unresolved manifest entry means Zotio
			// selected zero import operations (selected=0, planned=0), so no
			// Zotero mutation was attempted for this cached routing decision.
			// That is unlike ambiguous apply, where the mutation may have
			// succeeded before the deadline and invalidating would let PlanJobs
			// re-derive a fresh mutation for the same file, risking duplicate
			// attachment or duplicate library entry.
			if err := s.invalidatePlan(ctx, existing); err != nil {
				return nil, err
			}
		} else {
			return existing, nil
		}
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
		if !keyRE.MatchString(acquisition.ZotioItemKey) {
			return nil, fmt.Errorf("invalid Zotero item key %q", acquisition.ZotioItemKey)
		}
		// Missing-PDF queue jobs carry an existing Zotio item and a collection
		// key. Direct acquisitions normally have no item key, so a key-shaped
		// collection name remains a name and is filed. The unsupported case of
		// supplying both an explicit item key and a key-shaped collection name
		// remains ambiguous.
		plan.CollectionIsKey = keyRE.MatchString(collection)
		plan.Route = "existing_item"
		plan.ExpectedParentKey = acquisition.ZotioItemKey
		// "stored" mode here used to have no route to choose: attaching to an
		// item that already exists went through the Zotero Web API, which
		// uploads into Zotero's own file storage and consumes that plan whatever
		// the operator configured inside Zotero. On a WebDAV library zotio
		// refused it outright, so 13 of 161 plans on the machine where this was
		// measured had nowhere to go. The connector route closes that: zotio
		// creates a throwaway parent plus the file in one desktop session, moves
		// the attachment onto this item, and trashes the throwaway.
		//
		// A preview creates nothing and needs no running connector, so the
		// two-phase plan shape is unchanged; the apply needs Zotero desktop.
		attach := []string{"attachments", "add", acquisition.ZotioItemKey, artifactPath, "--mode", attachmentMode}
		if planRoute != "" {
			attach = append(attach, "--via", planRoute)
		}
		plan.PreviewArgs = append([]string{"--agent"}, attach...)
		plan.ApplyArgs = append([]string{"--agent", "--yes"}, attach...)
	} else {
		if !hasNewItemRoutingIdentifier(row.Work) {
			return nil, errors.New(newItemRoutingRefusal)
		}
		if err := s.CLI.Sync(ctx); err != nil {
			return nil, fmt.Errorf("refreshing Zotio library before deduplication: %w", err)
		}
		manifestPath, manifest, err := s.resolveManifest(ctx, plan, row.Work)
		if err != nil {
			return nil, err
		}
		plan.ManifestPath = manifestPath
		plan.Route, plan.ExpectedParentKey, err = manifestRoute(manifest)
		if err != nil {
			return nil, err
		}
		// "auto" prefers the local Zotero desktop and falls back to
		// api.zotero.org when it is not reachable. Forcing "web" here uploaded
		// every attachment into Zotero's own file storage, which ignores
		// whatever file storage the operator configured in Zotero — a WebDAV
		// server, for instance. On this machine that quietly consumed a 300 MB
		// plan the operator does not use and had not chosen, and once it filled
		// no paper could be filed at all. The desktop route hands the file to
		// Zotero, so Zotero puts it wherever the operator told it to.
		plan.PreviewArgs = []string{"--agent", "--via", newItemRoute, "import", "apply", manifestPath, "--attach-mode", attachmentMode}
		plan.ApplyArgs = []string{"--agent", "--yes", "--via", newItemRoute, "import", "apply", manifestPath, "--attach-mode", attachmentMode}
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
	ledgerCtx := context.WithoutCancel(ctx)
	if stale, err := s.planManifestUnresolved(plan); err != nil {
		return nil, err
	} else if stale {
		// Same nothing-attempted reasoning as the cached-plan path in planJob:
		// unresolved means Zotio never selected an import operation for this
		// manifest, so invalidating cannot discard a partial Zotero write.
		if err := s.invalidatePlan(ledgerCtx, plan); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("cached Zotio plan %s references an unresolved manifest; replan required", plan.ID)
	}
	idempotencyKey := "zotio_apply:" + plan.ID + ":" + confirmation
	if existing, err := s.recordedApply(ledgerCtx, idempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.Status == "ambiguous" {
			return nil, s.ambiguousReplayError(existing, plan.ID)
		}
		s.fileCollection(ctx, plan, existing)
		if err := s.markImported(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}
	claimed, err := s.claimApply(ledgerCtx, idempotencyKey, plan.JobID)
	if err != nil {
		return nil, err
	}
	if !claimed {
		result, recordErr := s.recordedApply(ledgerCtx, idempotencyKey)
		if recordErr != nil {
			return nil, recordErr
		}
		if result != nil {
			if result.Status == "ambiguous" {
				return nil, s.ambiguousReplayError(result, plan.ID)
			}
			s.fileCollection(ctx, plan, result)
			if err := s.markImported(ctx, result); err != nil {
				return nil, err
			}
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
		if errors.Is(commandErr, context.DeadlineExceeded) || errors.Is(commandErr, context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, s.recordAmbiguousApply(ctx, idempotencyKey, plan, out, applyErr)
		}
		return nil, s.recordFailedApplyAndInvalidatePlan(ctx, idempotencyKey, plan, out, applyErr)
	}
	envelope, err := decodeApply(out)
	if err != nil {
		applyErr := fmt.Errorf("decoding Zotio apply result: %w", err)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, s.recordAmbiguousApply(ctx, idempotencyKey, plan, out, applyErr)
		}
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
		if envelope.Result.Summary.Applied > 0 {
			return nil, s.recordFailedApplyKeepingPlan(ctx, idempotencyKey, plan, out, applyErr)
		}
		return nil, s.recordFailedApplyAndInvalidatePlan(ctx, idempotencyKey, plan, out, applyErr)
	}
	if envelope.Result.Summary.Applied > 0 && result.AttachmentKey == "" {
		applyErr := errors.New("Zotio apply succeeded without returning an attachment key")
		return nil, s.recordFailedApplyKeepingPlan(ctx, idempotencyKey, plan, out, applyErr)
	}
	if err := s.recordApply(ctx, idempotencyKey, result); err != nil {
		return nil, err
	}
	if err := s.markImported(ctx, result); err != nil {
		return nil, err
	}
	s.fileCollection(ctx, plan, result)
	s.enrichAutoImportedParent(ctx, plan, result)
	return result, nil
}

// markImported advances the acquisition lifecycle once the Zotero write is
// durably recorded: ready -> imported, carrying the item keys in the
// transition detail so reports need no exports-table access. A conflict means
// another actor (or an earlier replay) already moved the job — fine either
// way, the exports ledger stays the durable source of truth.
func (s *Service) markImported(ctx context.Context, result *ApplyResult) error {
	if result == nil || result.JobID == "" {
		return nil
	}
	switch result.Status {
	case "applied", "no_op", "duplicate":
	default:
		return nil
	}
	err := s.Bundle.Jobs.Transition(ctx, result.JobID, job.StateReady, job.StateImported, map[string]any{
		"status":         result.Status,
		"parent_key":     result.ParentKey,
		"attachment_key": result.AttachmentKey,
	})
	if errors.Is(err, job.ErrConflict) {
		return nil
	}
	return err
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
	if plan.CollectionIsKey {
		// A missing-PDF queue filter is a Zotero collection key. The existing
		// parent already belongs to that collection; filing accepts names only.
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
	if status, parentKey, skip, err := s.skipOwnedReadyImport(ctx, jobID); skip || err != nil {
		return status, parentKey, "", err
	}
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

// skipOwnedReadyImport short-circuits auto-import when Zotio's mirror already
// holds a parent item with a PDF for the job's identifiers. This avoids bundle
// export for DOI-only requests and records a duplicate outcome instead of an
// error when the library already has the paper.
func (s *Service) skipOwnedReadyImport(ctx context.Context, jobID string) (status, parentKey string, skip bool, err error) {
	if s == nil || s.CLI == nil || s.Bundle == nil {
		return "", "", false, nil
	}
	row, err := s.Bundle.Jobs.Get(ctx, jobID)
	if err != nil {
		return "", "", false, err
	}
	// A job that already names a Zotero item was excluded here, on the reading
	// that the existing-item route would settle it. That route is exactly what
	// cannot settle it: "zotio attachments add" is Web-API-only, so on a
	// library whose files live on the operator's own file store Zotero refuses
	// every stored upload, and an item that ALREADY holds the paper's PDF has
	// nothing to upload in the first place. Measured on the operator's library:
	// papers whose item carried papio's own artifact — the attachment filename
	// byte-equal to the job's artifact SHA-256 — were re-attached and refused
	// on every pass, indefinitely, because ready is terminal.
	if key := strings.TrimSpace(row.ZotioItemKey); key != "" {
		holding, known := s.itemsHoldingPDF(ctx, []string{key})
		if !known || !holding[key] {
			return "", "", false, nil
		}
		result := &ApplyResult{JobID: jobID, Status: "duplicate", ParentKey: key}
		if err := s.markImported(ctx, result); err != nil {
			return "", key, false, err
		}
		return "duplicate", key, true, nil
	}
	lookupWork := LookupWorkFrom(row.Work)
	if lookupWork.DOI == "" && lookupWork.ArXiv == "" && lookupWork.PMID == "" && lookupWork.ISBN == "" {
		return "", "", false, nil
	}
	lookup, err := s.LookupWorks(ctx, LookupWorksRequest{Works: []LookupWork{lookupWork}})
	if err != nil {
		return "", "", false, nil
	}
	if len(lookup.Works) != 1 || lookup.Works[0].Status != OwnershipOwnedWithPDF {
		return "", "", false, nil
	}
	parentKey = lookup.Works[0].ItemKey
	result := &ApplyResult{JobID: jobID, Status: "duplicate", ParentKey: parentKey}
	if err := s.markImported(ctx, result); err != nil {
		return "", parentKey, false, err
	}
	return "duplicate", parentKey, true, nil
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

func (s *Service) resolveManifest(ctx context.Context, plan *Plan, w work.Work) (string, importManifest, error) {
	stagingDir := filepath.Join(s.DataDir, "zotio", "staging", plan.JobID, plan.ArtifactSHA256)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return "", importManifest{}, err
	}
	name, err := importStagingBasename(w)
	if err != nil {
		return "", importManifest{}, err
	}
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
	if err := s.removeStaleUnresolvedManifest(manifestPath); err != nil {
		return "", importManifest{}, err
	}
	if err := atomicPrivateWrite(manifestPath, append(manifestJSON, '\n')); err != nil {
		return "", importManifest{}, err
	}
	return manifestPath, manifest, nil
}

func manifestIsUnresolved(manifest importManifest) bool {
	if len(manifest.Entries) != 1 {
		return true
	}
	return manifest.Entries[0].Status != "resolved"
}

func (s *Service) loadManifestAt(path string) (importManifest, error) {
	if path == "" {
		return importManifest{}, fmt.Errorf("empty Zotio manifest path")
	}
	if filepath.Dir(path) != filepath.Join(s.DataDir, "zotio", "manifests") {
		return importManifest{}, fmt.Errorf("Zotio manifest path is outside the private manifest directory")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return importManifest{}, err
	}
	var manifest importManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return importManifest{}, fmt.Errorf("decoding Zotio import manifest: %w", err)
	}
	return manifest, nil
}

func (s *Service) planManifestUnresolved(plan *Plan) (bool, error) {
	if plan == nil || plan.ManifestPath == "" {
		return false, nil
	}
	manifest, err := s.loadManifestAt(plan.ManifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return manifestIsUnresolved(manifest), nil
}

func (s *Service) removeStaleUnresolvedManifest(path string) error {
	manifest, err := s.loadManifestAt(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !manifestIsUnresolved(manifest) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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

// recordFailedApplyKeepingPlan records a replayable failure and deliberately
// leaves the cached plan in place. It exists for a failure that arrives AFTER
// zotio reports the mutation applied: the library already changed, so the only
// safe retry is a replay of the identical mutation, which zotio recognises and
// answers as a no-op. Invalidating here would let PlanJobs derive a fresh
// manifest and issue a second create, and zotio's own de-duplication would be
// the only thing standing between the operator and a duplicate paper.
//
// Measured live 2026-08-22: six connector-route creates each reported
// "applied": 1 with an empty item key, and each replay answered no_op carrying
// the key, so the retry converges without a second mutation.
func (s *Service) recordFailedApplyKeepingPlan(ctx context.Context, key string, plan *Plan, zotio json.RawMessage, applyErr error) error {
	if err := s.recordApply(context.WithoutCancel(ctx), key, failedApplyResult(s, plan, zotio, applyErr)); err != nil {
		return WithErrorInfo(fmt.Errorf("recording failed Zotio apply: %w", applyErr), zotio)
	}
	return WithErrorInfo(applyErr, zotio)
}

func (s *Service) recordFailedApplyAndInvalidatePlan(ctx context.Context, key string, plan *Plan, zotio json.RawMessage, applyErr error) error {
	durableCtx := context.WithoutCancel(ctx)
	if err := s.recordApply(durableCtx, key, failedApplyResult(s, plan, zotio, applyErr)); err != nil {
		return WithErrorInfo(fmt.Errorf("recording failed Zotio apply: %w", applyErr), zotio)
	}
	if err := s.invalidatePlan(durableCtx, plan); err != nil {
		return WithErrorInfo(fmt.Errorf("%w (invalidating cached Zotio plan: %w)", applyErr, err), zotio)
	}
	return WithErrorInfo(applyErr, zotio)
}

func failedApplyResult(s *Service, plan *Plan, zotio json.RawMessage, applyErr error) *ApplyResult {
	return &ApplyResult{
		PlanID:    plan.ID,
		JobID:     plan.JobID,
		Status:    "failed",
		ParentKey: plan.ExpectedParentKey,
		AppliedAt: s.now().UTC().Format(time.RFC3339),
		Error:     ClassifyError(applyErr, zotio).Hint,
		Zotio:     zotio,
	}
}

func (s *Service) recordAmbiguousApply(ctx context.Context, key string, plan *Plan, zotio json.RawMessage, applyErr error) error {
	info := ClassifyError(applyErr, zotio)
	result := &ApplyResult{
		PlanID:    plan.ID,
		JobID:     plan.JobID,
		Status:    "ambiguous",
		ParentKey: plan.ExpectedParentKey,
		AppliedAt: s.now().UTC().Format(time.RFC3339),
		Error:     info.Hint,
		Zotio:     zotio,
	}
	durableCtx := context.WithoutCancel(ctx)
	if err := s.recordApply(durableCtx, key, result); err != nil {
		return WithErrorInfo(fmt.Errorf("recording ambiguous Zotio apply: %w", applyErr), zotio)
	}
	// Leave the plan intact: the mutation may have succeeded before the deadline
	// so invalidating would let PlanJobs re-derive a fresh mutation for the same
	// file, risking duplicate attachment or duplicate library entry. A follow-up
	// PlanJobs is blocked by the ambiguous reservation until reconciliation
	// completes and the verification either records success or reopens retry.
	return WithErrorInfo(applyErr, zotio)
}
func (s *Service) ambiguousReplayError(result *ApplyResult, planID string) error {
	// The durable ambiguous record holds only the redacted hint (e.g.
	// "Zotio command canceled" / "Zotio command timed out"). Rebuild an error
	// that preserves the original context sentinel so callers remain able to
	// detect ambiguous/cancellable failures with errors.Is(..., context.Canceled)
	// or errors.Is(..., context.DeadlineExceeded) across the replay path.
	// Declared without a value: every arm below assigns one, and seeding it with
	// context.Canceled hid that from the reader and from the linter.
	var sentinel error
	switch {
	case strings.Contains(result.Error, "timed out"):
		sentinel = context.DeadlineExceeded
	case strings.Contains(result.Error, "canceled") || result.Error == "":
		sentinel = context.Canceled
	default:
		// Fall back to a generic ambiguous sentinel that ClassifyError will
		// map back onto the same class the first write recorded, even for
		// cancellation flavours that the hint text does not fully capture.
		sentinel = context.Canceled
	}
	base := fmt.Errorf("applying Zotio mutation: %s", result.Error)
	if strings.TrimSpace(result.Error) == "" {
		base = fmt.Errorf("Zotio apply is ambiguous: reconciliation required before retry for plan %s", planID)
	}
	return WithErrorInfo(fmt.Errorf("%w: %w", sentinel, base), result.Zotio)
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
	if result.Status == "ambiguous" {
		// An ambiguous apply (timeout/cancellation) may have mutated Zotero.
		// Surface it as a finished outcome so Apply replays the error rather
		// than silently reissuing the mutation. It remains non-reclaimable by
		// claimApply until explicitly reconciled or expired.
		return &result, nil
	}
	if result.Status == "in_progress" || result.Status == "failed" {
		// A failed apply is replayable; an in-progress apply remains owned by
		// another worker and is surfaced as a conflict by Apply.
		return nil, nil
	}
	return &result, nil
}

func (s *Service) claimApply(ctx context.Context, key, jobID string) (bool, error) {
	now := s.now().UTC()
	leaseExpiry := now.Add(-applyClaimLease).Unix()
	result, err := s.Store.DB().ExecContext(ctx, `
		INSERT INTO exports (job_id, kind, idempotency_key, result_json, created_at)
		VALUES (?, 'zotio_apply', ?, json_object('status', 'in_progress', 'claimed_at', ?), ?)
		ON CONFLICT(idempotency_key) DO UPDATE SET
			job_id = excluded.job_id,
			kind = excluded.kind,
			path = NULL,
			result_json = excluded.result_json,
			created_at = excluded.created_at
		WHERE exports.kind = 'zotio_apply' AND (
			exports.result_json IS NULL
			OR json_extract(exports.result_json, '$.status') = 'failed'
			OR (
				json_extract(exports.result_json, '$.status') = 'in_progress'
				AND CAST(json_extract(exports.result_json, '$.claimed_at') AS INTEGER) <= ?
			)
		)`,
		jobID, key, now.Unix(), store.Now(), leaseExpiry)
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
	mu := pathLock(target)
	mu.Lock()
	defer mu.Unlock()
	if err := verifyFileSHA256(target, expectedSHA); err == nil {
		return nil
	}
	// Do not blindly remove a file that may be mid-create by a raced caller;
	// our lock already serializes callers holding the same target path.
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".papio-stage-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
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
	if err := verifyFileSHA256(tmpName, expectedSHA); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	// Re-verify through the final path name to guard against a last-moment
	// replacement that an atomic rename still serializes away on POSIX.
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
