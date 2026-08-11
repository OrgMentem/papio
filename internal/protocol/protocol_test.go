// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The shared fixture corpus is the conformance contract: every file under
// testdata/protocol/valid must decode, every file under testdata/protocol/invalid
// must be rejected — by this package and by the extension's TypeScript parser.

package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

func TestHelloPayloadFeaturesStrictBoundsAndParity(t *testing.T) {
	frame := func(payload any) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     MsgHello,
			"msg_id":   "client-hello-1",
			"seq":      0,
			"payload":  payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	msg, err := DecodeBrowserMessage(frame(HelloPayload{
		ExtensionVersion: "0.14.0",
		Features:         []string{InstitutionalMaterializationFeature, "future_capability"},
	}))
	if err != nil {
		t.Fatalf("hello with negotiated features rejected: %v", err)
	}
	if got := msg.Payload.(*HelloPayload).Features; !slices.Equal(got, []string{InstitutionalMaterializationFeature, "future_capability"}) {
		t.Fatalf("decoded hello features = %#v", got)
	}
	features := make([]string, 33)
	for i := range features {
		features[i] = fmt.Sprintf("future_%d", i)
	}
	for name, payload := range map[string]any{
		"unknown field":     map[string]any{"extension_version": "0.14.0", "unknown": true},
		"duplicate feature": map[string]any{"extension_version": "0.14.0", "features": []string{"future_capability", "future_capability"}},
		"invalid feature":   map[string]any{"extension_version": "0.14.0", "features": []string{"Future"}},
		"over 32 features":  map[string]any{"extension_version": "0.14.0", "features": features},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeBrowserMessage(frame(payload)); err == nil {
				t.Fatalf("hello payload accepted: %#v", payload)
			}
		})
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

