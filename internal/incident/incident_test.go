// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package incident

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFingerprintStableKeyedAndRedacted(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	in := FingerprintInput{
		SafetyDomain:  "sage",
		HostFamily:    "https://www.sagepub.com/article/10.1234/private-doi",
		OutcomeKind:   "ui_changed",
		MarkerClasses: []string{"browser.provider_outcome.outcome", "browser.provider_outcome.adapter_id"},
	}
	got := Fingerprint(key, in)
	if got != Fingerprint(key, in) {
		t.Fatal("fingerprint changed across identical runs")
	}
	if len(got) != FingerprintSize*2 || strings.Contains(got, "sagepub") || strings.Contains(got, "private-doi") {
		t.Fatalf("fingerprint = %q, want redacted %d-byte hex", got, FingerprintSize)
	}
	if got != Fingerprint(key, FingerprintInput{
		SafetyDomain: "sage", HostFamily: "www.sagepub.com/another/identifier",
		OutcomeKind: "ui_changed", MarkerClasses: []string{"browser.provider_outcome.adapter_id", "browser.provider_outcome.outcome"},
	}) {
		t.Fatal("marker order or route identifier changed fingerprint")
	}
	if got == Fingerprint([]byte("another-key-that-is-also-32-byte!!"), in) {
		t.Fatal("different installation keys must produce distinct fingerprints")
	}
}

func TestLoadOrCreateKeyCreatesOnceWith0600(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || len(first) != KeySize {
		t.Fatal("incident key was not stable")
	}
	info, err := os.Stat(filepath.Join(dir, KeyName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("incident key mode = %o, want 600", info.Mode().Perm())
	}
}

func TestInputFromEventsUsesNamesAndNormalizesHost(t *testing.T) {
	input := InputFromEvents([]map[string]any{
		{"kind": "browser.page_capture", "detail": map[string]any{"host": "https://www.example.edu/article/10.1000/secret", "adapter_id": "adapter"}},
		{"kind": "browser.provider_outcome", "detail": map[string]any{"host": "provider.example.edu", "outcome": "ui_changed", "selector_value": "private"}},
		{"kind": "job.latch", "detail": map[string]any{"kind": "drift", "safety_domain": "adapter"}},
	})
	if input.HostFamily != "example.edu" || input.SafetyDomain != "adapter" || input.OutcomeKind != "ui_changed" {
		t.Fatalf("input = %#v", input)
	}
	for _, marker := range input.MarkerClasses {
		if strings.Contains(marker, "private") || strings.Contains(marker, "secret") || strings.Contains(marker, "10.1000") {
			t.Fatalf("marker leaked detail value: %q", marker)
		}
	}
}

func TestNormalizeHostFamily(t *testing.T) {
	for raw, want := range map[string]string{
		"https://www.provider.example.co.uk/article/42?doi=10.1/x": "example.co.uk",
		"provider.example.edu/path":                                "example.edu",
		"localhost:8080":                                           "localhost",
		"192.0.2.10/article":                                       "ip",
	} {
		if got := NormalizeHostFamily(raw); got != want {
			t.Errorf("NormalizeHostFamily(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestAggregateGroupsJobsByKeyedShapeAndEventWindow(t *testing.T) {
	key := []byte("installation-key")
	observations := []JobObservation{
		{
			JobID: "job-a", UpdatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Events: []map[string]any{
				{"kind": "browser.provider_outcome", "at": "2026-08-01T00:00:01Z", "detail": map[string]any{"host": "one.example.edu", "outcome": "ui_changed", "adapter_id": "a"}},
			},
		},
		{
			JobID: "job-b", UpdatedAt: time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC),
			Events: []map[string]any{
				{"kind": "browser.provider_outcome", "at": "2026-08-01T01:00:01Z", "detail": map[string]any{"host": "www.one.example.edu", "outcome": "ui_changed", "adapter_id": "a"}},
			},
		},
	}
	groups := Aggregate(key, observations)
	if len(groups) != 1 || groups[0].Jobs != 2 || groups[0].HostFamily != "example.edu" {
		t.Fatalf("groups = %#v, want one two-job example.edu group", groups)
	}
	if !groups[0].FirstSeen.Equal(time.Date(2026, 8, 1, 0, 0, 1, 0, time.UTC)) ||
		!groups[0].LastSeen.Equal(time.Date(2026, 8, 1, 1, 0, 1, 0, time.UTC)) {
		t.Fatalf("event window = %s..%s", groups[0].FirstSeen, groups[0].LastSeen)
	}
}
