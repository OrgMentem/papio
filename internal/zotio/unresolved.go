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

	"papio/internal/work"
)

// Papers that zotio cannot describe.
//
// "zotio import resolve" describes a new paper from its DOI registry record,
// and it asks two registries: Crossref and DataCite. A DOI that another agency
// registered (mEDRA, JaLC, KISTI and others) is a 404 at both, so zotio leaves
// the entry "unresolved", prints the manifest, and exits non-zero. A paper that
// papio knows by PMID or ISBN only fares worse: zotio reads a DOI or an arXiv
// ID from the staged name or the PDF and nothing else, so the entry comes back
// "unidentified", for Zotero's PDF recognizer. papio refused both on every
// retry, and the validated PDF never reached the library. Measured
// 2026-09-24: a paper with an mEDRA DOI, reported as "Zotero HTTP 404", and a
// preprint that papio knew by its PMID only.
//
// papio holds a record of its own for every ready paper: the title and the
// authors that its PDF identity check passed against, and the identifiers the
// request named. zotio's manifest takes a resolved create with an item that the
// caller supplies, so papio describes the paper itself. It does so only for an
// entry that names the job's own paper, and only after it asks the library
// about the job's identifiers: zotio matched the library by DOI alone, and not
// at all for a file that it could not identify.

// unresolvedEntryReport reports whether a failed "import resolve" is zotio's
// report of one unresolved entry. zotio exits non-zero when a metadata lookup
// fails, but it still prints the manifest, and the entry's note names the
// cause. The process error adds nothing to that note. Its first line, "1
// fanout request(s) failed" and the staged path, used the whole bound of every
// recorded message, so the operator saw "Error: 1 fanout..." and no cause.
// A canceled or timed-out resolve is never such a report.
func unresolvedEntryReport(raw json.RawMessage, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var manifest importManifest
	if json.Unmarshal(raw, &manifest) != nil {
		return false
	}
	return len(manifest.Entries) == 1 && manifest.Entries[0].Status == "unresolved"
}

// describeUnresolved completes a manifest whose one entry zotio left
// unresolved, and returns the manifest that zotio will apply. A paper that the
// library already holds becomes the entry zotio writes for a DOI it matched: a
// duplicate, or an attach to the item that has no PDF. Any other paper becomes
// a create with the item papioItem describes.
func (s *Service) describeUnresolved(ctx context.Context, w work.Work, raw json.RawMessage, manifest importManifest) (json.RawMessage, importManifest, error) {
	entry := manifest.Entries[0]
	if refusal := unresolvedRefusal(entry.Action, entry.Classification, entry.IdentifierType, entry.Identifier, w); refusal != "" {
		return nil, importManifest{}, unresolvedError(refusal, entry.Note)
	}
	// The mirror is the one planJob synced just before the resolve.
	lookup, err := s.LookupWorks(ctx, LookupWorksRequest{Works: []LookupWork{LookupWorkFrom(w)}, LocalOnly: true})
	if err != nil {
		return nil, importManifest{}, fmt.Errorf("checking the library for a paper Zotio left unresolved: %w", err)
	}
	var generic map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&generic); err != nil {
		return nil, importManifest{}, fmt.Errorf("decoding Zotio import manifest: %w", err)
	}
	entries, _ := generic["entries"].([]any)
	var fields map[string]any
	if len(entries) == 1 {
		fields, _ = entries[0].(map[string]any)
	}
	if fields == nil {
		return nil, importManifest{}, errors.New("decoding Zotio import manifest: want exactly one entry object")
	}
	fields["status"] = "resolved"
	fields["note"] = "papio resolved this entry from its own record"
	if note := strings.TrimSpace(entry.Note); note != "" {
		fields["note"] = "papio resolved this entry from its own record. Zotio: " + note
	}
	if strings.TrimSpace(entry.Identifier) == "" {
		if kind, value := strongestIdentifier(w); kind != "" {
			fields["identifier_type"], fields["identifier"] = kind, value
		}
	}
	switch owned := lookup.Works[0]; owned.Status {
	case OwnershipOwnedWithPDF:
		fields["classification"], fields["action"], fields["matched_key"] = "duplicate", "skip", owned.ItemKey
	case OwnershipOwnedMissingPDF:
		fields["classification"], fields["action"], fields["matched_key"] = "attach_candidate", "attach", owned.ItemKey
	default:
		fields["classification"], fields["action"], fields["item"] = "new", "create", papioItem(w)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(generic); err != nil {
		return nil, importManifest{}, err
	}
	described := json.RawMessage(bytes.TrimRight(out.Bytes(), "\n"))
	var completed importManifest
	if err := json.Unmarshal(described, &completed); err != nil {
		return nil, importManifest{}, fmt.Errorf("decoding Zotio import manifest: %w", err)
	}
	return described, completed, nil
}

