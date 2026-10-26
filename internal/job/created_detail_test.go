// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"papio/internal/work"
)

// TestTitleOnlyMultibyteCreatedDetailIsValidJSON pins papio-77df7de7: a
// title-only work with a multi-byte rune straddling the Describe truncation
// point must still store a valid JSON job.created detail, otherwise
// recordStandaloneOutcome drops the terminal outcome notification.
func TestTitleOnlyMultibyteCreatedDetailIsValidJSON(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	title := strings.Repeat("a", 59) + "é" + strings.Repeat("b", 10)
	id, err := js.CreateRequest(ctx, "wr_multibyte_01", work.Work{Title: title}, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	var detail string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT detail_json FROM events WHERE job_id = ? AND kind = 'job.created'`, id).Scan(&detail); err != nil {
		t.Fatalf("query job.created detail: %v", err)
	}
	if !json.Valid([]byte(detail)) {
		t.Fatalf("job.created detail is not valid JSON: %q", detail)
	}
	var decoded struct {
		RequestID string `json:"request_id"`
		Work      string `json:"work"`
	}
	if err := json.Unmarshal([]byte(detail), &decoded); err != nil {
		t.Fatalf("decode job.created detail: %v", err)
	}
	if decoded.RequestID != "wr_multibyte_01" {
		t.Fatalf("request_id = %q, want %q", decoded.RequestID, "wr_multibyte_01")
	}
	if !strings.HasPrefix(decoded.Work, "title:") {
		t.Fatalf("work = %q, want title: prefix", decoded.Work)
	}
	if _, err := js.RecheckOrigin(ctx, id); err != nil {
		t.Fatalf("RecheckOrigin: %v", err)
	}
}

// TestRecheckOriginToleratesLegacyCorruptDetail pins the tolerant half of
// papio-77df7de7: a job.created detail written before details were real JSON
// (Go %q quoting, e.g. a \x escape) must not fail the lookup, because that
// failure drops the terminal outcome notification. It reports no origin.
func TestRecheckOriginToleratesLegacyCorruptDetail(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, "wr_legacy_01", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if _, err := js.S.DB().ExecContext(ctx, `UPDATE events SET detail_json = ? WHERE job_id = ? AND kind = 'job.created'`,
		`{"request_id":"wr_legacy_01","work":"title:caf\xe9"}`, id); err != nil {
		t.Fatalf("corrupt detail: %v", err)
	}
	origin, err := js.RecheckOrigin(ctx, id)
	if err != nil {
		t.Fatalf("RecheckOrigin: %v", err)
	}
	if origin != "" {
		t.Fatalf("RecheckOrigin = %q, want empty", origin)
	}
}

// TestSupersededDetailIsValidJSONForMultibyteTitle covers the last %q-built
// detail: a recheck of an unavailable multibyte-title job must store a valid
// JSON job.superseded event on the old job.
func TestSupersededDetailIsValidJSONForMultibyteTitle(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	title := strings.Repeat("a", 59) + "é" + strings.Repeat("b", 10)
	w := work.Work{Title: title}
	oldID, err := js.CreateRequest(ctx, "wr_super_old", w, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if err := js.Transition(ctx, oldID, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("to resolving: %v", err)
	}
	if err := js.Transition(ctx, oldID, StateResolving, StateUnavailable, nil, WithTerminalReason(TerminalReasonCandidatesExhausted)); err != nil {
		t.Fatalf("to unavailable: %v", err)
	}
	res, err := js.CreateRecheckRequestForWork(ctx, "wr_super_new", w, "", "", testPolicy(), nil,
		Attribution{Principal: PrincipalUnknown}, oldID, 30)
	if err != nil || res.JobID == "" {
		t.Fatalf("CreateRecheckRequestForWork = %+v, %v", res, err)
	}
	var detail string
	if err := js.S.DB().QueryRowContext(ctx, `SELECT detail_json FROM events WHERE job_id = ? AND kind = 'job.superseded'`, oldID).Scan(&detail); err != nil {
		t.Fatalf("query job.superseded detail: %v", err)
	}
	if !json.Valid([]byte(detail)) {
		t.Fatalf("job.superseded detail is not valid JSON: %q", detail)
	}
	var decoded struct {
		SupersededBy string `json:"superseded_by"`
		Work         string `json:"work"`
	}
	if err := json.Unmarshal([]byte(detail), &decoded); err != nil {
		t.Fatalf("decode job.superseded detail: %v", err)
	}
	if decoded.SupersededBy != res.JobID {
		t.Fatalf("superseded_by = %q, want %q", decoded.SupersededBy, res.JobID)
	}
	if !strings.HasPrefix(decoded.Work, "title:") {
		t.Fatalf("work = %q, want title: prefix", decoded.Work)
	}
}
