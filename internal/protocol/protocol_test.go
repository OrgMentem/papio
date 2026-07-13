// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The shared fixture corpus is the conformance contract: every file under
// testdata/protocol/valid must decode, every file under testdata/protocol/invalid
// must be rejected — by this package and by the extension's TypeScript parser.

package protocol

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// The IdP privacy invariant is structural: auth payloads cannot carry a URL.
func TestAuthPayloadRejectsURLFields(t *testing.T) {
	msg := []byte(`{"protocol":"papio-browser/0.1","type":"auth_returned","msg_id":"m_auth_ret1","job_id":"job_0002_tyler","seq":5,"payload":{"url":"https://idp.example.edu/sso?token=SECRET"}}`)
	if _, err := DecodeBrowserMessage(msg); err == nil {
		t.Fatal("auth_returned payload with url field was accepted")
	}
}

func TestBrowserMessageSizeCap(t *testing.T) {
	big := append([]byte(`{"protocol":"papio-browser/0.1","type":"ack","msg_id":"m_ack_00001","seq":0,"payload":{}} `), make([]byte, MaxBrowserMessageBytes)...)
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