func TestProviderDriveEpochScopeAndOutcomes(t *testing.T) {
	frame := func(typ string, jobID any, payload map[string]any) []byte {
		t.Helper()
		env := map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     typ,
			"msg_id":   "epoch-msg-001",
			"seq":      1,
			"payload":  payload,
		}
		if jobID != nil {
			env["job_id"] = jobID
		}
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	base := map[string]any{
		"drive_attempt_id": "epoch-attempt-001",
		"ordinal":          0,
		"strategy":         "generic",
		"revision":         "1",
	}
	for _, tc := range []struct {
		typ     string
		outcome string
	}{
		{MsgProviderDriveEpochStartRequest, ""},
		{MsgProviderDriveEpochStartResult, "started"},
		{MsgProviderDriveEpochResultRequest, "strategy_outcome"},
		{MsgProviderDriveEpochResult, "applied"},
	} {
		t.Run(tc.typ+"/valid", func(t *testing.T) {
			payload := map[string]any{}
			for key, value := range base {
				payload[key] = value
			}
			if tc.outcome != "" {
				payload["outcome"] = tc.outcome
			}
			if _, err := DecodeBrowserMessage(frame(tc.typ, "job-epoch-001", payload)); err != nil {
				t.Fatalf("valid epoch frame rejected: %v", err)
			}
		})
		for _, missing := range []struct {
			name  string
			jobID any
		}{
			{name: "missing", jobID: nil},
			{name: "empty", jobID: ""},
		} {
			t.Run(tc.typ+"/job_id_"+missing.name, func(t *testing.T) {
				payload := map[string]any{}
				for key, value := range base {
					payload[key] = value
				}
				if tc.outcome != "" {
					payload["outcome"] = tc.outcome
				}
				if _, err := DecodeBrowserMessage(frame(tc.typ, missing.jobID, payload)); err == nil {
					t.Fatalf("epoch frame with %s job_id was accepted", missing.name)
				}
			})
		}
	}
	start := map[string]any{}
	for key, value := range base {
		start[key] = value
	}
	start["outcome"] = "applied"
	if _, err := DecodeBrowserMessage(frame(MsgProviderDriveEpochStartResult, "job-epoch-001", start)); err == nil {
		t.Fatal("start result accepted terminal outcome")
	}
	terminal := map[string]any{}
	for key, value := range base {
		terminal[key] = value
	}
	terminal["outcome"] = "started"
	if _, err := DecodeBrowserMessage(frame(MsgProviderDriveEpochResult, "job-epoch-001", terminal)); err == nil {
		t.Fatal("terminal result accepted start outcome")
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

	// request_id is optional — an unsolicited capture omits it, and that
	// absence is what lets the daemon refuse to bind it to a pending request
	// (papio-85a7420f4cd2564f). A requested capture echoes one, which must
	// round-trip and must be a real correlation id.
	echoed := valid
	echoed.RequestID = "DRA6SOdBEB1ZgMIRV8qfqQ"
	msg, err = DecodeBrowserMessage(frame("", echoed))
	if err != nil {
		t.Fatalf("decode page_capture with request_id: %v", err)
	}
	if got := msg.Payload.(*PageCapturePayload); *got != echoed {
		t.Fatalf("round-trip echoed payload = %#v, want %#v", got, echoed)
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
		{name: "malformed request_id", payload: func() PageCapturePayload {
			p := valid
			p.RequestID = "short"
			return p
		}()},
		{name: "request_id with illegal charset", payload: func() PageCapturePayload {
			p := valid
			p.RequestID = "has spaces in it"
			return p
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeBrowserMessage(frame("", tc.payload)); err == nil {
				t.Fatal("page_capture was accepted")
			}
		})
	}

	// The struct table above cannot express these: `omitempty` drops an empty
	// string, so a present-but-empty field only exists on the wire. Both the
	// TS parser and the schema reject "" (their charset bound has a minimum
	// length), and an absent field is the only shape that means "unsolicited",
	// so Go must not quietly treat "" as absence.
	for _, field := range []string{"adapter_id", "request_id"} {
		t.Run("empty "+field, func(t *testing.T) {
			payload := map[string]any{
				"host": valid.Host, "scenario": valid.Scenario,
				"encoding": valid.Encoding, "bytes": valid.Bytes, "body": valid.Body,
				field: "",
			}
			if _, err := DecodeBrowserMessage(frame("", payload)); err == nil {
				t.Fatalf("page_capture with an empty %s was accepted", field)
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
		"browser-triage-snapshot-request.json":                   MsgTriageSnapshotRequest,
		"browser-triage-snapshot-request-v4.json":                MsgTriageSnapshotRequest,
		"browser-triage-snapshot-response.json":                  MsgTriageSnapshotResponse,
		"browser-triage-snapshot-response-v3.json":               MsgTriageSnapshotResponse,
		"browser-triage-snapshot-response-v4.json":               MsgTriageSnapshotResponse,
		"browser-triage-snapshot-downloads-access-required.json": MsgTriageSnapshotResponse,
		"browser-triage-counts-request.json":                     MsgTriageCountsRequest,
		"browser-triage-counts-request-v2.json":                  MsgTriageCountsRequest,
		"browser-triage-counts-response.json":                    MsgTriageCountsResponse,
		"browser-triage-counts-response-v2.json":                 MsgTriageCountsResponse,
		"browser-session-evidence.json":                          MsgSessionEvidence,
		"browser-triage-decide.json":                             MsgTriageDecide,
		"browser-triage-decide-result.json":                      MsgTriageDecideResult,
		"browser-human-action-resolve.json":                      MsgHumanActionResolve,
		"browser-human-action-resolve-result.json":               MsgHumanActionResolveResult,
		"browser-review-preview-request.json":                    MsgReviewPreviewRequest,
		"browser-review-preview-result.json":                     MsgReviewPreviewResult,
		"browser-stats-request.json":                             MsgStatsRequest,
		"browser-stats-response.json":                            MsgStatsResponse,
		"browser-activity-request.json":                          MsgActivityRequest,
		"browser-activity-request-default.json":                  MsgActivityRequest,
		"browser-activity-response.json":                         MsgActivityResponse,
		"browser-page-bulk-status-request.json":                  MsgPageBulkStatusRequest,
		"browser-page-bulk-status-result.json":                   MsgPageBulkStatusResult,
		"browser-page-bulk-submit-request.json":                  MsgPageBulkSubmitRequest,
		"browser-page-bulk-submit-result.json":                   MsgPageBulkSubmitResult,
		"browser-handoff-link-request.json":                      MsgHandoffLinkRequest,
		"browser-handoff-link-result-opened.json":                MsgHandoffLinkResult,
		"browser-handoff-link-result-refusal.json":               MsgHandoffLinkResult,
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

func TestPageBulkPayloadRoundTripAndValidation(t *testing.T) {
	frame := func(typ string, payload any) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     typ,
			"msg_id":   "page-bulk-msg-0001",
			"seq":      1,
			"payload":  payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	hint := int64(12)
	statusRequest := PageBulkStatusRequestPayload{
		RequestID: "request-bulk-0001", ScanID: "scan-bulk-0001",
		Identifiers: []PageBulkIdentifier{
			{LocalID: "row-1", Kind: "doi", Value: "10.1000/example.42"},
			{LocalID: "row-2", Kind: "pmid", Value: "12345678"},
			{LocalID: "row-3", Kind: "openalex", Value: "W2741809807"},
		},
		RenderedRecordCountHint: &hint,
	}
	msg, err := DecodeBrowserMessage(frame(MsgPageBulkStatusRequest, statusRequest))
	if err != nil {
		t.Fatalf("decode page_bulk_status_request: %v", err)
	}
	got := msg.Payload.(*PageBulkStatusRequestPayload)
	if got.RequestID != statusRequest.RequestID || got.ScanID != statusRequest.ScanID || len(got.Identifiers) != 3 ||
		got.RenderedRecordCountHint == nil || *got.RenderedRecordCountHint != hint {
		t.Fatalf("round-trip status request = %#v, want %#v", got, statusRequest)
	}

	noHintRequest := PageBulkStatusRequestPayload{
		RequestID: "request-bulk-0001", ScanID: "scan-bulk-0001",
		Identifiers: []PageBulkIdentifier{{LocalID: "row-1", Kind: "doi", Value: "10.1000/example.42"}},
	}
	msg, err = DecodeBrowserMessage(frame(MsgPageBulkStatusRequest, noHintRequest))
	if err != nil {
		t.Fatalf("decode page_bulk_status_request without hint: %v", err)
	}
	if got := msg.Payload.(*PageBulkStatusRequestPayload); got.RenderedRecordCountHint != nil {
		t.Fatalf("rendered_record_count_hint = %v, want nil when absent", *got.RenderedRecordCountHint)
	}

	statusResult := PageBulkStatusResultPayload{
		RequestID: "request-bulk-0001", ScanID: "scan-bulk-0001",
		Items: []PageBulkStatusItem{
			{LocalID: "row-1", CanonicalKey: "work-key-1", Status: "eligible", OwnershipComplete: false},
			{LocalID: "row-2", CanonicalKey: "work-key-2", Status: "queued", OwnershipComplete: false, JobID: "job_bulk_00001"},
			{LocalID: "row-3", Status: "invalid", OwnershipComplete: false},
		},
		Truncated: true,
	}
	msg, err = DecodeBrowserMessage(frame(MsgPageBulkStatusResult, statusResult))
	if err != nil {
		t.Fatalf("decode page_bulk_status_result: %v", err)
	}
	if got := msg.Payload.(*PageBulkStatusResultPayload); !got.Truncated || len(got.Items) != 3 || got.Items[2].CanonicalKey != "" {
		t.Fatalf("round-trip status result = %#v, want %#v", got, statusResult)
	}

	submitRequest := PageBulkSubmitRequestPayload{
		RequestID: "request-bulk-0002", ScanID: "scan-bulk-0001",
		CanonicalKeys: []string{"work-key-1", "work-key-2"},
		Source:        PageBulkSubmitSource{Kind: "browser_page", Origin: "https://scholar.example.edu", Detector: "generic-identifiers/1"},
	}
	msg, err = DecodeBrowserMessage(frame(MsgPageBulkSubmitRequest, submitRequest))
	if err != nil {
		t.Fatalf("decode page_bulk_submit_request: %v", err)
	}
	got2 := msg.Payload.(*PageBulkSubmitRequestPayload)
	if got2.RequestID != submitRequest.RequestID || got2.ScanID != submitRequest.ScanID ||
		!slices.Equal(got2.CanonicalKeys, submitRequest.CanonicalKeys) || got2.Source != submitRequest.Source {
		t.Fatalf("round-trip submit request = %#v, want %#v", got2, submitRequest)
	}

	submitResult := PageBulkSubmitResultPayload{
		RequestID: "request-bulk-0002", ScanID: "scan-bulk-0001",
		Submitted: 1, Joined: 1, AlreadyOwned: 0, Invalid: 0, BatchID: "batch_bulk_00001",
	}
	msg, err = DecodeBrowserMessage(frame(MsgPageBulkSubmitResult, submitResult))
	if err != nil {
		t.Fatalf("decode page_bulk_submit_result: %v", err)
	}
	if got := msg.Payload.(*PageBulkSubmitResultPayload); *got != submitResult {
		t.Fatalf("round-trip submit result = %#v, want %#v", got, submitResult)
	}

	manyIdentifiers := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"local_id": fmt.Sprintf("row-%d", i), "kind": "doi", "value": fmt.Sprintf("10.1000/example.%d", i)}
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{name: "no identifiers", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001", "identifiers": []any{},
		}},
		{name: "201 identifiers", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001", "identifiers": manyIdentifiers(201),
		}},
		{name: "bad kind", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001",
			"identifiers": []map[string]any{{"local_id": "row-1", "kind": "isbn", "value": "9780000000002"}},
		}},
		{name: "duplicate local_id", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001",
			"identifiers": []map[string]any{
				{"local_id": "row-1", "kind": "doi", "value": "10.1000/a"},
				{"local_id": "row-1", "kind": "doi", "value": "10.1000/b"},
			},
		}},
		{name: "empty value", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001",
			"identifiers": []map[string]any{{"local_id": "row-1", "kind": "doi", "value": ""}},
		}},
		{name: "malformed scan_id", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "short",
			"identifiers": []map[string]any{{"local_id": "row-1", "kind": "doi", "value": "10.1000/a"}},
		}},
		{name: "negative rendered_record_count_hint", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001",
			"identifiers":                []map[string]any{{"local_id": "row-1", "kind": "doi", "value": "10.1000/a"}},
			"rendered_record_count_hint": -1,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeBrowserMessage(frame(MsgPageBulkStatusRequest, tc.payload)); err == nil {
				t.Fatal("page_bulk_status_request was accepted")
			}
		})
	}

	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{name: "canonical_key present for invalid", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001", "truncated": false,
			"items": []map[string]any{{"local_id": "row-1", "canonical_key": "work-key-1", "status": "invalid", "ownership_complete": false}},
		}},
		{name: "canonical_key missing for eligible", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001", "truncated": false,
			"items": []map[string]any{{"local_id": "row-1", "status": "eligible", "ownership_complete": false}},
		}},
		{name: "job_id on non-queued status", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001", "truncated": false,
			"items": []map[string]any{{"local_id": "row-1", "canonical_key": "work-key-1", "status": "eligible", "ownership_complete": false, "job_id": "job_bulk_00001"}},
		}},
		{name: "malformed job_id", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001", "truncated": false,
			"items": []map[string]any{{"local_id": "row-1", "canonical_key": "work-key-1", "status": "queued", "ownership_complete": false, "job_id": "short"}},
		}},
		{name: "unknown status", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001", "truncated": false,
			"items": []map[string]any{{"local_id": "row-1", "canonical_key": "work-key-1", "status": "unexpected", "ownership_complete": false}},
		}},
		{name: "201 items", payload: map[string]any{
			"request_id": "request-bulk-0001", "scan_id": "scan-bulk-0001", "truncated": false,
			"items": func() []map[string]any {
				out := make([]map[string]any, 201)
				for i := range out {
					out[i] = map[string]any{"local_id": fmt.Sprintf("row-%d", i), "canonical_key": "wk", "status": "eligible", "ownership_complete": false}
				}
				return out
			}(),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeBrowserMessage(frame(MsgPageBulkStatusResult, tc.payload)); err == nil {
				t.Fatal("page_bulk_status_result was accepted")
			}
		})
	}

	manyKeys := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("work-key-%d", i)
		}
		return out
	}
	validSource := map[string]any{"kind": "browser_page", "origin": "https://scholar.example.edu", "detector": "generic-identifiers/1"}
	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{name: "no keys", payload: map[string]any{
			"request_id": "request-bulk-0002", "scan_id": "scan-bulk-0001", "canonical_keys": []string{}, "source": validSource,
		}},
		{name: "51 keys", payload: map[string]any{
			"request_id": "request-bulk-0002", "scan_id": "scan-bulk-0001", "canonical_keys": manyKeys(51), "source": validSource,
		}},
		{name: "duplicate key", payload: map[string]any{
			"request_id": "request-bulk-0002", "scan_id": "scan-bulk-0001",
			"canonical_keys": []string{"work-key-1", "work-key-1"}, "source": validSource,
		}},
		{name: "origin with path", payload: map[string]any{
			"request_id": "request-bulk-0002", "scan_id": "scan-bulk-0001", "canonical_keys": []string{"work-key-1"},
			"source": map[string]any{"kind": "browser_page", "origin": "https://scholar.example.edu/path", "detector": "generic-identifiers/1"},
		}},
		{name: "origin with query", payload: map[string]any{
			"request_id": "request-bulk-0002", "scan_id": "scan-bulk-0001", "canonical_keys": []string{"work-key-1"},
			"source": map[string]any{"kind": "browser_page", "origin": "https://scholar.example.edu?x=1", "detector": "generic-identifiers/1"},
		}},
		{name: "origin with uppercase host", payload: map[string]any{
			"request_id": "request-bulk-0002", "scan_id": "scan-bulk-0001", "canonical_keys": []string{"work-key-1"},
			"source": map[string]any{"kind": "browser_page", "origin": "https://Scholar.Example.EDU", "detector": "generic-identifiers/1"},
		}},
		{name: "non-https origin", payload: map[string]any{
			"request_id": "request-bulk-0002", "scan_id": "scan-bulk-0001", "canonical_keys": []string{"work-key-1"},
			"source": map[string]any{"kind": "browser_page", "origin": "http://scholar.example.edu", "detector": "generic-identifiers/1"},
		}},
		{name: "empty detector", payload: map[string]any{
			"request_id": "request-bulk-0002", "scan_id": "scan-bulk-0001", "canonical_keys": []string{"work-key-1"},
			"source": map[string]any{"kind": "browser_page", "origin": "https://scholar.example.edu", "detector": ""},
		}},
		{name: "wrong source kind", payload: map[string]any{
			"request_id": "request-bulk-0002", "scan_id": "scan-bulk-0001", "canonical_keys": []string{"work-key-1"},
			"source": map[string]any{"kind": "extension", "origin": "https://scholar.example.edu", "detector": "generic-identifiers/1"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeBrowserMessage(frame(MsgPageBulkSubmitRequest, tc.payload)); err == nil {
				t.Fatal("page_bulk_submit_request was accepted")
			}
		})
	}

	for _, payload := range []map[string]any{
		{"request_id": "request-bulk-0003", "scan_id": "scan-bulk-0001", "submitted": -1, "joined": 0, "already_owned": 0, "invalid": 0, "batch_id": "batch_bulk_00001"},
		{"request_id": "request-bulk-0003", "scan_id": "scan-bulk-0001", "submitted": 0, "joined": 0, "already_owned": 0, "invalid": 0, "batch_id": "short"},
		{"request_id": "request-bulk-0003", "scan_id": "scan-bulk-0001", "submitted": 0, "joined": 0, "already_owned": 0, "invalid": 0},
	} {
		if _, err := DecodeBrowserMessage(frame(MsgPageBulkSubmitResult, payload)); err == nil {
			t.Fatalf("page_bulk_submit_result payload %#v was accepted", payload)
		}
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

func TestTriageDeliveryOfferedRoundTrips(t *testing.T) {
	payload := TriageSnapshotResponsePayload{
		RequestID: "request-offered-001", Schema: 3, GeneratedAt: "2026-01-01T00:00:00Z",
		Counts: TriageCounts{PendingTotal: 1, Actions: 1},
		Items: []TriageSnapshotItem{{
			Kind: "human_action", ID: "action_offered_001", Rank: 1, Title: "delivery",
			Facts: []TriageFact{}, Links: []TriageLink{},
			ActionID: 1, JobID: "job_offered_001", ActionKind: "document_delivery",
			JobState: "awaiting_human", Revision: 1, RouteClass: "document_delivery",
			AuthRequirement: "unknown", Attention: "required",
			Ops:      []string{"open_request_history", "confirm_request_exists", "confirm_request_absent"},
			Delivery: &TriageDelivery{Provider: "provider", State: "offered"},
		}},
		HasMore: false, UnsupportedItemsCount: 0,
	}
	frame := map[string]any{
		"protocol": BrowserProtocolVersion, "type": MsgTriageSnapshotResponse,
		"msg_id": "offered-roundtrip", "seq": 1, "payload": payload,
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatalf("offered delivery rejected: %v", err)
	}
	roundTripped, err := json.Marshal(map[string]any{
		"protocol": BrowserProtocolVersion, "type": MsgTriageSnapshotResponse,
		"msg_id": "offered-roundtrip-2", "seq": 2, "payload": message.Payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(roundTripped); err != nil {
		t.Fatalf("offered delivery failed round-trip validation: %v", err)
	}
}

func TestTriageSnapshotV3RequiresAttentionAndRouteFields(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), "browser-triage-snapshot-response-v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	loadFrame := func() map[string]any {
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatal(err)
		}
		return frame
	}

	// Missing attention on any v3 item is rejected.
	frame := loadFrame()
	items := frame["payload"].(map[string]any)["items"].([]any)
	delete(items[0].(map[string]any), "attention")
	mutated, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(mutated); err == nil || !strings.Contains(err.Error(), "attention") {
		t.Fatalf("v3 item missing attention err = %v, want rejection", err)
	}

	// Missing route_class on a v3 human_action item is rejected.
	frame = loadFrame()
	items = frame["payload"].(map[string]any)["items"].([]any)
	delete(items[1].(map[string]any), "route_class")
	mutated, err = json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(mutated); err == nil || !strings.Contains(err.Error(), "route_class") {
		t.Fatalf("v3 human_action missing route_class err = %v, want rejection", err)
	}

	// Missing auth_requirement on a v3 human_action item is rejected.
	frame = loadFrame()
	items = frame["payload"].(map[string]any)["items"].([]any)
	delete(items[1].(map[string]any), "auth_requirement")
	mutated, err = json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(mutated); err == nil || !strings.Contains(err.Error(), "auth_requirement") {
		t.Fatalf("v3 human_action missing auth_requirement err = %v, want rejection", err)
	}

	// The valid fixture itself decodes and the document_delivery item's
	// delivery sub-object round-trips.
	message, err := DecodeBrowserMessage(data)
	if err != nil {
		t.Fatalf("valid v3 fixture rejected: %v", err)
	}
	payload := message.Payload.(*TriageSnapshotResponsePayload)
	var deliveryItem *TriageSnapshotItem
	for i := range payload.Items {
		if payload.Items[i].ActionKind == "document_delivery" {
			deliveryItem = &payload.Items[i]
		}
	}
	if deliveryItem == nil || deliveryItem.Delivery == nil || deliveryItem.Delivery.Provider != "illiad" ||
		deliveryItem.Delivery.State != "unknown_outcome" || deliveryItem.AuthRequirement != "unknown" {
		t.Fatalf("document_delivery item = %+v, want delivery+auth_requirement populated", deliveryItem)
	}
}

