// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"strings"
	"testing"

	"papio/internal/store"
	"papio/internal/work"
)

func TestIdentifiersProvenanceSubmittedAndAdopted(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	w := work.Work{DOI: "10.1002/prov-submitted", Title: "Provenance"}
	id, err := js.CreateRequest(ctx, "wr_prov_a", w, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Submitted DOI should be provenance 'submitted'.
	var prov string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT provenance FROM identifiers WHERE work_request_id = ? AND kind = 'doi'`, "wr_prov_a").Scan(&prov); err != nil {
		t.Fatalf("query submitted provenance: %v", err)
	}
	if prov != string(ProvenanceSubmitted) {
		t.Fatalf("submitted provenance = %q, want %q", prov, ProvenanceSubmitted)
	}
	// Adopt via FillWorkMetadata.
	if _, err := js.FillWorkMetadata(ctx, id, work.Work{PMID: "99999999"}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT provenance FROM identifiers WHERE work_request_id = ? AND kind = 'pmid'`, "wr_prov_a").Scan(&prov); err != nil {
		t.Fatalf("query adopted provenance: %v", err)
	}
	if prov != string(ProvenanceAdopted) {
		t.Fatalf("adopted provenance = %q, want %q", prov, ProvenanceAdopted)
	}
	// SubmittedIdentity should anchor only submitted/verified.
	si, err := js.SubmittedIdentity(ctx, id)
	if err != nil {
		t.Fatalf("SubmittedIdentity: %v", err)
	}
	if !si.Attested {
		t.Fatalf("Attested false, want true")
	}
	foundDOI, foundPMID := false, false
	for _, id := range si.Identifiers {
		if id.Kind == "doi" && id.Value == "10.1002/prov-submitted" {
			foundDOI = true
		}
		if id.Kind == "pmid" {
			foundPMID = true
		}
	}
	if !foundDOI {
		t.Fatalf("anchor missing submitted DOI: %+v", si.Identifiers)
	}
	if foundPMID {
		t.Fatalf("anchor should not include adopted PMID: %+v", si.Identifiers)
	}
}

func TestLegacyRowAttestedFalseAndNoAnchorIdentifiers(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	// Insert legacy-shaped row: identifiers rely on DEFAULT provenance, work_requests.submitted_fields left NULL.
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO work_requests (id, created_at, title, authors_json, year, desired_version) VALUES (?, ?, ?, ?, ?, ?)`, "wr_legacy", store.Now(), "Legacy", `[]`, 2020, "any"); err != nil {
		t.Fatalf("insert legacy work_request: %v", err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO identifiers (work_request_id, kind, value, raw) VALUES (?, ?, ?, ?)`, "wr_legacy", "doi", "10.1000/legacy", "10.1000/legacy"); err != nil {
		t.Fatalf("insert legacy identifier: %v", err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at) VALUES (?, ?, 'queued', ?, ?, ?)`, "job_legacy", "wr_legacy", `{}`, store.Now(), store.Now()); err != nil {
		t.Fatalf("insert legacy job: %v", err)
	}
	si, err := js.SubmittedIdentity(ctx, "job_legacy")
	if err != nil {
		t.Fatalf("SubmittedIdentity: %v", err)
	}
	if si.Attested {
		t.Fatalf("Attested true for legacy row, want false")
	}
	if len(si.Identifiers) != 0 {
		t.Fatalf("legacy anchor identifiers = %+v, want 0 (only submitted/verified anchor)", si.Identifiers)
	}
	if len(si.Fields) != 0 {
		t.Fatalf("legacy Fields = %+v, want empty", si.Fields)
	}
	// Direct provenance should be the default 'unattested'.
	var prov string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT provenance FROM identifiers WHERE work_request_id = ?`, "wr_legacy").Scan(&prov); err != nil {
		t.Fatalf("query legacy provenance: %v", err)
	}
	if prov != string(ProvenanceUnattested) {
		t.Fatalf("legacy provenance = %q, want unattested", prov)
	}
}

func TestSubmittedFieldsTitleOnlyAndStableAfterFill(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	w := work.Work{Title: "Only Title"}
	id, err := js.CreateRequest(ctx, "wr_title_only", w, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var sf string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COALESCE(submitted_fields,'<null>') FROM work_requests WHERE id = ?`, "wr_title_only").Scan(&sf); err != nil {
		t.Fatalf("query submitted_fields: %v", err)
	}
	if sf != "title" {
		t.Fatalf("submitted_fields = %q, want title", sf)
	}
	// Enrich with year/authors — submitted_fields must not change.
	if _, err := js.FillWorkMetadata(ctx, id, work.Work{Title: "Only Title", Year: 2024, Authors: []string{"Ada Lovelace"}}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if err := js.S.DB().QueryRowContext(ctx, `SELECT submitted_fields FROM work_requests WHERE id = ?`, "wr_title_only").Scan(&sf); err != nil {
		t.Fatalf("query submitted_fields after fill: %v", err)
	}
	if sf != "title" {
		t.Fatalf("submitted_fields after fill = %q, want still title", sf)
	}
	si, err := js.SubmittedIdentity(ctx, id)
	if err != nil {
		t.Fatalf("SubmittedIdentity: %v", err)
	}
	if !si.Fields["title"] || si.Fields["year"] || si.Fields["authors"] {
		t.Fatalf("Fields = %+v, want only title", si.Fields)
	}
	// Title supplied, year/authors from fill should be in Work (via mutable row) but anchor's Fields distinguishes.
	if !si.Attested {
		t.Fatalf("Attested false, want true")
	}
	// Empty submission case: empty string distinct from NULL.
	id2, err := js.CreateRequest(ctx, "wr_empty", work.Work{}, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create empty: %v", err)
	}
	var sfNull *string
	var rawSF string
	// Check that row exists and submitted_fields is not NULL (empty string).
	if err := js.S.DB().QueryRowContext(ctx, `SELECT submitted_fields FROM work_requests WHERE id = ?`, "wr_empty").Scan(&rawSF); err != nil {
		t.Fatalf("query empty submitted_fields: %v", err)
	}
	_ = sfNull
	if rawSF != "" {
		t.Fatalf("empty submitted_fields = %q, want empty string", rawSF)
	}
	si2, err := js.SubmittedIdentity(ctx, id2)
	if err != nil {
		t.Fatalf("SubmittedIdentity empty: %v", err)
	}
	if !si2.Attested {
		t.Fatalf("empty Attested false, want true (post-cutover empty distinct from legacy NULL)")
	}
	if len(si2.Fields) != 0 {
		t.Fatalf("empty Fields = %+v, want empty map", si2.Fields)
	}
}

func TestIdentifiersProvenanceCheckRejectsBadValue(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	if _, err := js.S.DB().ExecContext(ctx, `INSERT INTO work_requests (id, created_at, desired_version, submitted_fields) VALUES (?, ?, ?, ?)`, "wr_check", store.Now(), "any", ""); err != nil {
		t.Fatalf("insert work_request: %v", err)
	}
	_, err := js.S.DB().ExecContext(ctx, `INSERT INTO identifiers (work_request_id, kind, value, raw, provenance) VALUES (?, ?, ?, ?, ?)`, "wr_check", "doi", "10.1000/bad", "10.1000/bad", "bogus")
	if err == nil {
		t.Fatalf("expected CHECK violation for provenance='bogus', got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "check") {
		t.Fatalf("error %q does not look like CHECK violation", err.Error())
	}
}
