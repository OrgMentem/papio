// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package illiad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateTransactionHappyPath(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("ApiKey")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode posted body: %v", err)
		}
		_, _ = w.Write([]byte(`{"TransactionNumber": 4821, "TransactionStatus": "Awaiting Request Processing", "CreationDate": "2026-08-07T00:00:00"}`))
	}))
	defer server.Close()

	client := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL, APIKey: "campus-secret"})
	tx, err := client.CreateTransaction(context.Background(), TransactionRequest{
		Username:           "jstudent",
		PhotoJournalTitle:  "Useful Journal",
		PhotoArticleTitle:  "A Grounded Result",
		PhotoArticleAuthor: "A. Author",
		DOI:                "10.5555/example",
		ReferenceField:     "ItemInfo4",
		ReferenceValue:     "papio:idem:abc123",
	})
	if err != nil {
		t.Fatalf("CreateTransaction: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/Transaction/" {
		t.Fatalf("path = %q, want /Transaction/", gotPath)
	}
	if gotAPIKey != "campus-secret" {
		t.Fatalf("ApiKey header = %q, want campus-secret", gotAPIKey)
	}
	if gotBody["ProcessType"] != "Borrowing" {
		t.Fatalf("ProcessType = %v, want the default \"Borrowing\"", gotBody["ProcessType"])
	}
	if gotBody["RequestType"] != "Article" {
		t.Fatalf("RequestType = %v, want the default \"Article\"", gotBody["RequestType"])
	}
	if gotBody["ItemInfo4"] != "papio:idem:abc123" {
		t.Fatalf("reference field ItemInfo4 = %v, want the idempotency token in the configured field", gotBody["ItemInfo4"])
	}
	if _, ok := gotBody["ReferenceField"]; ok {
		t.Fatalf("posted body leaked the literal ReferenceField key: %v", gotBody)
	}
	for key := range gotBody {
		if strings.Contains(strings.ToLower(key), "copyright") {
			t.Fatalf("posted body carries a copyright-compliance key %q; papio must never set one outside an institution-approved mapping", key)
		}
	}
	raw, err := json.Marshal(gotBody)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "copyright") {
		t.Fatalf("posted JSON contains \"copyright\": %s", raw)
	}

	if tx.TransactionNumber != 4821 || tx.TransactionStatus != "Awaiting Request Processing" {
		t.Fatalf("transaction = %+v", tx)
	}
}

func TestMarshalJSONRejectsCopyrightReferenceField(t *testing.T) {
	req := TransactionRequest{
		Username:       "jstudent",
		ReferenceField: "CopyrightAlreadyPaid",
		ReferenceValue: "true",
	}
	if _, err := json.Marshal(req); err == nil {
		t.Fatal("expected Marshal to reject a copyright-named reference field")
	}
}

func TestCreateTransactionRequiresUserIdentity(t *testing.T) {
	client := New(http.DefaultClient, "https://illiad.campus.example.edu", "key")
	if _, err := client.CreateTransaction(context.Background(), TransactionRequest{}); err == nil {
		t.Fatal("expected an error when neither Username nor ExternalUserID is set")
	}
}