func TestTriageSnapshotDeliveryGatedToDocumentDelivery(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), "browser-triage-snapshot-response-v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	items := frame["payload"].(map[string]any)["items"].([]any)
	verifyIdentityItem := items[1].(map[string]any)
	verifyIdentityItem["delivery"] = map[string]any{"provider": "illiad", "state": "pending"}
	mutated, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(mutated); err == nil || !strings.Contains(err.Error(), "delivery") {
		t.Fatalf("delivery on a non-document_delivery item err = %v, want rejection", err)
	}
}

func TestTriageSnapshotSchema2RejectsV3BlockedByValues(t *testing.T) {
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
	items[1].(map[string]any)["blocked_by"] = "login"
	mutated, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(mutated); err == nil || !strings.Contains(err.Error(), "blocked_by") {
		t.Fatalf("schema-2 frame with a v3-only blocked_by value err = %v, want rejection", err)
	}
}

func TestDeliveryReconcilePayloadRoundTripsAndValidates(t *testing.T) {
	frame := func(payload any) []byte {
		t.Helper()
		data, err := json.Marshal(map[string]any{
			"protocol": BrowserProtocolVersion, "type": MsgDeliveryReconcileRequest,
			"msg_id": "delivery-reconcile-0001", "seq": 1, "payload": payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	valid := DeliveryReconcilePayload{
		RequestID: "request-delivery-0001", JobID: "job_delivery_0001",
		Operation: "confirm_request_exists", ProviderReference: "TN-42",
	}
	msg, err := DecodeBrowserMessage(frame(valid))
	if err != nil {
		t.Fatalf("valid delivery_reconcile_request rejected: %v", err)
	}
	got := msg.Payload.(*DeliveryReconcilePayload)
	if *got != valid {
		t.Fatalf("round-trip = %#v, want %#v", got, valid)
	}

	absent := DeliveryReconcilePayload{
		RequestID: "request-delivery-0002", JobID: "job_delivery_0001", Operation: "confirm_request_absent",
	}
	if _, err := DecodeBrowserMessage(frame(absent)); err != nil {
		t.Fatalf("valid confirm_request_absent rejected: %v", err)
	}

	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{name: "confirm_request_exists without provider_reference", payload: map[string]any{
			"request_id": "request-delivery-0003", "job_id": "job_delivery_0001", "operation": "confirm_request_exists",
		}},
		{name: "confirm_request_absent with provider_reference", payload: map[string]any{
			"request_id": "request-delivery-0004", "job_id": "job_delivery_0001", "operation": "confirm_request_absent", "provider_reference": "TN-42",
		}},
		// Presence, not value: an explicit "" must be rejected exactly like a
		// non-empty value — p.ProviderReference == "" alone cannot tell
		// "absent" from "present and empty" apart, so this pins the
		// raw-field presence check in the delivery_reconcile_request decode
		// case, matching extension/src/protocol.ts and the JSON schema's
		// "not": {"required": ["provider_reference"]}.
		{name: "confirm_request_absent with empty-string provider_reference", payload: map[string]any{
			"request_id": "request-delivery-0006", "job_id": "job_delivery_0001", "operation": "confirm_request_absent", "provider_reference": "",
		}},
		{name: "unknown operation", payload: map[string]any{
			"request_id": "request-delivery-0005", "job_id": "job_delivery_0001", "operation": "open_request_history",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeBrowserMessage(frame(tc.payload)); err == nil {
				t.Fatal("invalid delivery_reconcile_request was accepted")
			}
		})
	}

	result := DeliveryReconcileResultPayload{RequestID: "request-delivery-0001", Outcome: "applied"}
	resultFrame, err := json.Marshal(map[string]any{
		"protocol": BrowserProtocolVersion, "type": MsgDeliveryReconcileResult,
		"msg_id": "delivery-reconcile-0002", "seq": 2, "payload": result,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(resultFrame); err != nil {
		t.Fatalf("valid delivery_reconcile_result rejected: %v", err)
	}
}

func TestHandoffLinkRejectsInvalidWireShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     string
		payload map[string]any
	}{
		{
			name: "request",
			typ:  MsgHandoffLinkRequest,
			payload: map[string]any{
				"request_id": "", "job_id": "job_handoff_0001",
			},
		},
		{
			name: "request with noncanonical request id",
			typ:  MsgHandoffLinkRequest,
			payload: map[string]any{
				"Request_ID": "handoff-request-001", "job_id": "job_handoff_0001",
			},
		},
		{
			name: "request with case-colliding job id",
			typ:  MsgHandoffLinkRequest,
			payload: map[string]any{
				"job_id": "job_handoff_0001", "Job_ID": "job_handoff_0002",
			},
		},
		{
			name: "result",
			typ:  MsgHandoffLinkResult,
			payload: map[string]any{
				"request_id": "", "outcome": "unavailable", "detail": "not available",
			},
		},
		{
			name: "result with noncanonical request id",
			typ:  MsgHandoffLinkResult,
			payload: map[string]any{
				"Request_ID": "handoff-request-001", "outcome": "unavailable", "detail": "not available",
			},
		},
		{
			name: "result with case-colliding outcome",
			typ:  MsgHandoffLinkResult,
			payload: map[string]any{
				"outcome": "unavailable", "Outcome": "opened", "detail": "not available",
			},
		},
		{
			name: "result with noncanonical URL",
			typ:  MsgHandoffLinkResult,
			payload: map[string]any{
				"request_id": "handoff-request-001", "outcome": "opened",
				"URL": "https://resolver.example.edu/openurl",
			},
		},
		{
			name: "result with noncanonical detail",
			typ:  MsgHandoffLinkResult,
			payload: map[string]any{
				"request_id": "handoff-request-001", "outcome": "unavailable",
				"Detail": "not available",
			},
		},
		{
			name: "result with raw space in URL",
			typ:  MsgHandoffLinkResult,
			payload: map[string]any{
				"request_id": "handoff-request-001", "outcome": "opened",
				"url": "https://resolver.example.edu/openurl?title=A Paper",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := json.Marshal(map[string]any{
				"protocol": BrowserProtocolVersion,
				"type":     tc.typ,
				"msg_id":   "handoff-empty-request-id",
				"seq":      1,
				"payload":  tc.payload,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeBrowserMessage(frame); err == nil {
				t.Fatal("invalid handoff link frame was accepted")
			}
		})
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

// TestDownloadIDRejectsZero pins the fail-closed floor introduced for
// download_id. Delivery provenance correlates on
// browserDownloadKey{JobID, DownloadID} (internal/browser/bridge.go): two
// downloads reported as download_id 0 for the same job collide on that key,
// so a download_complete for the second overwrites the pending entry's
// CandidateID from the first, and a delivery_context meant for the first
// download then applies its access_basis to the second, unrelated candidate —
// the mis-binding class delivery provenance exists to prevent, reached
// through the correlation key instead of the candidate id. This is
// fail-closed hardening rather than a live-bug fix: chrome.downloads
// allocates ids starting at 1 and increasing, so no genuine extension ever
// sends 0, and the floor must therefore still accept 1.
func TestDownloadIDRejectsZero(t *testing.T) {
	const downloadStarted = `{"protocol":"papio-browser/1","type":"download_started","msg_id":"m_dls_id_%d","job_id":"job_dls_id_case","seq":1,"payload":{"download_id":%d,"filename":"paper.pdf"}}`
	const downloadComplete = `{"protocol":"papio-browser/1","type":"download_complete","msg_id":"m_dlc_id_%d","job_id":"job_dlc_id_case","seq":1,"payload":{"download_id":%d,"filename":"paper.pdf","size_bytes":100}}`
	const deliveryContext = `{"protocol":"papio-browser/1","type":"delivery_context","msg_id":"m_dctx_id_%d","job_id":"job_dctx_id_case","seq":1,"payload":{"download_id":%d,"route":"direct","session_evidence":"none"}}`

	for _, tmpl := range []string{downloadStarted, downloadComplete, deliveryContext} {
		if _, err := DecodeBrowserMessage([]byte(fmt.Sprintf(tmpl, 1, 0))); err == nil {
			t.Fatalf("download_id 0 accepted: %s", tmpl)
		}
		if _, err := DecodeBrowserMessage([]byte(fmt.Sprintf(tmpl, 2, -1))); err == nil {
			t.Fatalf("download_id -1 accepted: %s", tmpl)
		}
		if _, err := DecodeBrowserMessage([]byte(fmt.Sprintf(tmpl, 3, 1))); err != nil {
			t.Fatalf("download_id 1 rejected: %s: %v", tmpl, err)
		}
	}
}

// TestPageHostSchemaAndValidatorAgree pins papio-a82ab8e6906fda25: the
// published `^[a-z0-9.-]{3,128}$` pattern alone silently admitted ".abc",
// "abc.", and "a..b" — shapes DeliveryContextPayload.validate already
// rejected explicitly, so the schema documented a contract neither
// executable parser honoured. The schema now adds a "not" clause encoding
// the same three rejections; this test keeps that clause and the Go
// validator in lockstep by re-deriving each side's accept/reject decision
// independently and comparing them, the same shape as
// TestBareRouteIsNeverLaxerThanThePublishedSchema above.
func TestPageHostSchemaAndValidatorAgree(t *testing.T) {
	schemaPattern := regexp.MustCompile(`^[a-z0-9.-]{3,128}$`)
	schemaNot := regexp.MustCompile(`(^\.)|(\.$)|(\.\.)`)
	for _, host := range []string{
		"publisher.example.edu",
		"a.b",
		".abc",
		"abc.",
		"a..b",
		".",
		"..",
	} {
		goOK := browserTextLen(host) <= 128 && hostRE.MatchString(host) &&
			!strings.Contains(host, "..") && !strings.HasPrefix(host, ".") && !strings.HasSuffix(host, ".")
		schemaOK := schemaPattern.MatchString(host) && !schemaNot.MatchString(host)
		if goOK != schemaOK {
			t.Errorf("page_host %q: Go accept=%v schema accept=%v, want equal", host, goOK, schemaOK)
		}
	}
}

// TestPageHostRejectsDotEdgeCases decodes real delivery_context frames for
// the three shapes above, pinning the corpus fixtures under
// testdata/protocol/invalid/browser-delivery-context-page-host-*.json
// through the actual decode path rather than a hand-rolled reimplementation
// of validate().
func TestPageHostRejectsDotEdgeCases(t *testing.T) {
	const frame = `{"protocol":"papio-browser/1","type":"delivery_context","msg_id":"m_dctx_host_case","job_id":"job_dctx_host_case","seq":1,"payload":{"download_id":1,"route":"direct","session_evidence":"none","page_host":"%s"}}`
	for _, host := range []string{".abc", "abc.", "a..b"} {
		if _, err := DecodeBrowserMessage([]byte(fmt.Sprintf(frame, host))); err == nil {
			t.Errorf("page_host %q accepted; want rejected", host)
		}
	}
	if _, err := DecodeBrowserMessage([]byte(fmt.Sprintf(frame, "publisher.example.edu"))); err != nil {
		t.Errorf("page_host %q rejected: %v", "publisher.example.edu", err)
	}
}

// TestOriginHintSchemaAndValidatorAgree keeps validateResolverOriginHint at
// least as strict as the published schema pattern below, the same
// one-directional shape TestBareRouteIsNeverLaxerThanThePublishedSchema
// uses for candidate.route: it fails only when Go accepts a value the
// schema pattern rejects, never the reverse. Full bidirectional parity is
// not asserted because it does not hold, and closing the remaining gap
// would mean duplicating the WHATWG URL parser's reparsing rules in Go —
// see validateResolverOriginHint's doc comment for the accepted-gap list:
// a purely numeric single-label host (reparsed as IPv4 by the WHATWG
// parser) and a zero-padded port both pass Go and this schema pattern but
// fail the TypeScript round-trip check, and neither is reachable from a
// genuine producer. A third gap this test cannot observe at all:
// SessionEvidencePayload.validate treats an explicit empty origin_hint as
// an omitted optional field and never calls this function, so an empty
// string decodes in Go a layer above where this pattern (and TypeScript)
// reject it.
//
// "https://123" (a bare numeric host — the multi-label requirement that
// used to exclude it was dropped as a release-blocker fix; see
// TestOriginHintAcceptsLegitimateHosts) and the port cases
// ("https://library:123456" over-length, "https://library:" a bare
// trailing colon with no digits, "https://library:08443" zero-padded, and
// "https://library:443" ordinary) are included below because they are
// exactly the shapes that relaxation and a Hostname()-only port check
// could diverge on. validateResolverOriginHint now validates the port
// explicitly, so Go must never be laxer than this pattern for any of them.
func TestOriginHintSchemaAndValidatorAgree(t *testing.T) {
	schemaPattern := regexp.MustCompile(`^https://[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[0-9]{1,5})?$`)
	for _, hint := range []string{
		"https://resolver.example.edu",
		"https://resolver.example.edu:8443",
		"https://a.b",
		"https://EXAMPLE.com",
		"https://a",
		"https://library",
		"https://123",
		"https://localhost",
		"https://localhost:8443",
		"https://127.0.0.1",
		"https://127.0.0.1:8443",
		"https://library:443",
		"https://library:08443",
		"https://library:123456",
		"https://library:",
		"https://[::1]",
		"https://[::1]:8443",
		"HTTPS://resolver.example.edu",
		"https://resolver.example.edu/path",
		"https://resolver.example.edu?x=1",
		"https://user@resolver.example.edu",
		"https://.abc.example.edu",
		"https://abc.example.edu.",
		"https://abc..example.edu",
	} {
		goOK := validateResolverOriginHint(hint) == nil
		schemaOK := schemaPattern.MatchString(hint) && utf8.RuneCountInString(hint) <= 300
		if goOK && !schemaOK {
			t.Errorf("origin_hint %q: Go accepts what the published schema rejects", hint)
		}
	}
}

// TestOriginHintAcceptsLegitimateHosts is the release-blocker regression
// test: a single-label intranet resolver, localhost (with and without a
// port), and an IPv4 literal are all values a valid papio config
// (internal/config/config.go's validateOpenURLBase requires only an https
// scheme and a non-empty host) can produce, so the wire validator must
// accept every one of them. It previously rejected the whole
// session_evidence frame outbound (Bridge.send self-validates and drops
// silently) and fatally on decode inbound under version skew — see
// validateResolverOriginHint's doc comment for the full incident.
func TestOriginHintAcceptsLegitimateHosts(t *testing.T) {
	for _, hint := range []string{
		"https://library",
		"https://localhost",
		"https://localhost:8443",
		"https://127.0.0.1",
	} {
		if err := validateResolverOriginHint(hint); err != nil {
			t.Errorf("origin_hint %q rejected: %v", hint, err)
		}
	}
}

// TestOriginHintRejectsUppercaseHost decodes a real session_evidence frame
// for the mixed-case disagreement value named in papio-26fa531528e29798,
// pinning the corpus fixture under
// testdata/protocol/invalid/browser-session-evidence-origin-hint-uppercase-host.json
// through the actual decode path. The single-label counterpart of this test
// moved to testdata/protocol/valid: a single-label host is a legitimate
// origin_hint value now, not an invalid one — see
// TestOriginHintAcceptsLegitimateHosts.
func TestOriginHintRejectsUppercaseHost(t *testing.T) {
	const frame = `{"protocol":"papio-browser/1","type":"session_evidence","msg_id":"m_origin_case","seq":1,"payload":{"evidence":"warm_verified","origin_hint":"%s","at":"2026-08-03T12:00:00Z"}}`
	if _, err := DecodeBrowserMessage([]byte(fmt.Sprintf(frame, "https://EXAMPLE.com"))); err == nil {
		t.Errorf("origin_hint %q accepted; want rejected", "https://EXAMPLE.com")
	}
	if _, err := DecodeBrowserMessage([]byte(fmt.Sprintf(frame, "https://resolver.example.edu"))); err != nil {
		t.Errorf("origin_hint %q rejected: %v", "https://resolver.example.edu", err)
	}
}

func TestProviderDirectGetTupleValidation(t *testing.T) {
	base := BrowserMessage{
		Protocol: BrowserProtocolVersion,
		Type:     MsgProviderDirectGetRequest,
		MsgID:    "msg-direct-1",
		JobID:    "job-direct-1",
		Seq:      1,
		Payload: ProviderDirectGetRequestPayload{
			DriveAttemptID:     "attempt-0001",
			Ordinal:            0,
			RouteRevision:      "sage-doi-pdf/1",
			ExpectedIdentifier: "doi:10.1234/example",
			URL:                "https://journals.sagepub.com/doi/pdf/10.1234/example?download=true",
			AllowedOrigin:      "https://journals.sagepub.com",
			PathFamily:         "/doi/pdf/{doi}",
			TermsPolicy:        "none",
		},
	}
	wire, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(wire); err != nil {
		t.Fatalf("valid direct request rejected: %v", err)
	}
	base.Payload = ProviderDirectGetRequestPayload{
		DriveAttemptID:     "attempt-0001",
		Ordinal:            0,
		RouteRevision:      "sage-doi-pdf/1",
		ExpectedIdentifier: "doi:10.1234/example",
		URL:                "https://journals.sagepub.com/doi/pdf/10.1234/example?token=secret",
		AllowedOrigin:      "https://journals.sagepub.com",
		PathFamily:         "/doi/pdf/{doi}",
		TermsPolicy:        "none",
	}
	wire, err = json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(wire); err == nil {
		t.Fatal("secret-bearing direct URL accepted")
	}
}
func TestInstitutionalMaterializationPayloadsRoundTripAndDisposition(t *testing.T) {
	generation := int64(0)
	frames := []struct {
		name, typ, jobID string
		payload          any
	}{
		{"claim", MsgInstitutionalClaimRequest, "job_inst_001", InstitutionalClaimRequestPayload{RequestID: "req_inst_001", CandidateID: "cand_001", MaterializationKind: "browser_tab"}},
		{"claim_response", MsgInstitutionalClaimResponse, "job_inst_001", InstitutionalClaimResponsePayload{RequestID: "req_inst_001", Outcome: "claimed", CandidateID: "cand_001", ClaimID: "claim_001", BindingID: "bind_001", BrowserHolderGeneration: &generation, LeaseUntil: "2026-08-11T12:00:00Z"}},
		{"bind", MsgInstitutionalBindRequest, "job_inst_001", InstitutionalBindRequestPayload{RequestID: "req_inst_001", ClaimID: "claim_001", BindingID: "bind_001", TabID: 7}},
		{"route", MsgInstitutionalRouteResponse, "job_inst_001", InstitutionalRouteResponsePayload{RequestID: "req_inst_001", Outcome: "issued", ClaimID: "claim_001", BindingID: "bind_001", RouteIssuanceOrdinal: 1, URL: "https://resolver.example/route"}},
		{"navigated", MsgInstitutionalNavigatedRequest, "job_inst_001", InstitutionalNavigatedRequestPayload{RequestID: "req_inst_001", ClaimID: "claim_001", BindingID: "bind_001", RouteIssuanceOrdinal: 1, TabID: 7}},
		{"reconcile", MsgInstitutionalReconcileRequest, "", InstitutionalReconcileRequestPayload{RequestID: "req_inst_001", Bindings: []InstitutionalReconcileBinding{{BindingID: "bind_001", TabID: 7}}}},
	}
	for _, tc := range frames {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]any{"protocol": BrowserProtocolVersion, "type": tc.typ, "msg_id": "msg_inst_001", "seq": 1, "payload": tc.payload}
			if tc.jobID != "" {
				env["job_id"] = tc.jobID
			}
			raw, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeBrowserMessage(raw); err != nil {
				t.Fatalf("round trip: %v", err)
			}
		})
	}
	disabled := InstitutionalRouteResponsePayload{RequestID: "req_inst_002", Outcome: "feature_disabled", Detail: "dark"}
	raw, _ := json.Marshal(map[string]any{"protocol": BrowserProtocolVersion, "type": MsgInstitutionalRouteResponse, "msg_id": "msg_inst_002", "seq": 2, "job_id": "job_inst_001", "payload": disabled})
	if _, err := DecodeBrowserMessage(raw); err != nil {
		t.Fatalf("disabled response: %v", err)
	}
	for _, bad := range []map[string]any{
		{"outcome": "issued", "claim_id": "claim_001", "binding_id": "bind_001"},
		{"outcome": "feature_disabled", "url": "https://resolver.example/route"},
	} {
		raw, _ := json.Marshal(map[string]any{"protocol": BrowserProtocolVersion, "type": MsgInstitutionalRouteResponse, "msg_id": "msg_inst_003", "seq": 3, "job_id": "job_inst_001", "payload": bad})
		if _, err := DecodeBrowserMessage(raw); err == nil {
			t.Fatalf("invalid route response accepted: %#v", bad)
		}
	}
}

func TestInstitutionalMaterializationBoundsAndScope(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"protocol": BrowserProtocolVersion, "type": MsgInstitutionalClaimRequest, "msg_id": "msg_inst_004", "seq": 4, "payload": map[string]any{"candidate_id": "cand_001", "materialization_kind": "browser_tab"}})
	if _, err := DecodeBrowserMessage(raw); err == nil {
		t.Fatal("job-scoped institutional request without job_id accepted")
	}
	raw, _ = json.Marshal(map[string]any{"protocol": BrowserProtocolVersion, "type": MsgInstitutionalReconcileRequest, "msg_id": "msg_inst_005", "seq": 5, "payload": map[string]any{"bindings": make([]any, 33)}})
	if _, err := DecodeBrowserMessage(raw); err == nil {
		t.Fatal("oversized reconcile bindings accepted")
	}
	raw, _ = json.Marshal(map[string]any{"protocol": BrowserProtocolVersion, "type": MsgInstitutionalBindRequest, "msg_id": "msg_inst_006", "seq": 6, "job_id": "job_inst_001", "payload": map[string]any{"claim_id": "claim_001", "binding_id": "bind_001", "tab_id": MaxBrowserInteger + 1}})
	if _, err := DecodeBrowserMessage(raw); err == nil {
		t.Fatal("oversized tab id accepted")
	}
	raw, _ = json.Marshal(map[string]any{"protocol": BrowserProtocolVersion, "type": MsgInstitutionalBindRequest, "msg_id": "msg_inst_008", "seq": 8, "job_id": "job_inst_001", "payload": map[string]any{"claim_id": "claim_001", "binding_id": "bind_001", "tab_id": -1}})
	if _, err := DecodeBrowserMessage(raw); err == nil {
		t.Fatal("negative tab id accepted")
	}
	raw, _ = json.Marshal(map[string]any{"protocol": BrowserProtocolVersion, "type": MsgInstitutionalNavigatedRequest, "msg_id": "msg_inst_009", "seq": 9, "job_id": "job_inst_001", "payload": map[string]any{"claim_id": "claim_001", "binding_id": "bind_001", "route_issuance_ordinal": 1, "tab_id": -1}})
	if _, err := DecodeBrowserMessage(raw); err == nil {
		t.Fatal("negative navigated tab id accepted")
	}
	raw, _ = json.Marshal(map[string]any{"protocol": BrowserProtocolVersion, "type": MsgInstitutionalReconcileRequest, "msg_id": "msg_inst_010", "seq": 10, "payload": map[string]any{"bindings": []any{map[string]any{"binding_id": "bind_001", "tab_id": -1}}}})
	if _, err := DecodeBrowserMessage(raw); err == nil {
		t.Fatal("negative reconcile tab id accepted")
	}
	raw, _ = json.Marshal(map[string]any{"protocol": BrowserProtocolVersion, "type": MsgInstitutionalReconcileRequest, "msg_id": "msg_inst_007", "seq": 7, "job_id": "job_inst_001", "payload": map[string]any{"bindings": []any{}}})
	if _, err := DecodeBrowserMessage(raw); err == nil {
		t.Fatal("session-scoped reconcile with job_id accepted")
	}
}

func TestInstitutionalMaterializationClosedOutcomesAndOpaqueIDs(t *testing.T) {
	frame := func(typ, jobID string, payload map[string]any) []byte {
		t.Helper()
		env := map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     typ,
			"msg_id":   "msg_inst_011",
			"seq":      11,
			"payload":  payload,
		}
		if jobID != "" {
			env["job_id"] = jobID
		}
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	t.Run("cross-family outcomes are rejected", func(t *testing.T) {
		cases := []struct {
			name, typ, jobID, outcome string
		}{
			{"claim rejects issued", MsgInstitutionalClaimResponse, "job_inst_001", "issued"},
			{"bind rejects claimed", MsgInstitutionalBindResponse, "job_inst_001", "claimed"},
			{"route rejects acknowledged", MsgInstitutionalRouteResponse, "job_inst_001", "acknowledged"},
			{"navigated rejects bound", MsgInstitutionalNavigatedResponse, "job_inst_001", "bound"},
			{"reconcile rejects busy", MsgInstitutionalReconcileResponse, "", "busy"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := DecodeBrowserMessage(frame(tc.typ, tc.jobID, map[string]any{"outcome": tc.outcome})); err == nil {
					t.Fatalf("accepted outcome %q for %s", tc.outcome, tc.typ)
				}
			})
		}
	})

	t.Run("opaque IDs require at least eight characters", func(t *testing.T) {
		if _, err := DecodeBrowserMessage(frame(MsgInstitutionalClaimRequest, "job_inst_001", map[string]any{
			"candidate_id": "short", "materialization_kind": "browser_tab",
		})); err == nil {
			t.Fatal("short candidate_id accepted")
		}
		if _, err := DecodeBrowserMessage(frame(MsgInstitutionalBindRequest, "job_inst_001", map[string]any{
			"claim_id": "short", "binding_id": "binding_01", "tab_id": 1,
		})); err == nil {
			t.Fatal("short claim_id accepted")
		}
	})

	t.Run("success outcomes forbid detail", func(t *testing.T) {
		successes := []struct {
			name, typ, jobID string
			payload          map[string]any
		}{
			{"claim", MsgInstitutionalClaimResponse, "job_inst_001", map[string]any{
				"outcome": "claimed", "candidate_id": "cand_001", "claim_id": "claim_001",
				"binding_id": "bind_001", "browser_holder_generation": 0,
				"lease_until": "2026-08-11T12:00:00Z", "detail": "forbidden",
			}},
			{"bind", MsgInstitutionalBindResponse, "job_inst_001", map[string]any{
				"outcome": "bound", "claim_id": "claim_001", "binding_id": "bind_001", "detail": "forbidden",
			}},
			{"route", MsgInstitutionalRouteResponse, "job_inst_001", map[string]any{
				"outcome": "issued", "claim_id": "claim_001", "binding_id": "bind_001",
				"route_issuance_ordinal": 1, "url": "https://resolver.example/route", "detail": "forbidden",
			}},
			{"navigated", MsgInstitutionalNavigatedResponse, "job_inst_001", map[string]any{
				"outcome": "acknowledged", "claim_id": "claim_001", "binding_id": "bind_001", "detail": "forbidden",
			}},
			{"reconcile", MsgInstitutionalReconcileResponse, "", map[string]any{
				"outcome": "reconciled", "detail": "forbidden",
			}},
		}
		for _, tc := range successes {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := DecodeBrowserMessage(frame(tc.typ, tc.jobID, tc.payload)); err == nil {
					t.Fatalf("%s success with detail accepted", tc.name)
				}
			})
		}
	})
}