// unresolvedRefusal returns why papio cannot describe the entry's paper, or ""
// when it can. zotio leaves two kinds of entry unresolved: a file it could not
// identify, and a DOI whose registry lookup failed. That DOI can be one zotio
// read from the PDF's text, such as a cited paper's, so papio describes it
// only when it is the job's own DOI.
func unresolvedRefusal(action, classification, identifierType, identifier string, w work.Work) string {
	switch {
	case action == "recognize" && classification == "unidentified":
	case action == "create" && classification == "new":
		want, wantErr := work.NormalizeDOI(w.DOI)
		got, gotErr := work.NormalizeDOI(identifier)
		if identifierType != "doi" || wantErr != nil || gotErr != nil || got != want {
			return "Zotio left a DOI unresolved that is not the job's DOI"
		}
	default:
		return fmt.Sprintf("Zotio left an entry with action %q unresolved", action)
	}
	if strings.TrimSpace(w.Title) == "" {
		return "Zotio left the paper unresolved and papio holds no title for it"
	}
	return ""
}

// unresolvedError reports a paper that neither zotio nor papio can describe.
// The refusal leads, because a recorded message keeps only its first 120 bytes,
// and zotio's note follows for the log.
func unresolvedError(refusal, note string) error {
	message := refusal
	if note = strings.TrimSpace(note); note != "" {
		message += ": " + note
	}
	return &ClassifiedError{
		cause: errors.New(message),
		info:  safeErrorInfo(ErrorClassMetadataUnresolved, refusal, 0),
	}
}

// papioItem describes a paper from papio's own record. Each author keeps the
// one form papio holds, as a single-field name: splitting "Lo Moro G" into a
// given and a family name would be a guess. The PMID and the arXiv ID go in
// Extra, which every item type has; Zotero desktop moves a recognized Extra
// line into the item's own field when its type has one.
func papioItem(w work.Work) map[string]any {
	item := map[string]any{"itemType": "journalArticle", "title": strings.TrimSpace(w.Title)}
	doi, _ := work.NormalizeDOI(w.DOI)
	pmid, _ := work.NormalizePMID(w.PMID)
	arxiv, _ := work.NormalizeArXiv(w.ArXiv)
	if isbn := normalizedISBN(w.ISBN); isbn != "" && doi == "" && pmid == "" && arxiv == "" {
		item["itemType"] = "book"
		item["ISBN"] = isbn
	}
	creators := make([]any, 0, len(w.Authors))
	for _, name := range w.Authors {
		if name = strings.TrimSpace(name); name != "" {
			creators = append(creators, map[string]any{"creatorType": "author", "name": name})
		}
	}
	if len(creators) > 0 {
		item["creators"] = creators
	}
	if w.Year > 0 {
		item["date"] = strconv.Itoa(w.Year)
	}
	if doi != "" {
		item["DOI"] = doi
	}
	var extra []string
	if pmid != "" {
		extra = append(extra, "PMID: "+pmid)
	}
	if arxiv != "" {
		extra = append(extra, "arXiv: "+arxiv)
	}
	if len(extra) > 0 {
		item["extra"] = strings.Join(extra, "\n")
	}
	return item
}

// strongestIdentifier names the job's paper for an entry zotio found no
// identifier in, in the order the staged name prefers them.
func strongestIdentifier(w work.Work) (kind, value string) {
	if doi, err := work.NormalizeDOI(w.DOI); err == nil {
		return "doi", doi
	}
	if arxiv, err := work.NormalizeArXiv(w.ArXiv); err == nil {
		return "arxiv", arxiv
	}
	if pmid, err := work.NormalizePMID(w.PMID); err == nil {
		return "pmid", pmid
	}
	if isbn := normalizedISBN(w.ISBN); isbn != "" {
		return "isbn", isbn
	}
	return "", ""
}