func TestGetTransactionParsesRecord(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"TransactionNumber": 501,
			"TransactionStatus": "Delivered to Web",
			"CreationDate": "2026-08-01T12:00:00",
			"PhotoArticleTitle": "A Grounded Result",
			"DOI": "10.5555/example",
			"ItemInfo4": "papio:idem:abc123"
		}`))
	}))
	defer server.Close()

	client := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL, APIKey: "key"})
	tx, err := client.GetTransaction(context.Background(), 501)
	if err != nil {
		t.Fatalf("GetTransaction: %v", err)
	}
	if gotPath != "/Transaction/501" {
		t.Fatalf("path = %q, want /Transaction/501", gotPath)
	}
	if tx.TransactionNumber != 501 || tx.TransactionStatus != "Delivered to Web" || tx.PhotoArticleTitle != "A Grounded Result" {
		t.Fatalf("transaction = %+v", tx)
	}
	if v, ok := tx.ReferenceValue("ItemInfo4"); !ok || v != "papio:idem:abc123" {
		t.Fatalf("ReferenceValue(ItemInfo4) = (%q, %v), want (papio:idem:abc123, true)", v, ok)
	}
	if _, ok := tx.ReferenceValue("SpecialID"); ok {
		t.Fatalf("ReferenceValue(SpecialID) should be false: only the five ItemInfo fields are echoed")
	}
}

func TestGetTransactionNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL, APIKey: "key"})
	_, err := client.GetTransaction(context.Background(), 9999)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTransaction error = %v, want ErrNotFound", err)
	}
}

func TestUserRequestsParsesList(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		switch r.URL.EscapedPath() {
		case "/Users/ExternalUserId/jstudent%2Fcampus%20id@campus.example.edu":
			_, _ = w.Write([]byte(`{"UserName":"jstudent"}`))
		case "/Transaction/UserRequests/jstudent":
			_, _ = w.Write([]byte(`[
				{"TransactionNumber": 1, "TransactionStatus": "Delivered to Web", "CreationDate": "2026-08-01T00:00:00", "ItemInfo4": "papio:idem:a"},
				{"TransactionNumber": 2, "TransactionStatus": "Cancelled by ILL Staff", "CreationDate": "2026-08-02T00:00:00", "ItemInfo4": "papio:idem:b"}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL, APIKey: "key"})
	txs, err := client.UserRequests(context.Background(), "jstudent/campus id@campus.example.edu")
	if err != nil {
		t.Fatalf("UserRequests: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/Users/ExternalUserId/jstudent%2Fcampus%20id@campus.example.edu" || paths[1] != "/Transaction/UserRequests/jstudent" {
		t.Fatalf("paths = %v, want external-id resolution followed by UserRequests", paths)
	}
	if len(txs) != 2 {
		t.Fatalf("transactions = %d, want 2", len(txs))
	}
	if txs[0].TransactionNumber != 1 || txs[1].TransactionNumber != 2 {
		t.Fatalf("transactions = %+v", txs)
	}
	if v, _ := txs[1].ReferenceValue("ItemInfo4"); v != "papio:idem:b" {
		t.Fatalf("second transaction reference value = %q, want papio:idem:b", v)
	}
}

func TestUserRequestsRejectsEmptyReference(t *testing.T) {
	client := New(http.DefaultClient, "https://illiad.campus.example.edu", "key")
	if _, err := client.UserRequests(context.Background(), "  "); err == nil {
		t.Fatal("expected an error for an empty user reference")
	}
}

func TestErrorClassification(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		retryAfter    string
		wantCred      bool
		wantTemporary bool
		wantWait      time.Duration
	}{
		{name: "401 is a credential error", status: http.StatusUnauthorized, wantCred: true},
		{name: "403 is a credential error", status: http.StatusForbidden, wantCred: true},
		{name: "408 is temporary", status: http.StatusRequestTimeout, wantTemporary: true},
		{name: "429 is temporary with retry-after", status: http.StatusTooManyRequests, retryAfter: "12", wantTemporary: true, wantWait: 12 * time.Second},
		{name: "500 is temporary", status: http.StatusInternalServerError, wantTemporary: true},
		{name: "503 is temporary", status: http.StatusServiceUnavailable, wantTemporary: true},
		{name: "400 is a hard failure", status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				w.WriteHeader(test.status)
			}))
			defer server.Close()

			client := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL, APIKey: "key"})
			_, err := client.GetTransaction(context.Background(), 1)
			if err == nil {
				t.Fatal("expected an error")
			}

			var credErr *CredentialError
			isCred := errors.As(err, &credErr)
			if isCred != test.wantCred {
				t.Fatalf("errors.As(*CredentialError) = %v, want %v (err=%v)", isCred, test.wantCred, err)
			}

			wait, temporary := Temporary(err)
			if temporary != test.wantTemporary {
				t.Fatalf("Temporary(%v) = %v, want %v", err, temporary, test.wantTemporary)
			}
			if test.wantWait != 0 && wait != test.wantWait {
				t.Fatalf("retry wait = %v, want %v", wait, test.wantWait)
			}
		})
	}
}

