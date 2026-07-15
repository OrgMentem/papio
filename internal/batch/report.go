// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package batch owns durable CLI batch manifests and the joined batch digest.
package batch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"papio/internal/job"
	"papio/internal/protocol"
)

const SchemaVersion = "papio-batch-manifest/1"

var idPattern = regexp.MustCompile(`^batch-[0-9a-f]{8}$`)

// Manifest records one batch submission independently of ephemeral daemon state.
type Manifest struct {
	SchemaVersion string         `json:"schema_version"`
	ID            string         `json:"id"`
	CreatedAt     string         `json:"created_at"`
	Label         string         `json:"label,omitempty"`
	Collection    string         `json:"collection,omitempty"`
	Works         []ManifestWork `json:"works"`
}

// ManifestWork records both the original work and how it entered the batch.
type ManifestWork struct {
	RequestID string               `json:"request_id"`
	JobID     string               `json:"job_id,omitempty"`
	Status    string               `json:"status"`
	Work      protocol.WorkRequest `json:"work"`
	Error     string               `json:"error,omitempty"`
}

// Report is a live digest formed by joining a manifest with durable jobs/events.
type Report struct {
	BatchID    string        `json:"batch_id"`
	CreatedAt  string        `json:"created_at"`
	Label      string        `json:"label,omitempty"`
	Collection string        `json:"collection,omitempty"`
	Summary    ReportSummary `json:"summary"`
	Works      []ReportWork  `json:"works"`
}

type ReportSummary struct {
	Total    int            `json:"total"`
	Outcomes map[string]int `json:"outcomes"`
}

// ReportWork carries a machine-readable outcome and only the durable detail
// needed for a user or agent to decide its next action.
type ReportWork struct {
	RequestID     string               `json:"request_id"`
	JobID         string               `json:"job_id,omitempty"`
	Work          protocol.WorkRequest `json:"work"`
	Outcome       string               `json:"outcome"`
	Reason        string               `json:"reason,omitempty"`
	FailureClass  string               `json:"failure_class,omitempty"`
	ParentKey     string               `json:"parent_key,omitempty"`
	AttachmentKey string               `json:"attachment_key,omitempty"`
	Collection    string               `json:"collection,omitempty"`
}

// Jobs is the narrow durable state dependency needed to build a report.
type Jobs interface {
	Get(context.Context, string) (*job.Row, error)
	Events(context.Context, string) ([]map[string]any, error)
	ListHumanActions(context.Context, bool) ([]job.HumanAction, error)
}

// NewManifest returns a stable batch identity for the date and sorted work
// identity set. The caller assigns per-work request IDs using RequestID.
func NewManifest(requests []protocol.WorkRequest, label, collection string, now time.Time) *Manifest {
	id := ID(requests, now)
	works := make([]ManifestWork, len(requests))
	for i, request := range requests {
		request.RequestID = RequestID(id, request)
		request.Collection = strings.TrimSpace(collection)
		works[i] = ManifestWork{RequestID: request.RequestID, Status: "submitted", Work: request}
	}
	return &Manifest{
		SchemaVersion: SchemaVersion,
		ID:            id,
		CreatedAt:     now.UTC().Format(time.RFC3339),
		Label:         strings.TrimSpace(label),
		Collection:    strings.TrimSpace(collection),
		Works:         works,
	}
}

// ID derives a deterministic identity from the local calendar date and the
// sorted set of canonical work identities. Duplicate inputs do not alter it.
func ID(requests []protocol.WorkRequest, now time.Time) string {
	identities := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		identities[workIdentity(request)] = struct{}{}
	}
	values := make([]string, 0, len(identities))
	for identity := range identities {
		values = append(values, identity)
	}
	sort.Strings(values)
	sum := sha256.Sum256([]byte(now.Format("2006-01-02") + "\n" + strings.Join(values, "\n")))
	return "batch-" + hex.EncodeToString(sum[:4])
}

// RequestID supplies a deterministic, unique-within-batch idempotency key.
func RequestID(batchID string, request protocol.WorkRequest) string {
	sum := sha256.Sum256([]byte(workIdentity(request)))
	return batchID + "-" + hex.EncodeToString(sum[:4])
}

func workIdentity(request protocol.WorkRequest) string {
	if ids := request.Identifiers; ids != nil {
		parts := make([]string, 0, 5)
		if ids.DOI != "" {
			parts = append(parts, "doi:"+ids.DOI)
		}
		if ids.PMID != "" {
			parts = append(parts, "pmid:"+ids.PMID)
		}
		if ids.ArXiv != "" {
			parts = append(parts, "arxiv:"+ids.ArXiv)
		}
		if ids.ISBN != "" {
			parts = append(parts, "isbn:"+ids.ISBN)
		}
		if ids.OpenAlex != "" {
			parts = append(parts, "openalex:"+ids.OpenAlex)
		}
		if len(parts) != 0 {
			sort.Strings(parts)
			return strings.Join(parts, "\n")
		}
	}
	return fmt.Sprintf("title:%s\nauthors:%s\nyear:%d", strings.TrimSpace(request.Title), strings.Join(request.Authors, "\x00"), request.Year)
}