func TestInstitutionalCandidateOfferRoundTripScopeAndBounds(t *testing.T) {
	frame := func(jobID string, payload map[string]any) []byte {
		t.Helper()
		raw, err := json.Marshal(map[string]any{
			"protocol": BrowserProtocolVersion,
			"type":     MsgInstitutionalCandidateOffer,
			"msg_id":   "candidate-offer-001",
			"seq":      1,
			"job_id":   jobID,
			"payload":  payload,
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	valid := map[string]any{
		"candidate_id":         "candidate-001",
		"materialization_kind": "browser_tab",
		"expires_at":           "2026-08-11T12:00:00Z",
	}
	msg, err := DecodeBrowserMessage(frame("job-inst-001", valid))
	if err != nil {
		t.Fatalf("valid candidate offer rejected: %v", err)
	}
	payload, ok := msg.Payload.(*InstitutionalCandidateOfferPayload)
	if !ok || payload.CandidateID != "candidate-001" || payload.MaterializationKind != "browser_tab" ||
		payload.ExpiresAt != "2026-08-11T12:00:00Z" {
		t.Fatalf("decoded payload = %#v", msg.Payload)
	}
	roundTrip, err := json.Marshal(map[string]any{
		"protocol": BrowserProtocolVersion, "type": MsgInstitutionalCandidateOffer,
		"msg_id": "candidate-offer-002", "seq": 2, "job_id": "job-inst-001", "payload": payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBrowserMessage(roundTrip); err != nil {
		t.Fatalf("round-trip rejected: %v", err)
	}

	t.Run("missing job scope", func(t *testing.T) {
		if _, err := DecodeBrowserMessage(frame("", valid)); err == nil {
			t.Fatal("candidate offer without job_id accepted")
		}
	})
	t.Run("unknown URL field", func(t *testing.T) {
		withURL := map[string]any{"candidate_id": "candidate-001", "materialization_kind": "browser_tab", "expires_at": valid["expires_at"], "url": "https://resolver.example"}
		if _, err := DecodeBrowserMessage(frame("job-inst-001", withURL)); err == nil {
			t.Fatal("candidate offer carrying URL accepted")
		}
	})
	t.Run("invalid kind", func(t *testing.T) {
		invalid := map[string]any{"candidate_id": "candidate-001", "materialization_kind": "direct_download", "expires_at": valid["expires_at"]}
		if _, err := DecodeBrowserMessage(frame("job-inst-001", invalid)); err == nil {
			t.Fatal("direct_download candidate offer accepted")
		}
	})
	t.Run("invalid expiry", func(t *testing.T) {
		invalid := map[string]any{"candidate_id": "candidate-001", "materialization_kind": "browser_tab", "expires_at": "not-a-time"}
		if _, err := DecodeBrowserMessage(frame("job-inst-001", invalid)); err == nil {
			t.Fatal("invalid expiry accepted")
		}
	})
	t.Run("candidate ID bounds", func(t *testing.T) {
		for _, candidateID := range []string{"short", strings.Repeat("x", 129)} {
			invalid := map[string]any{"candidate_id": candidateID, "materialization_kind": "browser_tab", "expires_at": valid["expires_at"]}
			if _, err := DecodeBrowserMessage(frame("job-inst-001", invalid)); err == nil {
				t.Fatalf("candidate ID length %d accepted", len(candidateID))
			}
		}
	})
	t.Run("frame size cap", func(t *testing.T) {
		oversized := frame("job-inst-001", map[string]any{
			"candidate_id": "candidate-001", "materialization_kind": "browser_tab",
			"expires_at": valid["expires_at"], "extra": strings.Repeat("x", MaxBrowserMessageBytes),
		})
		if _, err := DecodeBrowserMessage(oversized); err == nil {
			t.Fatal("oversized candidate offer accepted")
		}
	})
}
