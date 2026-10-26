// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"papio/internal/work"
)

// TagRetracted marks a personal-library item whose DOI exactly matches a
// current verified retraction notice. It is an automatic tag, so a manual tag
// of the same name is never retyped or removed. Corrections and expressions
// of concern never produce it.
const TagRetracted = "papio:retracted"

const (
	// retractionLedgerPrefix namespaces retraction rows in zotio_tag_state,
	// whose primary key is one row per item: an item can carry an exception
	// tag and the retraction tag at once, and the exception reconciler must
	// not read or remove the retraction rows (tagLedger filters them out).
	retractionLedgerPrefix = "retraction:"

	// retractionRescanEvery bounds how stale the item lookup may get while the
	// notice set stays unchanged: a paper added to Zotero after the last pass
	// still gets tagged within a day.
	retractionRescanEvery = 24 * time.Hour
)

// RetractedDOISource lists the DOIs with a current verified retraction
// notice. Implementations return only retractions: corrections and
// expressions of concern are excluded at the source. An error means the notice
// set is unknown; callers must not treat it as "no retractions".
type RetractedDOISource interface {
	RetractedDOIs(context.Context) ([]string, error)
}

// RetractionTagChange is one planned or applied tag change on a Zotero item.
// DOI is empty on removals, which the ledger records by item only.
type RetractionTagChange struct {
	ItemKey string `json:"item_key"`
	DOI     string `json:"doi,omitempty"`
}

// RetractionTagResult is stable machine output for
// `papio zotio tags retractions`. Add and Remove are the planned changes; the
// counts report what an apply pass did.
type RetractionTagResult struct {
	Applied   bool                  `json:"applied"`
	Enabled   bool                  `json:"enabled"`
	Tag       string                `json:"tag"`
	Retracted int                   `json:"retracted_dois"`
	Add       []RetractionTagChange `json:"add"`
	Remove    []RetractionTagChange `json:"remove"`
	Added     int                   `json:"added"`
	Removed   int                   `json:"removed"`
	// Preserved counts adds papio skipped because the item already carried a
	// same-name manual tag; papio records no ownership and never removes it.
	Preserved int `json:"preserved"`
}

type retractionFound struct {
	Key  string `json:"key"`
	Data struct {
		ParentItem string `json:"parentItem"`
		DOI        string `json:"DOI"`
	} `json:"data"`
}