func directory(dataDir string) string { return filepath.Join(dataDir, "batches") }

func path(dataDir, id string) string { return filepath.Join(directory(dataDir), id+".json") }

// Write persists a private manifest atomically.
func Write(dataDir string, manifest *Manifest) error {
	if manifest == nil || !idPattern.MatchString(manifest.ID) || manifest.SchemaVersion != SchemaVersion {
		return errors.New("invalid batch manifest")
	}
	if err := os.MkdirAll(directory(dataDir), 0o700); err != nil {
		return fmt.Errorf("creating batch manifest directory: %w", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding batch manifest: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(directory(dataDir), ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("creating batch manifest: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protecting batch manifest: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("writing batch manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("closing batch manifest: %w", err)
	}
	if err := os.Rename(temporaryPath, path(dataDir, manifest.ID)); err != nil {
		return fmt.Errorf("publishing batch manifest: %w", err)
	}
	return nil
}

// Load reads one exact batch ID, or the most recently created manifest for
// "latest". IDs are deliberately constrained before building a filesystem path.
func Load(dataDir, requested string) (*Manifest, error) {
	requested = strings.TrimSpace(requested)
	if requested == "latest" {
		entries, err := os.ReadDir(directory(dataDir))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("no batch manifests found")
			}
			return nil, fmt.Errorf("listing batch manifests: %w", err)
		}
		var latest *Manifest
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if !idPattern.MatchString(id) {
				continue
			}
			manifest, err := loadPath(path(dataDir, id))
			if err != nil {
				return nil, err
			}
			if latest == nil || manifest.CreatedAt > latest.CreatedAt || (manifest.CreatedAt == latest.CreatedAt && manifest.ID > latest.ID) {
				latest = manifest
			}
		}
		if latest == nil {
			return nil, fmt.Errorf("no batch manifests found")
		}
		return latest, nil
	}
	if !idPattern.MatchString(requested) {
		return nil, fmt.Errorf("invalid batch ID %q", requested)
	}
	return loadPath(path(dataDir, requested))
}

func loadPath(manifestPath string) (*Manifest, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("batch manifest not found")
		}
		return nil, fmt.Errorf("reading batch manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decoding batch manifest: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion || !idPattern.MatchString(manifest.ID) {
		return nil, errors.New("invalid batch manifest")
	}
	return &manifest, nil
}

// BuildReport joins manifest entries against current durable job rows, events,
// and open actions. It never treats a missing job as a daemon failure: reports
// must remain usable after local database cleanup.
func BuildReport(ctx context.Context, manifest *Manifest, jobs Jobs) (*Report, error) {
	if manifest == nil || jobs == nil {
		return nil, errors.New("batch report requires a manifest and job store")
	}
	actions, err := jobs.ListHumanActions(ctx, false)
	if err != nil {
		return nil, err
	}
	actionsByJob := make(map[string][]job.HumanAction, len(actions))
	for _, action := range actions {
		actionsByJob[action.JobID] = append(actionsByJob[action.JobID], action)
	}
	report := &Report{
		BatchID: manifest.ID, CreatedAt: manifest.CreatedAt, Label: manifest.Label, Collection: manifest.Collection,
		Summary: ReportSummary{Total: len(manifest.Works), Outcomes: map[string]int{}},
		Works:   make([]ReportWork, 0, len(manifest.Works)),
	}
	for _, manifestWork := range manifest.Works {
		item, err := buildWorkReport(ctx, manifestWork, jobs, actionsByJob[manifestWork.JobID])
		if err != nil {
			return nil, err
		}
		report.Works = append(report.Works, item)
		report.Summary.Outcomes[item.Outcome]++
	}
	return report, nil
}

func buildWorkReport(ctx context.Context, manifestWork ManifestWork, jobs Jobs, actions []job.HumanAction) (ReportWork, error) {
	item := ReportWork{RequestID: manifestWork.RequestID, JobID: manifestWork.JobID, Work: manifestWork.Work, Collection: manifestWork.Work.Collection}
	switch manifestWork.Status {
	case "skipped_owned":
		item.Outcome = "skipped_owned"
		return item, nil
	case "submission_failed":
		item.Outcome, item.FailureClass = "failed", "submission"
		return item, nil
	case "submitted", "existing_item_attached":
	default:
		return item, fmt.Errorf("unknown manifest work status %q", manifestWork.Status)
	}
	if manifestWork.JobID == "" {
		item.Outcome, item.FailureClass = "failed", "missing_job"
		return item, nil
	}
	row, err := jobs.Get(ctx, manifestWork.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			item.Outcome, item.FailureClass = "failed", "missing_job"
			return item, nil
		}
		return item, err
	}
	if row == nil {
		item.Outcome, item.FailureClass = "failed", "missing_job"
		return item, nil
	}
	if row.Policy.Collection != "" {
		item.Collection = row.Policy.Collection
	}
	events, err := jobs.Events(ctx, row.ID)
	if err != nil {
		return item, err
	}
	parent, attachment, imported := importKeys(events)
	switch row.State {
	case job.StateReady:
		item.ParentKey, item.AttachmentKey = parent, attachment
		if item.ParentKey == "" && row.ZotioItemKey != "" {
			item.ParentKey = row.ZotioItemKey
		}
		switch {
		case manifestWork.Status == "existing_item_attached":
			item.Outcome = "existing_item_attached"
		case browserFetched(events):
			item.Outcome = "browser_fetched_then_imported"
		case imported || row.State == job.StateReady:
			item.Outcome = "imported"
		}
	case job.StateAwaitingHuman:
		item.Outcome, item.Reason = "awaiting_human", awaitingReason(actions)
	case job.StateNeedsReview:
		item.Outcome = "needs_review"
	case job.StateFailed, job.StateUnavailable, job.StateCancelled:
		item.Outcome = "failed"
		item.FailureClass = row.TerminalReason
		if item.FailureClass == "" {
			item.FailureClass = row.State
		}
	default:
		item.Outcome, item.Reason = "in_progress", row.State
	}
	return item, nil
}

