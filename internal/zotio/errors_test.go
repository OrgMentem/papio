// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestClassifyErrorTable(t *testing.T) {
	pathLaden := "request https://zotero.example.test/items/AB12CD34 failed at /Users/reader/papio/private.db"
	cases := []struct {
		name       string
		err        error
		envelope   json.RawMessage
		wantClass  string
		wantHint   string
		wantStatus int
	}{
		{
			name:       "zotero http 4xx envelope",
			err:        errors.New("zotio import apply failed"),
			envelope:   json.RawMessage(`{"ok":false,"error":{"http_status":429,"message":"https://zotero.example.test/users/123"}}`),
			wantClass:  ErrorClassZoteroHTTP4xx,
			wantHint:   "Zotero HTTP 429",
			wantStatus: 429,
		},
		{name: "field validation", err: errors.New("zotio stderr: Unknown item field 'abstractNote'"), wantClass: ErrorClassZoteroFieldValidation, wantHint: "unknown item field"},
		{name: "mirror sync", err: errors.New("syncing Zotio library: upstream rejected request"), wantClass: ErrorClassMirrorSyncFailed, wantHint: "Zotio mirror sync failed"},
		{name: "exec timeout", err: errors.New("zotio command timed out after 30s"), wantClass: ErrorClassZotioExecTimeout, wantHint: "Zotio command timed out"},
		{name: "context timeout", err: context.DeadlineExceeded, wantClass: ErrorClassZotioExecTimeout, wantHint: "Zotio command timed out"},
		{name: "not configured", err: errors.New("zotio executable is not configured"), wantClass: ErrorClassZotioNotConfigured, wantHint: "Zotio is not configured"},
		{name: "confirmation mismatch", err: errors.New("confirmation SHA-256 does not match plan zplan_deadbeef"), wantClass: ErrorClassPlanConfirmationMismatch, wantHint: "plan confirmation does not match"},
		{name: "reservation conflict", err: errors.New("Zotio apply reservation was not finalized"), wantClass: ErrorClassReservationConflict, wantHint: "Zotio apply reservation conflict"},
		{name: "local db locked", err: errors.New("database is locked"), wantClass: ErrorClassLocalDBLocked, wantHint: "local database is locked"},
		{name: "network chain", err: &net.DNSError{Err: "no such host", Name: "zotero.example.test"}, wantClass: ErrorClassNetwork, wantHint: "network connection failed"},
		{name: "unknown never copies stderr", err: errors.New(pathLaden), wantClass: ErrorClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := ClassifyError(tc.err, tc.envelope)
			if info.Class != tc.wantClass || info.Hint != tc.wantHint || info.HTTPStatus != tc.wantStatus {
				t.Fatalf("ClassifyError() = %+v, want class=%q hint=%q status=%d", info, tc.wantClass, tc.wantHint, tc.wantStatus)
			}
			if len(info.Hint) > maxErrorHintBytes || strings.Contains(info.Hint, "https://") || strings.Contains(info.Hint, "/Users/") {
				t.Fatalf("unsafe error hint %q", info.Hint)
			}
		})
	}
}

func TestSanitizeErrorHintStripsURLsAndPaths(t *testing.T) {
	hint := SanitizeErrorHint("request https://zotero.example.test/users/42 at /Users/reader/private/papio.db C:\\Users\\reader\\papio.db failed")
	if strings.ContainsAny(hint, "/\\") || strings.Contains(hint, "zotero.example.test") || strings.Contains(hint, "reader") {
		t.Fatalf("SanitizeErrorHint leaked private detail: %q", hint)
	}
}

func TestWithErrorInfoPreservesSafeClassification(t *testing.T) {
	wrapped := WithErrorInfo(errors.New("zotio stderr: unknown item field at /Users/reader/item.json"))
	info := ErrorInfoFrom(wrapped)
	if info.Class != ErrorClassZoteroFieldValidation || info.Hint != "unknown item field" {
		t.Fatalf("ErrorInfoFrom() = %+v", info)
	}
}
