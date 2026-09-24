// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"papio/internal/job"
	"papio/internal/work"
)

// Save-time filing.
//
// An import through Zotero desktop used to leave two writes to the Zotero Web
// API afterwards: filing the new item into the policy collection, and filling
// its DOI and abstract. The Web API has no copy of the item until the desktop
// syncs it, so both answered 404 and waited on FollowUpRetrier, which could
// take minutes and, with sync paused, never finish. The desktop can do both
// while it saves the item. zotio's "import apply" maps a manifest item's
// collection key to a desktop target and files the item in the same session
// that saves it, and Zotero saves the DOI and abstract that come with the
// item. describeNewItem puts them in the manifest. savedWithImport reads
// zotio's answer to decide whether the follow-ups have anything left to do.

// AbstractLookup returns the abstract a metadata source holds for a DOI, or ""
// when it holds none.
type AbstractLookup func(ctx context.Context, doi string) (string, error)

// errFilingTargetMissing marks an apply that zotio refused before it wrote
// anything, because Zotero desktop could not map the policy collection to one
// of its own targets. The usual cause is a collection that the Web API created
// and the desktop has not synced yet. Apply has dropped the plan and recorded
// the filing as deferred, so the next plan leaves it to the follow-up.
var errFilingTargetMissing = errors.New("Zotero desktop cannot file into the policy collection yet; it is filed after the import instead")

const (
	// filingDeferred is the zotio.collection_filing status that says filing
	// moved from the desktop save to the post-import follow-up.
	filingDeferred = "deferred"
	// desktopTargetMissingReason names why filing moved.
	desktopTargetMissingReason = "desktop_target_missing"
	desktopTargetMissingHint   = "Zotero desktop does not show this collection yet; it is filed after the import instead"

	collectionPageSize = 100
	// maxCollectionPages bounds one name lookup. A library this large gets no
	// save-time filing, which only costs the follow-up it always had.
	maxCollectionPages = 50
)

// desktopTargetMissingPhrases are zotio's own words (connector_target.go) for
// a collection key that Zotero desktop cannot map to exactly one target: the
// desktop does not have the collection, its path matches no target, or its
// path matches more than one.
var desktopTargetMissingPhrases = []string{
	"was not found in the live zotero collection list",
	"no desktop connector target matched",
	"maps to ambiguous connector path",
}