func importKeys(events []map[string]any) (parent, attachment string, imported bool) {
	for _, event := range events {
		if stringField(event, "kind") != "zotio.auto_import" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		status, _ := detail["status"].(string)
		if status != "applied" && status != "no_op" {
			continue
		}
		imported = true
		if value, _ := detail["parent_key"].(string); value != "" {
			parent = value
		}
		if value, _ := detail["attachment_key"].(string); value != "" {
			attachment = value
		}
	}
	return parent, attachment, imported
}

func browserFetched(events []map[string]any) bool {
	for _, event := range events {
		switch stringField(event, "kind") {
		case "browser.download_complete":
			return true
		case "state.transition":
			detail, _ := event["detail"].(map[string]any)
			if source, _ := detail["source"].(string); source == "browser" {
				return true
			}
		}
	}
	return false
}

func awaitingReason(actions []job.HumanAction) string {
	for _, action := range actions {
		if action.Status != "open" {
			continue
		}
		switch action.Kind {
		case "terms_acceptance_required":
			return "terms"
		case "openurl_handoff":
			if strings.HasPrefix(action.Detail, "open-access fetch via browser\n") {
				return "oa_browser"
			}
			return "institutional"
		}
	}
	return "institutional"
}

func stringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

// Markdown renders a stable, compact agent-facing digest.
func Markdown(report *Report) string {
	if report == nil {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# Papio batch `%s`\n\n", report.BatchID)
	if report.Label != "" {
		fmt.Fprintf(&out, "Label: %s\n\n", report.Label)
	}
	fmt.Fprintf(&out, "%d works: %s.\n", report.Summary.Total, summaryLine(report.Summary.Outcomes))
	groups := make(map[string][]ReportWork)
	for _, item := range report.Works {
		groups[item.Outcome] = append(groups[item.Outcome], item)
	}
	for _, outcome := range []string{"imported", "browser_fetched_then_imported", "existing_item_attached", "awaiting_human", "needs_review", "failed", "skipped_owned", "in_progress"} {
		items := groups[outcome]
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(&out, "\n## %s (%d)\n", markdownHeading(outcome), len(items))
		for _, item := range items {
			fmt.Fprintf(&out, "- %s", describe(item.Work))
			if item.JobID != "" {
				fmt.Fprintf(&out, " (`%s`)", item.JobID)
			}
			detail := markdownDetail(item)
			if detail != "" {
				fmt.Fprintf(&out, ": %s", detail)
			}
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func summaryLine(outcomes map[string]int) string {
	parts := make([]string, 0, len(outcomes))
	for _, outcome := range []string{"imported", "browser_fetched_then_imported", "existing_item_attached", "awaiting_human", "needs_review", "failed", "skipped_owned", "in_progress"} {
		if count := outcomes[outcome]; count != 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, outcome))
		}
	}
	return strings.Join(parts, ", ")
}

func markdownHeading(outcome string) string {
	return strings.ReplaceAll(strings.Title(strings.ReplaceAll(outcome, "_", " ")), " ", " ")
}

func markdownDetail(item ReportWork) string {
	parts := make([]string, 0, 4)
	if item.Reason != "" {
		parts = append(parts, item.Reason)
	}
	if item.FailureClass != "" {
		parts = append(parts, item.FailureClass)
	}
	if item.ParentKey != "" {
		parts = append(parts, "parent `"+item.ParentKey+"`")
	}
	if item.AttachmentKey != "" {
		parts = append(parts, "attachment `"+item.AttachmentKey+"`")
	}
	if item.Collection != "" {
		parts = append(parts, "collection `"+item.Collection+"`")
	}
	return strings.Join(parts, "; ")
}

func describe(request protocol.WorkRequest) string {
	if request.Title != "" {
		return request.Title
	}
	if request.Identifiers != nil {
		for _, value := range []string{request.Identifiers.DOI, request.Identifiers.ArXiv, request.Identifiers.PMID, request.Identifiers.ISBN, request.Identifiers.OpenAlex} {
			if value != "" {
				return value
			}
		}
	}
	return request.RequestID
}
