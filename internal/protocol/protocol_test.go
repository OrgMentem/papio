// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The shared fixture corpus is the conformance contract: every file under
// testdata/protocol/valid must decode, every file under testdata/protocol/invalid
// must be rejected — by this package and by the extension's TypeScript parser.

package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func decodeByPrefix(t *testing.T, name string, data []byte) error {
	t.Helper()
	switch {
	case strings.HasPrefix(name, "work-request-"):
		_, err := DecodeWorkRequest(data)
		return err
	case strings.HasPrefix(name, "acquisition-bundle-"):
		_, err := DecodeAcquisitionBundle(data)
		return err
	case strings.HasPrefix(name, "browser-"):
		_, err := DecodeBrowserMessage(data)
		return err
	default:
		t.Fatalf("fixture %q has no decoder prefix", name)
		return nil
	}
}

func corpusDir(t *testing.T, kind string) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "protocol", kind)
}

func TestValidCorpusDecodes(t *testing.T) {
	dir := corpusDir(t, "valid")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if len(entries) < 8 {
		t.Fatalf("valid corpus has %d fixtures, want at least 8", len(entries))
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if derr := decodeByPrefix(t, e.Name(), data); derr != nil {
			t.Errorf("valid fixture %s rejected: %v", e.Name(), derr)
		}
	}
}

func TestInvalidCorpusFailsClosed(t *testing.T) {
	dir := corpusDir(t, "invalid")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if len(entries) < 8 {
		t.Fatalf("invalid corpus has %d fixtures, want at least 8", len(entries))
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if derr := decodeByPrefix(t, e.Name(), data); derr == nil {
			t.Errorf("invalid fixture %s was accepted; the contract must fail closed", e.Name())
		}
	}
}