// describeNewItem completes a new item's manifest entry before papio writes,
// hashes and previews it. It adds the policy collection's key, so the desktop
// files the item in the session that saves it, and it fills the DOI and the
// abstract when the registry record that zotio's resolver used left them
// empty. It never replaces a value the resolver supplied. It returns the
// manifest unchanged, byte for byte, when it has nothing to add.
func (s *Service) describeNewItem(ctx context.Context, plan *Plan, row *job.Row, raw json.RawMessage) (json.RawMessage, error) {
	var manifest map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decoding Zotio import manifest: %w", err)
	}
	entries, _ := manifest["entries"].([]any)
	if len(entries) != 1 {
		return raw, nil
	}
	entry, _ := entries[0].(map[string]any)
	item, _ := entry["item"].(map[string]any)
	if item == nil {
		return raw, nil
	}
	changed := false
	key, err := s.saveTimeCollectionKey(ctx, plan, item)
	if err != nil {
		return nil, err
	}
	if key != "" {
		item["collections"] = []any{key}
		plan.CollectionKey = key
		changed = true
	}
	// The job supplies the DOI and the abstract, so they can describe only an
	// item that zotio built from the job's own DOI.
	if doi, ok := entryDOI(entry, plan.DOI); ok {
		if blankField(item, "DOI") {
			item["DOI"] = doi
			changed = true
		}
		if blankField(item, "abstractNote") {
			if abstract := s.discoveryAbstract(ctx, row, doi); abstract != "" {
				item["abstractNote"] = abstract
				changed = true
			}
		}
	}
	plan.SavesDOIAndAbstract = !blankField(item, "DOI") && !blankField(item, "abstractNote")
	if !changed {
		return raw, nil
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(manifest); err != nil {
		return nil, err
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// saveTimeCollectionKey returns the key of the policy collection that the
// desktop save should file into, or "" to file after the import. That is the
// right answer for no policy collection, a filing already deferred for this
// job, and a name that matches no collection or more than one: the follow-up
// handles all of these, and creates a missing collection, as it always has.
func (s *Service) saveTimeCollectionKey(ctx context.Context, plan *Plan, item map[string]any) (string, error) {
	name := strings.TrimSpace(plan.Collection)
	if name == "" {
		return "", nil
	}
	if _, ok := item["collections"]; ok {
		// zotio's resolver never files an item. If a future one does, that
		// choice is zotio's to make.
		return "", nil
	}
	deferred, err := s.collectionFilingDeferred(ctx, plan.JobID, name)
	if err != nil || deferred {
		return "", err
	}
	return s.collectionKeyByName(ctx, name), nil
}

// collectionFilingDeferred reports whether an earlier apply for this job gave
// up filing into the named collection at save time.
func (s *Service) collectionFilingDeferred(ctx context.Context, jobID, collection string) (bool, error) {
	var deferred bool
	err := s.Store.DB().QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM events
			WHERE job_id = ? AND kind = ?
				AND json_extract(detail_json, '$.status') = ?
				AND json_extract(detail_json, '$.collection') = ?)`,
		jobID, followUpCollectionFiling, filingDeferred, collection).Scan(&deferred)
	return deferred, err
}

// collectionRow is the part of a "zotio collections list" row that a name
// lookup reads.
type collectionRow struct {
	Key  string `json:"key"`
	Data struct {
		Key     string `json:"key"`
		Name    string `json:"name"`
		Deleted any    `json:"deleted"`
	} `json:"data"`
}

// collectionKeyByName resolves a collection name to the key of the one
// collection with exactly that name, compared the way "zotio items
// add-to-collection --collection-name" compares it. It returns "" when no
// collection or more than one has the name, or when it cannot read the whole
// list. When zotio reads the desktop's local API, as the connector route
// needs, the list is the same one zotio later maps to a desktop target, so a
// collection that the desktop has not synced yet is normally not found here
// and stays on the follow-up. Apply catches the rest (desktopTargetMissing).
func (s *Service) collectionKeyByName(ctx context.Context, name string) string {
	seen := make(map[string]bool)
	var matches []string
	complete := false
	for page := 0; page < maxCollectionPages && !complete; page++ {
		raw, err := s.CLI.RunJSON(ctx, "--agent", "collections", "list",
			"--limit", strconv.Itoa(collectionPageSize), "--start", strconv.Itoa(page*collectionPageSize))
		if err != nil {
			return ""
		}
		rows, err := decodeRows[collectionRow](raw)
		if err != nil {
			return ""
		}
		fresh := 0
		for _, row := range rows {
			key := strings.TrimSpace(row.Key)
			if key == "" {
				key = strings.TrimSpace(row.Data.Key)
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			fresh++
			if !deletedFlag(row.Data.Deleted) && strings.TrimSpace(row.Data.Name) == name {
				matches = append(matches, key)
			}
		}
		// A short page ends the list. A page with nothing new means the source
		// ignored the offset and would repeat itself forever.
		complete = len(rows) < collectionPageSize || fresh == 0
	}
	if !complete || len(matches) != 1 || !keyRE.MatchString(matches[0]) {
		return ""
	}
	return matches[0]
}

func deletedFlag(value any) bool {
	switch deleted := value.(type) {
	case bool:
		return deleted
	case float64:
		return deleted != 0
	default:
		return false
	}
}

// discoveryAbstract asks the discovery source for the abstract of the job's
// DOI when the post-import enrichment would otherwise look for one: auto-enrich
// is on and the job asked for auto-import. A failed or refused lookup leaves
// the abstract to that enrichment.
func (s *Service) discoveryAbstract(ctx context.Context, row *job.Row, doi string) string {
	if s.Abstracts == nil || !s.AutoEnrich || row == nil || !row.Policy.AutoImport || strings.TrimSpace(doi) == "" {
		return ""
	}
	abstract, err := s.Abstracts(ctx, doi)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(abstract)
}

// entryDOI returns the job's DOI, normalized, when zotio resolved the manifest
// entry from that same DOI.
func entryDOI(entry map[string]any, jobDOI string) (string, bool) {
	if kind, _ := entry["identifier_type"].(string); kind != "doi" {
		return "", false
	}
	identifier, _ := entry["identifier"].(string)
	want, err := work.NormalizeDOI(jobDOI)
	if err != nil {
		return "", false
	}
	got, err := work.NormalizeDOI(identifier)
	return want, err == nil && got == want
}

func blankField(item map[string]any, field string) bool {
	value, _ := item[field].(string)
	return strings.TrimSpace(value) == ""
}

// savedWithImport reports whether zotio saved the new item whole through
// Zotero desktop. zotio reports a stored connector create "applied" only when
// the save, the target filing, the PDF, and the key read-back all succeeded. A
// filing or PDF failure after the save is a "conflict", which never reaches a
// recorded success. A replay that zotio answers "no_op" proves nothing about
// the first save, so it keeps the follow-ups.
func savedWithImport(plan *Plan, result *ApplyResult) bool {
	if plan == nil || result == nil || plan.Route != "manifest_create" || result.Status != "applied" {
		return false
	}
	var envelope mutationEnvelope
	if json.Unmarshal(result.Zotio, &envelope) != nil || envelope.Result == nil {
		return false
	}
	summary := envelope.Result.Summary
	if summary.Failed != 0 || summary.Conflicts != 0 {
		return false
	}
	for _, item := range envelope.Result.Items {
		if item.Status == "applied" && stringField(item.Reason, "via") == "connector" {
			return true
		}
	}
	return false
}

// filedWithImport reports whether the desktop save already filed the item.
func filedWithImport(plan *Plan, result *ApplyResult) bool {
	return plan != nil && plan.CollectionKey != "" && savedWithImport(plan, result)
}

// followUp finishes a first successful apply. The Web API follow-ups run only
// for what the desktop save did not already do. A save that did it records the
// same event kinds with "with_import", so the job's activity, the batch report
// and FollowUpRetrier all read one history whichever way the item was filed.
func (s *Service) followUp(ctx context.Context, plan *Plan, result *ApplyResult) {
	durable := context.WithoutCancel(ctx)
	if filedWithImport(plan, result) {
		_ = s.Bundle.Jobs.RecordEvent(durable, plan.JobID, followUpCollectionFiling, map[string]any{
			"collection":     strings.TrimSpace(plan.Collection),
			"collection_key": plan.CollectionKey,
			"status":         "applied",
			"with_import":    true,
		})
	} else {
		s.fileCollection(ctx, plan, result)
	}
	if !plan.SavesDOIAndAbstract || !savedWithImport(plan, result) {
		s.enrichAutoImportedParent(ctx, plan, result)
		return
	}
	if s.autoEnrichApplies(ctx, plan, result) {
		_ = s.Bundle.Jobs.RecordEvent(durable, plan.JobID, followUpEnrich, map[string]any{
			"parent_key":  result.ParentKey,
			"summary":     "saved DOI and abstract with the new item",
			"status":      "applied",
			"with_import": true,
		})
	}
}

// desktopTargetMissing reports whether zotio refused a save-time filing
// because Zotero desktop could not map the collection key to exactly one of
// its targets. zotio resolves the target before saveItems. This refusal
// therefore comes back as a failed create with a plain-text reason, and with
// nothing applied or committed. A conflict after a committed save carries a
// detail object instead, and never matches.
func desktopTargetMissing(raw json.RawMessage) bool {
	var envelope mutationEnvelope
	if len(raw) == 0 || json.Unmarshal(raw, &envelope) != nil || envelope.Mode != "apply" || envelope.Result == nil {
		return false
	}
	if summary := envelope.Result.Summary; summary.Applied != 0 || summary.Conflicts != 0 || summary.Failed == 0 {
		return false
	}
	for _, item := range envelope.Result.Items {
		if item.Status != "failed" {
			continue
		}
		reason, ok := item.Reason.(string)
		if !ok {
			return false
		}
		lower := strings.ToLower(reason)
		for _, phrase := range desktopTargetMissingPhrases {
			if strings.Contains(lower, phrase) {
				return true
			}
		}
	}
	return false
}

// deferCollectionFiling records that this job's item is filed after the
// import instead of at save time. The record is what the next plan reads.
func (s *Service) deferCollectionFiling(ctx context.Context, plan *Plan) {
	_ = s.Bundle.Jobs.RecordEvent(context.WithoutCancel(ctx), plan.JobID, followUpCollectionFiling, map[string]any{
		"collection":     strings.TrimSpace(plan.Collection),
		"collection_key": plan.CollectionKey,
		"status":         filingDeferred,
		"reason":         desktopTargetMissingReason,
		"error_hint":     desktopTargetMissingHint,
	})
}
