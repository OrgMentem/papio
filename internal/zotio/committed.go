// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"papio/internal/work"
)

// Committed creates.
//
// A connector create is several desktop requests with no transaction across
// them: saveItems, then updateSession to file the item, then saveAttachment.
// When a later step fails, zotio reports the create as a "conflict" whose
// reason says committed: the item is in Zotero, usually without its PDF, and
// often under a key zotio could not read back. papio used to record that as
// an ordinary failed apply and drop the plan. The next pass resolved the PDF
// again, the mirror did not show the item yet, the resolver called the paper
// new, and a second create put a duplicate paper in the library.
//
// So a committed create is recorded on the job as zotio.import_committed, and
// from then on the job never creates an item again. Each later plan first
// names the committed item: from zotio's evidence when it carried a key, or
// else from the job's identifiers in a freshly synced mirror. With exactly one
// item it plans the existing-item route, which attaches the PDF to that item
// and runs the follow-ups. With none it waits, and with several it stops for
// the operator.

const (
	importCommittedEvent   = "zotio.import_committed"
	importCommittedPending = "unreconciled"
	importCommittedFound   = "reconciled"
)

// errCommittedCreatePending is the refusal while the committed item is not
// visible in the mirror yet.
var errCommittedCreatePending = errors.New("an earlier Zotero import saved this paper's item but did not finish; papio waits until Zotio shows that item and then attaches the PDF to it, instead of creating it again")

// committedCreateEvidence returns the reason zotio gave for a create that it
// committed but did not finish, or nil when the envelope reports none.
func committedCreateEvidence(raw json.RawMessage) map[string]any {
	var envelope mutationEnvelope
	if len(raw) == 0 || json.Unmarshal(raw, &envelope) != nil || envelope.Result == nil {
		return nil
	}
	for _, item := range envelope.Result.Items {
		if item.Status != "conflict" && item.Status != "failed" {
			continue
		}
		if reason, ok := item.Reason.(map[string]any); ok && reason["committed"] == true {
			return reason
		}
	}
	return nil
}

// recordCommittedCreate records a committed but unfinished create before the
// failed apply drops its plan. If the record cannot be written, the apply is
// recorded as ambiguous instead, which keeps the plan and blocks every retry:
// dropping the plan without the record would allow the duplicate create.
func (s *Service) recordCommittedCreate(ctx context.Context, key string, plan *Plan, out json.RawMessage, evidence map[string]any, applyErr error) error {
	detail := map[string]any{"status": importCommittedPending}
	for _, field := range []string{"via", "title", "session", "connector_key", "attachment_marker"} {
		if value := strings.TrimSpace(stringField(evidence, field)); value != "" {
			detail[field] = value
		}
	}
	for _, field := range []string{"parent_key", "key"} {
		if value := strings.TrimSpace(stringField(evidence, field)); keyRE.MatchString(value) {
			detail["parent_key"] = value
			break
		}
	}
	if err := s.Bundle.Jobs.RecordEvent(context.WithoutCancel(ctx), plan.JobID, importCommittedEvent, detail); err != nil {
		return s.recordAmbiguousApply(ctx, key, plan, out, applyErr)
	}
	return s.recordFailedApplyAndInvalidatePlan(ctx, key, plan, out, applyErr)
}

// latestCommittedCreate returns the job's latest zotio.import_committed
// detail, or nil when the job has none.
func (s *Service) latestCommittedCreate(ctx context.Context, jobID string) (map[string]any, error) {
	var raw string
	err := s.Store.DB().QueryRowContext(ctx,
		`SELECT detail_json FROM events WHERE job_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		jobID, importCommittedEvent).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(raw), &detail); err != nil {
		return nil, fmt.Errorf("decoding %s event: %w", importCommittedEvent, err)
	}
	return detail, nil
}

// committedParentKey returns the key of the item an earlier create of this
// job committed, or "" when the job never committed one. While that item
// cannot be named it returns an error, so the job plans nothing at all.
func (s *Service) committedParentKey(ctx context.Context, jobID string, w work.Work) (string, error) {
	detail, err := s.latestCommittedCreate(ctx, jobID)
	if err != nil || detail == nil {
		return "", err
	}
	if key := stringField(detail, "parent_key"); keyRE.MatchString(key) {
		return key, nil
	}
	if err := s.CLI.Sync(ctx); err != nil {
		return "", fmt.Errorf("%w (refreshing Zotio library: %w)", errCommittedCreatePending, err)
	}
	identifiers, err := normalizedLookupIdentifiers(LookupWorkFrom(w))
	if err != nil {
		return "", err
	}
	seen := make(map[string]bool)
	var found []string
	for _, identifier := range identifiers {
		keys, err := s.findParentItemKeys(ctx, identifier)
		if err != nil {
			return "", fmt.Errorf("%w (%w)", errCommittedCreatePending, err)
		}
		for _, key := range keys {
			if !seen[key] {
				seen[key] = true
				found = append(found, key)
			}
		}
	}
	switch len(found) {
	case 0:
		return "", errCommittedCreatePending
	case 1:
	default:
		return "", fmt.Errorf("an earlier Zotero import saved this paper's item but did not finish, and Zotio now shows %d items for the paper; delete the extra items in Zotero so that papio can attach the PDF to the one that remains", len(found))
	}
	key := found[0]
	resolved := map[string]any{"status": importCommittedFound, "parent_key": key}
	if err := s.Bundle.Jobs.RecordEvent(context.WithoutCancel(ctx), jobID, importCommittedEvent, resolved); err != nil {
		return "", err
	}
	return key, nil
}

// refuseCreateAfterCommit is the last guard before a create reaches zotio: a
// job with a committed create on record never creates another item, whatever
// plan asks for it.
func (s *Service) refuseCreateAfterCommit(ctx context.Context, plan *Plan) error {
	if plan.Route != "manifest_create" {
		return nil
	}
	detail, err := s.latestCommittedCreate(ctx, plan.JobID)
	if err != nil {
		return err
	}
	if detail != nil {
		return fmt.Errorf("refusing plan %s: %w", plan.ID, errCommittedCreatePending)
	}
	return nil
}