func TestStrictDecodeRejectsTrailingDocuments(t *testing.T) {
	data, err := json.Marshal(WorkRequest{
		SchemaVersion:  WorkRequestSchemaVersion,
		RequestID:      "request-0001",
		Identifiers:    &Identifiers{DOI: "10.1000/example"},
		DesiredVersion: "any",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, trailing := range []string{`{}`, `trailing`} {
		if _, err := DecodeWorkRequest(append(data, trailing...)); err == nil {
			t.Fatalf("DecodeWorkRequest accepted trailing %q", trailing)
		}
	}
}

func TestZotioItemKeyValidationMatchesV1Schema(t *testing.T) {
	bundleFixture, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), "acquisition-bundle-min.json"))
	if err != nil {
		t.Fatal(err)
	}
	var bundleFields map[string]any
	if err := json.Unmarshal(bundleFixture, &bundleFields); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		key     string
		wantErr bool
	}{
		{key: "A"},
		{key: "ab12CD34"},
		{key: strings.Repeat("a", 32)},
		{key: "ab-12", wantErr: true},
		{key: strings.Repeat("a", 33), wantErr: true},
	} {
		t.Run(tc.key, func(t *testing.T) {
			requestData, err := json.Marshal(WorkRequest{
				SchemaVersion:  WorkRequestSchemaVersion,
				RequestID:      "request-0001",
				Identifiers:    &Identifiers{DOI: "10.1000/example"},
				ZotioItemKey:   tc.key,
				DesiredVersion: "any",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeWorkRequest(requestData); (err != nil) != tc.wantErr {
				t.Fatalf("DecodeWorkRequest() error = %v, want error %t", err, tc.wantErr)
			}

			bundleFields["zotio_item_key"] = tc.key
			bundleData, err := json.Marshal(bundleFields)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeAcquisitionBundle(bundleData); (err != nil) != tc.wantErr {
				t.Fatalf("DecodeAcquisitionBundle() error = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}

func TestHelloAckPayloadRoundTripAndBounds(t *testing.T) {
	frame := func(payload any) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     MsgHelloAck,
			"msg_id":   "daemon-ack-001",
			"seq":      1,
			"payload":  payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	msg, err := DecodeBrowserMessage(frame(HelloAckPayload{
		DaemonVersion: "0.1.0",
		Features:      []string{"browser_handoff"},
	}))
	if err != nil {
		t.Fatalf("decode hello_ack: %v", err)
	}
	payload := msg.Payload.(*HelloAckPayload)
	if payload.DaemonVersion != "0.1.0" || len(payload.Features) != 1 || payload.Features[0] != "browser_handoff" {
		t.Fatalf("round-trip payload = %#v", payload)
	}
	if _, err := DecodeBrowserMessage(frame(EmptyPayload{})); err != nil {
		t.Fatalf("empty hello_ack rejected: %v", err)
	}
	if _, err := DecodeBrowserMessage(frame(map[string]any{"daemon_version": strings.Repeat("v", 51)})); err == nil {
		t.Fatal("hello_ack accepted daemon_version longer than 50 chars")
	}
	if _, err := DecodeBrowserMessage(frame(map[string]any{"features": make([]string, 33)})); err == nil {
		t.Fatal("hello_ack accepted more than 32 features")
	}
	if _, err := DecodeBrowserMessage(frame(map[string]any{"features": []any{nil}})); err == nil {
		t.Fatal("hello_ack accepted null feature entry")
	}
	acked, err := DecodeBrowserMessage(frame(HelloAckPayload{
		ResolverOrigins: []string{"https://onesearch.library.example.edu", "https://example.primo.exlibrisgroup.com"},
	}))
	if err != nil {
		t.Fatalf("decode resolver_origins: %v", err)
	}
	if got := acked.Payload.(*HelloAckPayload).ResolverOrigins; len(got) != 2 || got[0] != "https://onesearch.library.example.edu" {
		t.Fatalf("resolver_origins round-trip = %#v", got)
	}
	if _, err := DecodeBrowserMessage(frame(map[string]any{"resolver_origins": make([]string, 33)})); err == nil {
		t.Fatal("hello_ack accepted more than 32 resolver_origins")
	}
	if _, err := DecodeBrowserMessage(frame(map[string]any{"resolver_origins": []any{nil}})); err == nil {
		t.Fatal("hello_ack accepted null resolver_origin entry")
	}
	for _, bad := range []string{"http://insecure.example.edu", "https://example.edu/path", "https://example.edu?x=1", "ftp://example.edu"} {
		if _, err := DecodeBrowserMessage(frame(map[string]any{"resolver_origins": []string{bad}})); err == nil {
			t.Fatalf("hello_ack accepted invalid resolver origin %q", bad)
		}
	}
}

func TestHandoffFocusPayloadRoundTripAndScope(t *testing.T) {
	frame := func(jobID string, payload any) []byte {
		t.Helper()
		env := map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     MsgHandoffFocus,
			"msg_id":   "daemon-focus-001",
			"seq":      1,
			"payload":  payload,
		}
		if jobID != "" {
			env["job_id"] = jobID
		}
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	msg, err := DecodeBrowserMessage(frame("job_focus_001", EmptyPayload{}))
	if err != nil {
		t.Fatalf("decode handoff_focus: %v", err)
	}
	if msg.Type != MsgHandoffFocus || msg.JobID != "job_focus_001" {
		t.Fatalf("handoff_focus envelope = %#v", msg)
	}
	if _, ok := msg.Payload.(*EmptyPayload); !ok {
		t.Fatalf("handoff_focus payload = %T, want *EmptyPayload", msg.Payload)
	}
	if _, err := DecodeBrowserMessage(frame("", EmptyPayload{})); err == nil {
		t.Fatal("handoff_focus without job_id was accepted")
	}
	if _, err := DecodeBrowserMessage(frame("job_focus_001", map[string]bool{"unexpected": true})); err == nil {
		t.Fatal("handoff_focus with non-empty payload was accepted")
	}
}

func TestPageAcquirePayloadRoundTripAndValidation(t *testing.T) {
	frame := func(typ string, payload any) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     typ,
			"msg_id":   "page-acquire-001",
			"seq":      1,
			"payload":  payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	valid := PageAcquirePayload{
		URL: "https://publisher.example.edu/article/42",
		DOI: "10.1000/Example.42", Title: "An Example Paper", Source: "popup",
	}
	msg, err := DecodeBrowserMessage(frame(MsgPageAcquire, valid))
	if err != nil {
		t.Fatalf("decode page_acquire: %v", err)
	}
	if got := msg.Payload.(*PageAcquirePayload); *got != valid {
		t.Fatalf("round-trip payload = %#v, want %#v", got, valid)
	}

	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{name: "missing URL", payload: map[string]any{}},
		{name: "bad scheme", payload: map[string]any{"url": "ftp://publisher.example.edu/article/42"}},
		{name: "oversize DOI", payload: map[string]any{
			"url": "https://publisher.example.edu/article/42", "doi": strings.Repeat("d", 513),
		}},
		{name: "null optional field", payload: map[string]any{
			"url": "https://publisher.example.edu/article/42", "title": nil,
		}},
		{name: "unknown field", payload: map[string]any{
			"url": "https://publisher.example.edu/article/42", "debug": "no",
		}},
		{name: "NUL URL", payload: map[string]any{
			"url": "https://publisher.example.edu/article/\x00",
		}},
		{name: "NUL DOI", payload: map[string]any{
			"url": "https://publisher.example.edu/article/42", "doi": "10.1000/\x00example",
		}},
		{name: "NUL title", payload: map[string]any{
			"url": "https://publisher.example.edu/article/42", "title": "Example\x00 Paper",
		}},
		{name: "NUL source", payload: map[string]any{
			"url": "https://publisher.example.edu/article/42", "source": "pop\x00up",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeBrowserMessage(frame(MsgPageAcquire, tc.payload)); err == nil {
				t.Fatal("page_acquire was accepted")
			}
		})
	}

	errorAck, err := DecodeBrowserMessage(frame(MsgPageAcquireAck, PageAcquireAckPayload{
		Error: "page has no DOI",
	}))
	if err != nil {
		t.Fatalf("decode page_acquire error ack: %v", err)
	}
	if got := errorAck.Payload.(*PageAcquireAckPayload); got.Error != "page has no DOI" || got.JobID != "" || got.Duplicate {
		t.Fatalf("round-trip error ack = %#v", got)
	}

	ack, err := DecodeBrowserMessage(frame(MsgPageAcquireAck, PageAcquireAckPayload{
		JobID: "job_page_acquire_001", Duplicate: true,
	}))
	if err != nil {
		t.Fatalf("decode page_acquire_ack: %v", err)
	}
	if got := ack.Payload.(*PageAcquireAckPayload); got.JobID != "job_page_acquire_001" || !got.Duplicate {
		t.Fatalf("round-trip ack = %#v", got)
	}
	for _, payload := range []map[string]any{
		{"job_id": nil},
		{"duplicate": nil},
		{"error": strings.Repeat("e", 1001)},
		{"unexpected": true},
		{},
		{"duplicate": true},
		{"job_id": "job_page_acquire_001", "error": "already queued"},
		{"error": "bad\x00error"},
		{"error": ""},
		{"job_id": "", "error": "page has no DOI"},
	} {
		if _, err := DecodeBrowserMessage(frame(MsgPageAcquireAck, payload)); err == nil {
			t.Fatalf("page_acquire_ack payload %#v was accepted", payload)
		}
	}
}

func TestPageCapturePayloadRoundTripAndValidation(t *testing.T) {
	frame := func(jobID string, payload any) []byte {
		t.Helper()
		env := map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     MsgPageCapture,
			"msg_id":   "page-capture-001",
			"seq":      1,
			"payload":  payload,
		}
		if jobID != "" {
			env["job_id"] = jobID
		}
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	valid := PageCapturePayload{
		Host: "journals.sagepub.com", Scenario: "observed",
		AdapterID: "sage", AdapterVersion: "0.2.0",
		Encoding: "gzip+base64", Bytes: 56,
		Body: "H4sIAAAAAAACE7NRTMlPLqksSFXIKMnNsbOBkEn5KZV2aZkVJaVFqQrJiQUg2kYfLGqjD1YCAN4m2uc4AAAA",
	}
	msg, err := DecodeBrowserMessage(frame("job_capture_001", valid))
	if err != nil {
		t.Fatalf("decode page_capture: %v", err)
	}
	if got := msg.Payload.(*PageCapturePayload); *got != valid {
		t.Fatalf("round-trip payload = %#v, want %#v", got, valid)
	}
	if msg.JobID != "job_capture_001" {
		t.Fatalf("page_capture job_id = %q", msg.JobID)
	}
	if _, err := DecodeBrowserMessage(frame("", valid)); err != nil {
		t.Fatalf("unscoped page_capture rejected: %v", err)
	}

	for _, tc := range []struct {
		name    string
		payload PageCapturePayload
	}{
		{name: "wrong encoding", payload: func() PageCapturePayload {
			p := valid
			p.Encoding = "base64"
			return p
		}()},
		{name: "malformed base64", payload: func() PageCapturePayload {
			p := valid
			p.Body = "not base64!"
			return p
		}()},
		{name: "oversize bytes", payload: func() PageCapturePayload {
			p := valid
			p.Bytes = 2<<20 + 1
			return p
		}()},
		{name: "bad host", payload: func() PageCapturePayload {
			p := valid
			p.Host = "https://journals.sagepub.com"
			return p
		}()},
		{name: "unknown scenario", payload: func() PageCapturePayload {
			p := valid
			p.Scenario = "unexpected"
			return p
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeBrowserMessage(frame("", tc.payload)); err == nil {
				t.Fatal("page_capture was accepted")
			}
		})
	}
}

func TestPageCaptureRequestRoundTripAndValidation(t *testing.T) {
	frame := func(typ string, payload any) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     typ,
			"msg_id":   "capture-request-frame",
			"seq":      2,
			"payload":  payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	settle := int64(2500)
	request := PageCaptureRequestPayload{
		RequestID: "capture-request-001",
		URL:       "https://journals.example.org/article/42",
		Provider:  "example_provider",
		Scenario:  "success",
		SettleMS:  &settle,
	}
	msg, err := DecodeBrowserMessage(frame(MsgPageCaptureRequest, request))
	if err != nil {
		t.Fatalf("decode page_capture_request: %v", err)
	}
	gotRequest := msg.Payload.(*PageCaptureRequestPayload)
	if gotRequest.RequestID != request.RequestID || gotRequest.URL != request.URL ||
		gotRequest.Provider != request.Provider || gotRequest.Scenario != request.Scenario ||
		gotRequest.SettleMS == nil || *gotRequest.SettleMS != settle {
		t.Fatalf("round-trip request = %#v, want %#v", gotRequest, request)
	}
	result := PageCaptureRequestResultPayload{
		RequestID: request.RequestID,
		Outcome:   "captured",
		Detail:    "stored",
	}
	msg, err = DecodeBrowserMessage(frame(MsgPageCaptureRequestResult, result))
	if err != nil {
		t.Fatalf("decode page_capture_request_result: %v", err)
	}
	if got := msg.Payload.(*PageCaptureRequestResultPayload); *got != result {
		t.Fatalf("round-trip result = %#v, want %#v", got, result)
	}

	for _, payload := range []map[string]any{
		{"request_id": request.RequestID, "url": "http://example.org/article", "provider": "example", "scenario": "success"},
		{"request_id": request.RequestID, "url": request.URL, "provider": "bad provider", "scenario": "success"},
		{"request_id": request.RequestID, "url": request.URL, "provider": "example", "scenario": "observed"},
		{"request_id": request.RequestID, "url": request.URL, "provider": "example", "scenario": "success", "settle_ms": 10001},
		{"request_id": request.RequestID, "url": request.URL, "provider": "example", "scenario": "success", "settle_ms": nil},
	} {
		if _, err := DecodeBrowserMessage(frame(MsgPageCaptureRequest, payload)); err == nil {
			t.Fatalf("invalid page_capture_request payload %#v was accepted", payload)
		}
	}
	for _, payload := range []map[string]any{
		{"request_id": request.RequestID, "outcome": "unknown"},
		{"request_id": request.RequestID, "outcome": "captured", "detail": nil},
		{"request_id": "short", "outcome": "captured"},
	} {
		if _, err := DecodeBrowserMessage(frame(MsgPageCaptureRequestResult, payload)); err == nil {
			t.Fatalf("invalid page_capture_request_result payload %#v was accepted", payload)
		}
	}
}

// The IdP privacy invariant is structural: auth payloads cannot carry a URL.
func TestAuthPayloadRejectsURLFields(t *testing.T) {
	msg := []byte(`{"protocol":"papio-browser/1","type":"auth_returned","msg_id":"m_auth_ret1","job_id":"job_0002_tyler","seq":5,"payload":{"url":"https://idp.example.edu/sso?token=SECRET"}}`)
	if _, err := DecodeBrowserMessage(msg); err == nil {
		t.Fatal("auth_returned payload with url field was accepted")
	}
}

func TestJobOfferLoginEntityIDValidation(t *testing.T) {
	const withoutEntityID = `{"protocol":"papio-browser/1","type":"job_offer","msg_id":"offer-msg-1","job_id":"job_offer_1","seq":1,"payload":{"openurl":"https://resolver.example.edu/openurl","provider_hosts":["example.edu"],"access_mode":"delegated","expires_at":"2026-07-17T12:00:00Z"}}`
	const withEntityID = `{"protocol":"papio-browser/1","type":"job_offer","msg_id":"offer-msg-2","job_id":"job_offer_2","seq":2,"payload":{"openurl":"https://resolver.example.edu/openurl","provider_hosts":["example.edu"],"access_mode":"delegated","login_entity_id":"https://idp.example.edu/entity","expires_at":"2026-07-17T12:00:00Z"}}`
	const nonHTTPS = `{"protocol":"papio-browser/1","type":"job_offer","msg_id":"offer-msg-3","job_id":"job_offer_3","seq":3,"payload":{"openurl":"https://resolver.example.edu/openurl","provider_hosts":["example.edu"],"access_mode":"delegated","login_entity_id":"http://idp.example.edu/entity","expires_at":"2026-07-17T12:00:00Z"}}`

	msg, err := DecodeBrowserMessage([]byte(withEntityID))
	if err != nil {
		t.Fatalf("job_offer with login_entity_id rejected: %v", err)
	}
	if got := msg.Payload.(*JobOfferPayload).LoginEntityID; got != "https://idp.example.edu/entity" {
		t.Fatalf("login_entity_id = %q", got)
	}
	if _, err := DecodeBrowserMessage([]byte(nonHTTPS)); err == nil {
		t.Fatal("job_offer with non-https login_entity_id accepted")
	}
	if _, err := DecodeBrowserMessage([]byte(withoutEntityID)); err != nil {
		t.Fatalf("job_offer without login_entity_id rejected: %v", err)
	}
}

func TestJobOfferProquestAccountIDValidation(t *testing.T) {
	const withoutAccountID = `{"protocol":"papio-browser/1","type":"job_offer","msg_id":"offer-msg-1","job_id":"job_offer_1","seq":1,"payload":{"openurl":"https://resolver.example.edu/openurl","provider_hosts":["example.edu"],"access_mode":"delegated","expires_at":"2026-07-17T12:00:00Z"}}`
	const withAccountID = `{"protocol":"papio-browser/1","type":"job_offer","msg_id":"offer-msg-2","job_id":"job_offer_2","seq":2,"payload":{"openurl":"https://resolver.example.edu/openurl","provider_hosts":["example.edu"],"access_mode":"delegated","proquest_account_id":"12345","expires_at":"2026-07-17T12:00:00Z"}}`
	const nonDigits = `{"protocol":"papio-browser/1","type":"job_offer","msg_id":"offer-msg-3","job_id":"job_offer_3","seq":3,"payload":{"openurl":"https://resolver.example.edu/openurl","provider_hosts":["example.edu"],"access_mode":"delegated","proquest_account_id":"12345x","expires_at":"2026-07-17T12:00:00Z"}}`

	msg, err := DecodeBrowserMessage([]byte(withAccountID))
	if err != nil {
		t.Fatalf("job_offer with proquest_account_id rejected: %v", err)
	}
	if got := msg.Payload.(*JobOfferPayload).ProquestAccountID; got != "12345" {
		t.Fatalf("proquest_account_id = %q", got)
	}
	if _, err := DecodeBrowserMessage([]byte(nonDigits)); err == nil {
		t.Fatal("job_offer with non-digits proquest_account_id accepted")
	}
	if _, err := DecodeBrowserMessage([]byte(withoutAccountID)); err != nil {
		t.Fatalf("job_offer without proquest_account_id rejected: %v", err)
	}
}

func TestBrowserMessageSizeCap(t *testing.T) {
	big := append([]byte(`{"protocol":"papio-browser/1","type":"ack","msg_id":"m_ack_00001","seq":0,"payload":{}} `), make([]byte, MaxBrowserMessageBytes)...)
	if _, err := DecodeBrowserMessage(big); err == nil || !strings.Contains(err.Error(), "exceeds cap") {
		t.Fatalf("oversized frame err = %v, want size-cap rejection", err)
	}
}

// The bundle path must be exactly its content address (cross-field invariant
// stronger than the JSON Schema alone).
func TestBundlePathMustMatchSHA(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(corpusDir(t, "invalid"), "acquisition-bundle-path-mismatch.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, derr := DecodeAcquisitionBundle(data); derr == nil || !strings.Contains(derr.Error(), "must equal") {
		t.Fatalf("path-mismatch err = %v, want content-address mismatch", derr)
	}
}

// Identity invariants are route-aware: a new-item bundle (no zotio_item_key)
// must carry full bibliographic identity, but an attach-to-existing bundle only
// carries an item key + file, so its identity is descriptive and optional. This
// is the `acquire --from-zotio` unblock — authorless Zotero items must still
// attach.
func TestBundleIdentityInvariantsAreRouteAware(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), "acquisition-bundle-min.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	base, err := DecodeAcquisitionBundle(data)
	if err != nil {
		t.Fatalf("decode base fixture: %v", err)
	}

	t.Run("new_item_requires_authors", func(t *testing.T) {
		b := *base
		b.ZotioItemKey = ""
		b.Identity.Authors = nil
		if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "authors") {
			t.Fatalf("authorless new-item bundle err = %v, want authors invariant", err)
		}
	})

	t.Run("new_item_requires_title", func(t *testing.T) {
		b := *base
		b.ZotioItemKey = ""
		b.Identity.Title = "ab"
		if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "title") {
			t.Fatalf("short-title new-item bundle err = %v, want title invariant", err)
		}
	})

	t.Run("attach_allows_missing_identity", func(t *testing.T) {
		b := *base
		b.ZotioItemKey = "AB12CD34"
		b.Identity.Authors = nil
		b.Identity.Title = ""
		if err := b.Validate(); err != nil {
			t.Fatalf("authorless attach bundle rejected: %v", err)
		}
	})

	t.Run("attach_still_bounds_authors", func(t *testing.T) {
		b := *base
		b.ZotioItemKey = "AB12CD34"
		big := make([]string, 101)
		for i := range big {
			big[i] = "x"
		}
		b.Identity.Authors = big
		if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "authors") {
			t.Fatalf("101-author attach bundle err = %v, want upper-bound rejection", err)
		}
	})
}

func TestTriageFixturePayloadRoundTrips(t *testing.T) {
	cases := map[string]string{
		"browser-triage-snapshot-request.json":     MsgTriageSnapshotRequest,
		"browser-triage-snapshot-response.json":    MsgTriageSnapshotResponse,
		"browser-triage-counts-request.json":       MsgTriageCountsRequest,
		"browser-triage-counts-request-v2.json":    MsgTriageCountsRequest,
		"browser-triage-counts-response.json":      MsgTriageCountsResponse,
		"browser-triage-counts-response-v2.json":   MsgTriageCountsResponse,
		"browser-session-evidence.json":            MsgSessionEvidence,
		"browser-triage-decide.json":               MsgTriageDecide,
		"browser-triage-decide-result.json":        MsgTriageDecideResult,
		"browser-human-action-resolve.json":        MsgHumanActionResolve,
		"browser-human-action-resolve-result.json": MsgHumanActionResolveResult,
		"browser-review-preview-request.json":      MsgReviewPreviewRequest,
		"browser-review-preview-result.json":       MsgReviewPreviewResult,
		"browser-stats-request.json":               MsgStatsRequest,
		"browser-stats-response.json":              MsgStatsResponse,
		"browser-activity-request.json":            MsgActivityRequest,
		"browser-activity-request-default.json":    MsgActivityRequest,
		"browser-activity-response.json":           MsgActivityResponse,
	}
	for name, wantType := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), name))
			if err != nil {
				t.Fatal(err)
			}
			message, err := DecodeBrowserMessage(data)
			if err != nil {
				t.Fatal(err)
			}
			if message.Type != wantType {
				t.Fatalf("type = %q, want %q", message.Type, wantType)
			}
			encoded, err := json.Marshal(map[string]any{
				"protocol": message.Protocol, "type": message.Type, "msg_id": message.MsgID,
				"seq": message.Seq, "payload": message.Payload,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeBrowserMessage(encoded); err != nil {
				t.Fatalf("round-trip decode: %v", err)
			}
		})
	}
}

