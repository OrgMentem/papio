// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// SubmittedIdentity loads the immutable submit snapshot for one job. Legacy rows
// with NULL submitted_fields return Attested false so cache reuse and validation
// cannot treat adopted metadata as if the requester supplied it.
func (js *Store) SubmittedIdentity(ctx context.Context, jobID string) (SubmittedIdentity, error) {
	db := js.S.DB()
	var requestID string
	var title, authorsJSON sql.NullString
	var year sql.NullInt64
	var submittedFields sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT j.work_request_id, w.title, w.authors_json, w.year, w.submitted_fields
		FROM jobs j
		JOIN work_requests w ON w.id = j.work_request_id
		WHERE j.id = ?`, jobID).Scan(&requestID, &title, &authorsJSON, &year, &submittedFields)
	if err != nil {
		return SubmittedIdentity{}, err
	}

	anchor := SubmittedIdentity{
		Fields: make(map[string]bool),
	}
	if !submittedFields.Valid {
		return anchor, nil
	}
	anchor.Attested = true
	for _, field := range strings.Split(submittedFields.String, ",") {
		field = strings.TrimSpace(field)
		if field != "" {
			anchor.Fields[field] = true
		}
	}
	if title.Valid {
		anchor.Work.Title = title.String
	}
	if year.Valid {
		anchor.Work.Year = int(year.Int64)
	}
	if authorsJSON.Valid && authorsJSON.String != "" {
		if err := json.Unmarshal([]byte(authorsJSON.String), &anchor.Work.Authors); err != nil {
			return SubmittedIdentity{}, fmt.Errorf("decode authors: %w", err)
		}
	}

	rows, err := db.QueryContext(ctx, `
		SELECT kind, value, COALESCE(raw, value), provenance
		FROM identifiers WHERE work_request_id = ? AND provenance IN ('submitted','verified')`, requestID)
	if err != nil {
		return SubmittedIdentity{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, value, raw, prov string
		if err := rows.Scan(&kind, &value, &raw, &prov); err != nil {
			return SubmittedIdentity{}, err
		}
		anchor.Identifiers = append(anchor.Identifiers, Identifier{
			Kind: kind, Value: value, Raw: raw, Provenance: Provenance(prov),
		})
		switch kind {
		case "doi":
			anchor.Work.DOI = value
		case "pmid":
			anchor.Work.PMID = value
		case "arxiv":
			anchor.Work.ArXiv = value
		case "isbn":
			anchor.Work.ISBN = value
		case "openalex":
			anchor.Work.OpenAlex = value
		}
	}
	if err := rows.Err(); err != nil {
		return SubmittedIdentity{}, err
	}
	return anchor, nil
}

// AnchorAllowsDOICache reports whether a DOI→SHA256 fast path may run for doi
// against this anchor: the request must be attested and the DOI must be present
// among submitted or verified identifiers.
func (a SubmittedIdentity) AnchorAllowsDOICache(doi string) bool {
	if !a.Attested || strings.TrimSpace(doi) == "" {
		return false
	}
	for _, id := range a.Identifiers {
		if id.Kind != "doi" || !strings.EqualFold(id.Value, doi) {
			continue
		}
		switch id.Provenance {
		case ProvenanceSubmitted, ProvenanceVerified:
			return true
		default:
			return false
		}
	}
	return false
}

// InsufficientIdentityAuthority reports the sparse-input case: the anchor attests
// title only (no year, authors, or strong identifier) so candidate-derived
// metadata must not verify the work or reach ready without independent authority.
func (a SubmittedIdentity) InsufficientIdentityAuthority() bool {
	if !a.Attested {
		return false
	}
	if a.Fields["year"] || a.Fields["authors"] || a.Fields["doi"] || a.Fields["pmid"] ||
		a.Fields["arxiv"] || a.Fields["openalex"] || a.Fields["isbn"] {
		return false
	}
	if !a.Fields["title"] {
		return false
	}
	for _, id := range a.Identifiers {
		if id.Provenance == ProvenanceSubmitted || id.Provenance == ProvenanceVerified {
			return false
		}
	}
	return true
}