// ReconcileRetractionTags converges the papio-owned retraction tag with the
// current retraction notices. Without apply it only plans. Preview and apply
// share one planner, so the preview names exactly the changes apply makes.
//
// Disabled (zotio.retraction_tags off) the desired set is empty: apply removes
// only papio-owned tags recorded in the ledger. A preview while disabled still
// lists the adds enabling would make, so the option can be reviewed before it
// is turned on. A manual tag is never overwritten or removed: adds use the
// automatic tag type and an already-present manual tag yields no_op, which the
// ledger records as foreign, and removals use --automatic-only.
func (s *Service) ReconcileRetractionTags(ctx context.Context, apply bool) (*RetractionTagResult, error) {
	if s == nil || s.CLI == nil || s.Store == nil {
		return nil, fmt.Errorf("Zotio integration is not configured")
	}
	s.tagMu.Lock()
	defer s.tagMu.Unlock()

	result := &RetractionTagResult{Applied: apply, Enabled: s.RetractionTags, Tag: TagRetracted,
		Add: []RetractionTagChange{}, Remove: []RetractionTagChange{}}

	var desired map[string]string // item key -> DOI
	if s.RetractionTags || !apply {
		if s.Retractions == nil {
			return nil, fmt.Errorf("retraction notices are not available: enable the retraction_watch source")
		}
		dois, err := s.Retractions.RetractedDOIs(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading retraction notices: %w", err)
		}
		result.Retracted = len(dois)
		if desired, err = s.retractedItemKeys(ctx, dois); err != nil {
			return nil, err
		}
	}
	ledger, err := s.retractionLedger(ctx)
	if err != nil {
		return nil, err
	}

	effective := desired
	if !s.RetractionTags {
		effective = map[string]string{}
	}
	keys := make([]string, 0, len(desired)+len(ledger))
	seen := map[string]struct{}{}
	for key := range desired {
		seen[key] = struct{}{}
	}
	for key := range ledger {
		seen[key] = struct{}{}
	}
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	type step struct {
		key    string
		doi    string
		remove bool
		drop   bool // forget a ledger row that owns no remote tag
	}
	var steps []step
	for _, key := range keys {
		have, tracked := ledger[key]
		_, wantEffective := effective[key]
		if tracked && !wantEffective {
			switch have.Status {
			case tagStatusOwned, tagStatusPending:
				result.Remove = append(result.Remove, RetractionTagChange{ItemKey: key})
				steps = append(steps, step{key: key, remove: true})
			default:
				steps = append(steps, step{key: key, drop: true})
			}
		}
		if _, want := desired[key]; want && (!tracked || have.Status == tagStatusPending || have.Status == tagStatusMissing) {
			result.Add = append(result.Add, RetractionTagChange{ItemKey: key, DOI: desired[key]})
			if wantEffective {
				steps = append(steps, step{key: key, doi: desired[key]})
			}
		}
	}
	if !apply {
		return result, nil
	}

	if len(steps) > 0 {
		preflight, err := s.CLI.Preflight(ctx)
		if err != nil {
			return nil, err
		}
		if compareVersion(preflight.Version, tagsMinimumZotioVersion) < 0 {
			return nil, fmt.Errorf("zotio %s does not support automatic tags (requires >= %s) — update zotio, or disable zotio.retraction_tags", preflight.Version, tagsMinimumZotioVersion)
		}
	}

	var errs []error
	for _, st := range steps {
		ledgerKey := retractionLedgerPrefix + st.key
		switch {
		case st.drop:
			if err := s.deleteTagState(ctx, ledgerKey); err != nil {
				errs = append(errs, err)
			}
		case st.remove:
			outcome, runErr := s.mutateTag(ctx, false, st.key, TagRetracted)
			if outcome == "applied" {
				result.Removed++
			}
			if outcome == "applied" || outcome == "no_op" || isZotioNotFound(runErr) {
				if err := s.deleteTagState(ctx, ledgerKey); err != nil {
					errs = append(errs, err)
					continue
				}
			} else {
				errs = append(errs, fmt.Errorf("removing %s from %s: %w", TagRetracted, st.key, runErr))
				continue
			}
			if runErr != nil {
				errs = append(errs, runErr)
			}
		default:
			have, tracked := ledger[st.key]
			wasPending := tracked && have.Status == tagStatusPending
			if err := s.upsertTagState(ctx, ledgerKey, TagRetracted, tagStatusPending); err != nil {
				errs = append(errs, err)
				continue
			}
			outcome, reason, runErr := s.mutateTagWithReason(ctx, true, st.key, TagRetracted)
			switch outcome {
			case "applied":
				result.Added++
				if err := s.upsertTagState(ctx, ledgerKey, TagRetracted, tagStatusOwned); err != nil {
					errs = append(errs, err)
					continue
				}
			case "no_op":
				status := tagStatusForeign
				if wasPending && reason.TagTypes[TagRetracted] == 1 {
					status = tagStatusOwned
				} else {
					result.Preserved++
				}
				if err := s.upsertTagState(ctx, ledgerKey, TagRetracted, status); err != nil {
					errs = append(errs, err)
					continue
				}
			default:
				if isZotioNotFound(runErr) {
					if err := s.upsertTagState(ctx, ledgerKey, TagRetracted, tagStatusMissing); err != nil {
						errs = append(errs, err)
					}
				}
			}
			if runErr != nil {
				errs = append(errs, fmt.Errorf("adding %s to %s: %w", TagRetracted, st.key, runErr))
			}
		}
	}
	return result, partialReconcileError(&TagReconcileResult{Added: result.Added, Removed: result.Removed}, errs)
}

// retractedItemKeys resolves each retracted DOI to the personal-library parent
// items that hold exactly that DOI. Any lookup failure aborts the pass: a
// partial map would read as "retraction withdrawn" and strip owned tags.
func (s *Service) retractedItemKeys(ctx context.Context, dois []string) (map[string]string, error) {
	out := map[string]string{}
	for _, raw := range dois {
		doi, err := work.NormalizeDOI(raw)
		if err != nil {
			continue
		}
		rows, err := s.CLI.RunJSON(ctx, "--agent", "--group=", "items", "find", "--doi", doi)
		if err != nil {
			return nil, fmt.Errorf("looking up Zotio doi %q: %w", doi, err)
		}
		found, err := decodeRows[retractionFound](rows)
		if err != nil {
			return nil, fmt.Errorf("decoding Zotio doi lookup: %w", err)
		}
		for _, item := range found {
			key := strings.TrimSpace(item.Key)
			if item.Data.ParentItem != "" || !keyRE.MatchString(key) {
				continue
			}
			// Exact match only: a lookup that returns near matches must not
			// tag an item with a different DOI.
			if got, err := work.NormalizeDOI(item.Data.DOI); err != nil || got != doi {
				continue
			}
			out[key] = doi
		}
	}
	return out, nil
}