func TestActivityRequestDefaultsLimit(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), "browser-activity-request-default.json"))
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodeBrowserMessage(data)
	if err != nil {
		t.Fatal(err)
	}
	payload := message.Payload.(*ActivityRequestPayload)
	if payload.Limit != 20 {
		t.Fatalf("activity request limit = %d, want default 20", payload.Limit)
	}
}

func TestTriageSnapshotRejectsUnknownBlockedBy(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), "browser-triage-snapshot-response.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	payload := frame["payload"].(map[string]any)
	items := payload["items"].([]any)
	items[1].(map[string]any)["blocked_by"] = "captcha"
	mutated, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(mutated); err == nil || !strings.Contains(err.Error(), "blocked_by") {
		t.Fatalf("unknown blocked_by err = %v, want rejection", err)
	}
}

func TestTriageSnapshotSchema1RejectsAccessFieldsButAllowsTheirAbsence(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), "browser-triage-snapshot-response.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	payload := frame["payload"].(map[string]any)
	payload["schema"] = float64(1)
	action := payload["items"].([]any)[1].(map[string]any)
	delete(action, "requires_auth")
	delete(action, "blocked_by")
	legacy, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(legacy); err != nil {
		t.Fatalf("schema-1 snapshot without access fields rejected: %v", err)
	}

	action["requires_auth"] = true
	action["blocked_by"] = "paywall"
	withAccessFields, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(withAccessFields); err == nil || !strings.Contains(err.Error(), "schema 1") {
		t.Fatalf("schema-1 snapshot with access fields err = %v, want rejection", err)
	}
}

