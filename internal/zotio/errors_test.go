// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		{
			name:       "zotero file storage refused http 413",
			err:        errors.New("zotio import apply failed"),
			envelope:   json.RawMessage(`{"ok":false,"error":{"http_status":413,"message":"payload too large"}}`),
			wantClass:  ErrorClassZoteroFileStorageRefused,
			wantHint:   "Zotero file storage refused upload (HTTP 413)",
			wantStatus: 413,
		},
		{
			// zotio HTML-escapes the separator in its reason text.
			name:       "zotero storage quota html escaped",
			err:        errors.New("zotio import apply failed"),
			envelope:   json.RawMessage(`{"ok":false,"error":{"http_status":413,"message":"authorizing upload: File would exceed quota (300.4 &gt; 300)"}}`),
			wantClass:  ErrorClassZoteroStorageQuota,
			wantHint:   "Zotero storage plan is full (300.4 of 300 MB used)",
			wantStatus: 413,
		},
		{
			// The shape a real row holds, copied from the operator's exports
			// ledger: zotio's "&gt;" with the "&" escaped again by
			// encoding/json. The nested reason also carries a URL path, so this
			// case additionally proves the hint survives sanitisation — an
			// earlier version returned "" here because the raw reason contained
			// slashes.
			name:       "zotero storage quota as stored in the ledger",
			err:        errors.New("Zotero HTTP 413"),
			envelope:   json.RawMessage(`{"ok":false,"result":{"items":[{"status":"failed","reason":"attachment item 33ZSEDQ9 created but its file is not registered: authorizing upload: the upload exceeds the library owner's Zotero storage quota (HTTP 413): POST /items/33ZSEDQ9/file returned HTTP 413: File would exceed quota (300.4 \u0026gt; 300)"}]},"error":{"http_status":413}}`),
			wantClass:  ErrorClassZoteroStorageQuota,
			wantHint:   "Zotero storage plan is full (300.4 of 300 MB used)",
			wantStatus: 413,
		},
		{
			name:       "zotero storage quota bare separator",
			err:        errors.New("zotio import apply failed"),
			envelope:   json.RawMessage(`{"ok":false,"error":{"http_status":413,"message":"File would exceed quota (1024 > 1000)"}}`),
			wantClass:  ErrorClassZoteroStorageQuota,
			wantHint:   "Zotero storage plan is full (1024 of 1000 MB used)",
			wantStatus: 413,
		},
		{
			// A quota refusal Zotero did not put figures on is still a quota
			// refusal; the class must not depend on the numbers parsing.
			name:       "zotero storage quota without figures",
			err:        errors.New("zotio import apply failed"),
			envelope:   json.RawMessage(`{"ok":false,"error":{"http_status":413,"message":"upload exceeds the library owner's storage quota"}}`),
			wantClass:  ErrorClassZoteroStorageQuota,
			wantHint:   "Zotero storage plan is full",
			wantStatus: 413,
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
		{name: "bundle validation title", err: errors.New("planning job job_deadbeef: bundle validation: identity has no citation title: resolve bibliographic metadata from the identifier or supply title and authors on the work request"), wantClass: ErrorClassBundleValidation, wantHint: "no citation record for this paper: verify the DOI or supply title and authors"},
		{name: "already in library manifest", err: errors.New(`unsupported Zotio manifest outcome action="skip" classification="duplicate"`), wantClass: ErrorClassAlreadyInLibrary, wantHint: "paper already in Zotero library"},
		{name: "unknown carries sanitized text", err: errors.New(pathLaden), wantClass: ErrorClassUnknown, wantHint: SanitizeErrorHint(pathLaden)},
		{name: "routing requires doi", err: errors.New("planning job job_deadbeef: new-item Zotio routing requires a DOI"), wantClass: ErrorClassRoutingRequiresDOI, wantHint: newItemRoutingRefusal},
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

func TestClassifyErrorDistinguishesCancelFromTimeout(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantClass string
		wantHint  string
	}{
		{"wrapped context canceled", fmt.Errorf("zotio command canceled: %w", context.Canceled), ErrorClassZotioCanceled, "Zotio command canceled"},
		{"bare context canceled", context.Canceled, ErrorClassZotioCanceled, "Zotio command canceled"},
		{"canceled message only", errors.New("zotio command canceled"), ErrorClassZotioCanceled, "Zotio command canceled"},
		{"wrapped deadline exceeded", fmt.Errorf("zotio command timed out after 30s: %w", context.DeadlineExceeded), ErrorClassZotioExecTimeout, "Zotio command timed out"},
		{"bare deadline exceeded", context.DeadlineExceeded, ErrorClassZotioExecTimeout, "Zotio command timed out"},
		{"timeout message only", errors.New("zotio command timed out after 2m0s"), ErrorClassZotioExecTimeout, "Zotio command timed out"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := ClassifyError(tc.err)
			if info.Class != tc.wantClass || info.Hint != tc.wantHint {
				t.Fatalf("ClassifyError() = %+v, want class=%q hint=%q", info, tc.wantClass, tc.wantHint)
			}
		})
	}
}

func TestWithErrorInfoPreservesSafeClassification(t *testing.T) {
	wrapped := WithErrorInfo(errors.New("zotio stderr: unknown item field at /Users/reader/item.json"))
	info := ErrorInfoFrom(wrapped)
	if info.Class != ErrorClassZoteroFieldValidation || info.Hint != "unknown item field" {
		t.Fatalf("ErrorInfoFrom() = %+v", info)
	}
}

// zotio refuses a stored upload with a structured precondition and no HTTP
// status when the library keeps its files on the operator's own file store.
// That refusal used to fall through to `unknown`, which no doctor check reads.
func TestClassifyZotioFileStoragePreconditionRefusal(t *testing.T) {
	err := errors.New(`zotio attachments: {"kind":"precondition_unmet","capability":"attachments add","precondition":"zotero_file_storage","detail":"Zotero desktop keeps personal-library attachment files on WebDAV"}`)
	info := ClassifyError(err)
	if info.Class != ErrorClassZoteroFileStorageRefused {
		t.Fatalf("class = %q, want %q", info.Class, ErrorClassZoteroFileStorageRefused)
	}
	if info.Hint == "" || strings.Contains(info.Hint, "webdav") {
		t.Fatalf("hint = %q, want a non-empty hint that names no host", info.Hint)
	}
}

// A byte-exact cut produced hints like "attachments add requir", which reads as
// corruption rather than elision.
func TestSanitizeErrorHintCutsOnWordBoundary(t *testing.T) {
	hint := SanitizeErrorHint(strings.Repeat("attachments add requires a live library ", 6))
	if len(hint) > maxErrorHintBytes {
		t.Fatalf("hint is %d bytes, want at most %d", len(hint), maxErrorHintBytes)
	}
	if !strings.HasSuffix(hint, "...") {
		t.Fatalf("hint = %q, want a trailing ellipsis marking the elision", hint)
	}
	trimmed := strings.TrimSuffix(hint, "...")
	if strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, "requir") {
		t.Fatalf("hint = %q, want a cut on a word boundary", hint)
	}
}