func (s *Service) retractionLedger(ctx context.Context) (map[string]tagLedgerState, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT item_key, tag, status FROM zotio_tag_state WHERE item_key LIKE ?`, retractionLedgerPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("reading retraction tag ledger: %w", err)
	}
	defer rows.Close()
	ledger := map[string]tagLedgerState{}
	for rows.Next() {
		var key string
		var state tagLedgerState
		if err := rows.Scan(&key, &state.Tag, &state.Status); err != nil {
			return nil, err
		}
		ledger[strings.TrimPrefix(key, retractionLedgerPrefix)] = state
	}
	return ledger, rows.Err()
}

func (s *Service) hasRetractionLedger(ctx context.Context) bool {
	var exists int
	err := s.Store.DB().QueryRowContext(ctx, `SELECT 1 FROM zotio_tag_state WHERE item_key LIKE ? LIMIT 1`, retractionLedgerPrefix+"%").Scan(&exists)
	return err == nil
}

// RetractionTagReconciler adapts ReconcileRetractionTags to the daemon's
// maintenance seam. It is the optional automatic run: it exists only when the
// option is on or papio-owned retraction tags remain to clean, and it does the
// per-DOI item lookups only when the notice set changed or a day has passed.
type RetractionTagReconciler struct {
	service    *Service
	signature  string
	lastPass   time.Time
	lastErr    string
	lastErrLog time.Time
	retry      tagReconcileRetry
}

// RetractionTagReconciler returns nil when the option is off and no ledger
// state remains, so the default-off path adds no maintenance work.
func (s *Service) RetractionTagReconciler() *RetractionTagReconciler {
	if s == nil || s.CLI == nil || s.Store == nil {
		return nil
	}
	if !s.RetractionTags && !s.hasRetractionLedger(context.Background()) {
		return nil
	}
	return &RetractionTagReconciler{service: s}
}

// MaintenanceFailure reports the last failed pass until reconciliation
// succeeds. The scheduler reads it after RunDue on the maintenance goroutine.
func (r *RetractionTagReconciler) MaintenanceFailure() error {
	if r == nil {
		return nil
	}
	return r.retry.err
}

func (r *RetractionTagReconciler) RunDue(ctx context.Context) error {
	if r == nil {
		return nil
	}
	s := r.service
	if !r.retry.due(s.now()) {
		return nil
	}
	signature := "off"
	if s.RetractionTags {
		if s.Retractions == nil {
			return r.fail(fmt.Errorf("retraction notices are not available: enable the retraction_watch source"))
		}
		dois, err := s.Retractions.RetractedDOIs(ctx)
		if err != nil {
			return r.fail(err)
		}
		sum := sha256.Sum256([]byte(strings.Join(dois, "\n")))
		signature = "on:" + hex.EncodeToString(sum[:])
	} else if !s.hasRetractionLedger(ctx) {
		r.retry, r.lastErr = tagReconcileRetry{}, ""
		return nil
	}
	if signature == r.signature && s.now().Sub(r.lastPass) < retractionRescanEvery {
		r.retry, r.lastErr = tagReconcileRetry{}, ""
		return nil
	}
	if _, err := s.ReconcileRetractionTags(ctx, true); err != nil {
		return r.fail(err)
	}
	r.signature, r.lastPass, r.lastErr = signature, s.now(), ""
	r.retry = tagReconcileRetry{}
	return nil
}

func (r *RetractionTagReconciler) fail(err error) error {
	r.retry.fail(r.service.now(), err)
	if msg := err.Error(); msg != r.lastErr || time.Since(r.lastErrLog) >= tagErrorLogInterval {
		log.Printf("zotio retraction tag reconcile: %v", err)
		r.lastErr = msg
		r.lastErrLog = time.Now()
	}
	return err
}