// TestEntitlementWireShapeIsCaseInsensitive pins the fail-open a review found
// in the guard itself. encoding/json matches struct fields case-insensitively,
// so `"Entitlement": null` populates the decoded struct — while a raw map
// lookup is case-sensitive and would miss it, letting a v1 document carry a key
// its frozen schema forbids.
func TestEntitlementWireShapeIsCaseInsensitive(t *testing.T) {
	base, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), "acquisition-bundle-v2-entitlement.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		from string
		to   string
	}{
		{name: "null entitlement in v1", from: `"schema_version": "acquisition-bundle/2"`, to: `"schema_version": "acquisition-bundle/1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Replace(string(base), tc.from, tc.to, 1)
			doc = regexp.MustCompile(`(?s)"entitlement": \{.*?\}`).ReplaceAllString(doc, `"Entitlement": null`)
			if _, err := DecodeAcquisitionBundle([]byte(doc)); err == nil {
				t.Fatal("a v1 bundle carrying \"Entitlement\": null was accepted")
			}
		})
	}

	mixed := regexp.MustCompile(`"entitlement_ref": "[^"]*"`).ReplaceAllString(string(base), `"Entitlement_Ref": ""`)
	if _, err := DecodeAcquisitionBundle([]byte(mixed)); err == nil {
		t.Fatal("an empty \"Entitlement_Ref\" was accepted")
	}
}

// TestBareRouteIsNeverLaxerThanThePublishedSchema keeps the Go validator at
// least as strict as `^https://[^/?#@]+$`. url.Parse lowercases the scheme, so
// a u.Scheme comparison silently accepted "HTTPS://host" that the published
// pattern rejects — laxer than the contract, in the direction that matters.
func TestBareRouteIsNeverLaxerThanThePublishedSchema(t *testing.T) {
	pattern := regexp.MustCompile(`^https://[^/?#@]+$`)
	for _, route := range []string{
		"https://example.org",
		"https://example.org:8443",
		"https://[2001:db8::1]:443",
		"HTTPS://example.org",
		"HttpS://example.org",
		"https://example.org/",
		"https://example.org/paper.pdf",
		"https://user:pass@example.org",
		"https://example.org?a=b",
		"https://example.org#f",
		"http://example.org",
		"https://",
		"",
		"https://" + strings.Repeat("a", 2100),
	} {
		goOK := validateBareRoute(route) == nil
		schemaOK := pattern.MatchString(route) && utf8.RuneCountInString(route) <= 2000
		if goOK && !schemaOK {
			t.Errorf("route %q: Go accepts what the published schema rejects", route)
		}
	}
}