func TestFailureClassOfFailsClosed(t *testing.T) {
	var nilFailure *FailureError
	cause := errors.New("transport failed")

	for _, test := range []struct {
		name      string
		err       error
		wantClass FailureClass
		wantOK    bool
	}{
		{name: "missing error fails closed", wantClass: FailureAmbiguous},
		{name: "typed nil failure fails closed", err: nilFailure, wantClass: FailureAmbiguous},
		{name: "pre-send stays replayable", err: &FailureError{Class: FailurePreSend, Err: cause}, wantClass: FailurePreSend, wantOK: true},
		{name: "ambiguous stays non-replayable", err: &FailureError{Class: FailureAmbiguous, Err: cause}, wantClass: FailureAmbiguous, wantOK: true},
		{name: "missing class stays ambiguous", err: &FailureError{Err: cause}, wantClass: FailureAmbiguous},
		{name: "unknown class stays ambiguous", err: &FailureError{Class: FailureClass("unrecognised"), Err: cause}, wantClass: FailureAmbiguous},
		{name: "empty class stays ambiguous", err: &FailureError{Class: "", Err: cause}, wantClass: FailureAmbiguous},
	} {
		t.Run(test.name, func(t *testing.T) {
			gotClass, gotOK := FailureClassOf(test.err)
			if gotClass != test.wantClass || gotOK != test.wantOK {
				t.Fatalf("FailureClassOf(%v) = (%q, %v), want (%q, %v)", test.err, gotClass, gotOK, test.wantClass, test.wantOK)
			}
		})
	}
}

func TestFailureErrorPreservesCauseAndClassification(t *testing.T) {
	cause := errors.New("transport failed")

	for _, test := range []struct {
		name      string
		err       error
		wantText  string
		wantCause error
		wantClass FailureClass
	}{
		{
			name:      "nil cause explains trusted class",
			err:       &FailureError{Class: FailurePreSend},
			wantText:  "illiad: classified request failure (pre_send)",
			wantClass: FailurePreSend,
		},
		{
			name:      "cause text remains visible",
			err:       &FailureError{Class: FailureAmbiguous, Err: cause},
			wantText:  "transport failed",
			wantCause: cause,
			wantClass: FailureAmbiguous,
		},
		{
			name:      "wrapped failure remains discoverable",
			err:       fmt.Errorf("submit request: %w", &FailureError{Class: FailurePreSend, Err: cause}),
			wantText:  "submit request: transport failed",
			wantCause: cause,
			wantClass: FailurePreSend,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.err.Error(); got != test.wantText {
				t.Fatalf("Error() = %q, want %q", got, test.wantText)
			}
			if test.wantCause != nil && !errors.Is(test.err, test.wantCause) {
				t.Fatalf("errors.Is(%v, cause) = false, want true", test.err)
			}

			var failure *FailureError
			if !errors.As(test.err, &failure) {
				t.Fatalf("errors.As(%v, *FailureError) = false, want true", test.err)
			}
			if failure.Class != test.wantClass {
				t.Fatalf("FailureError.Class = %q, want %q", failure.Class, test.wantClass)
			}
		})
	}
}

func TestParseRetryAfterClampsHugeValues(t *testing.T) {
	for _, value := range []string{"9223372037", "9999999999", "922337203685477581"} {
		if got := parseRetryAfter(value, time.Now()); got < 0 {
			t.Fatalf("parseRetryAfter(%q) = %v, want a non-negative clamped duration: a negative wait inverts Retry-After into an immediate retry storm", value, got)
		}
	}
	if got := parseRetryAfter("12", time.Now()); got != 12*time.Second {
		t.Fatalf("parseRetryAfter(12) = %v", got)
	}
}

func TestRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"TransactionNumber": 1, "TransactionStatus": "` + strings.Repeat("x", 512) + `"}`))
	}))
	defer server.Close()

	client := NewWithOptions(Options{Client: http.DefaultClient, BaseURL: server.URL, APIKey: "key", MaxResponseBytes: 64})
	_, err := client.GetTransaction(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("GetTransaction = %v, want a size-limit rejection", err)
	}
	if _, temporary := Temporary(err); temporary {
		t.Fatalf("an oversized body is a malformed response, not a retryable condition: %v", err)
	}
}
