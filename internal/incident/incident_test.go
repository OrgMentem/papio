// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package incident

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestAggregateBuildsImmutableDecisiveObservations(t *testing.T) {
	key := []byte("incident-test-key")
	events := []map[string]any{
		{"kind": "browser.page_capture", "at": "2026-08-01T00:00:01Z", "detail": map[string]any{
			"host": "https://first.example.edu/article/old", "adapter_id": "first", "adapter_version": "1", "body": "old",
		}},
		{"kind": "job.transition", "at": "2026-08-01T00:00:02Z", "detail": map[string]any{"reason": "unrelated"}},
		{"kind": "browser.provider_outcome", "at": "2026-08-01T00:00:03Z", "detail": map[string]any{
			"outcome": "ui_changed", "adapter_id": "first", "adapter_version": "1",
		}},
		{"kind": "browser.page_capture", "at": "2026-08-01T00:00:04Z", "detail": map[string]any{
			"host": "later.other.org", "adapter_id": "later", "adapter_version": "2", "body": "later",
		}},
		{"kind": "job.transition", "at": "2026-08-01T00:00:05Z", "detail": map[string]any{"reason": "rewritten"}},
		{"kind": "browser.provider_outcome", "at": "2026-08-01T00:00:06Z", "detail": map[string]any{
			"outcome": "wrong_work", "adapter_id": "later", "adapter_version": "2",
		}},
	}
	groups := Aggregate(key, []JobObservation{{JobID: "job-1", UpdatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Events: events}})
	if len(groups) != 2 {
		t.Fatalf("groups = %#v, want distinct outcomes", groups)
	}
	seen := map[string]Group{}
	for _, group := range groups {
		seen[group.Outcome] = group
		if !group.FirstSeen.Equal(group.LastSeen) {
			t.Fatalf("single decisive event window widened: %#v", group)
		}
	}
	if got := seen["ui_changed"]; got.HostFamily != "example.edu" || !got.FirstSeen.Equal(time.Date(2026, 8, 1, 0, 0, 3, 0, time.UTC)) {
		t.Fatalf("first outcome was rewritten by later context: %#v", got)
	}
	if got := seen["wrong_work"]; got.HostFamily != "other.org" || !got.FirstSeen.Equal(time.Date(2026, 8, 1, 0, 0, 6, 0, time.UTC)) {
		t.Fatalf("second outcome context = %#v", got)
	}
}

func TestAggregateCaptureOnlyAndLateCaptureDoNotInventOrRewrite(t *testing.T) {
	events := []map[string]any{
		{"kind": "browser.page_capture", "at": "2026-08-01T00:00:01Z", "detail": map[string]any{"host": "capture.example.edu", "adapter_id": "a"}},
		{"kind": "job.transition", "at": "2026-08-01T00:00:02Z", "detail": map[string]any{"reason": "late"}},
	}
	if groups := Aggregate([]byte("key"), []JobObservation{{JobID: "capture-only", UpdatedAt: time.Now(), Events: events}}); len(groups) != 0 {
		t.Fatalf("capture-only history produced incidents: %#v", groups)
	}
	events = append(events,
		map[string]any{"kind": "browser.provider_outcome", "at": "2026-08-01T00:00:03Z", "detail": map[string]any{"outcome": "ui_changed", "adapter_id": "a"}},
		map[string]any{"kind": "browser.page_capture", "at": "2026-08-01T00:00:04Z", "detail": map[string]any{"host": "late.other.org", "adapter_id": "a"}},
	)
	groups := Aggregate([]byte("key"), []JobObservation{{JobID: "late-capture", Events: events}})
	if len(groups) != 1 || groups[0].HostFamily != "example.edu" || !groups[0].LastSeen.Equal(time.Date(2026, 8, 1, 0, 0, 3, 0, time.UTC)) {
		t.Fatalf("late capture rewrote decisive observation: %#v", groups)
	}
}
func TestLoadOrCreateKeyConcurrentCreatorsAndInvalidArtifactRecovery(t *testing.T) {
	dir := t.TempDir()
	const creators = 16
	keys := make(chan []byte, creators)
	errs := make(chan error, creators)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			key, err := LoadOrCreateKey(dir)
			if err != nil {
				errs <- err
				return
			}
			keys <- key
		}()
	}
	close(start)
	wg.Wait()
	close(keys)
	close(errs)
	var first []byte
	for key := range keys {
		if first == nil {
			first = key
		} else if string(first) != string(key) {
			t.Fatalf("concurrent creators published different keys")
		}
	}
	for err := range errs {
		t.Fatal(err)
	}
	if len(first) != KeySize {
		t.Fatalf("key length = %d, want %d", len(first), KeySize)
	}
	path := filepath.Join(dir, KeyName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != KeySize {
		t.Fatalf("recovered key length = %d, want %d", len(recovered), KeySize)
	}
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err = LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != KeySize {
		t.Fatalf("recovered key length = %d, want %d", len(recovered), KeySize)
	}
}

func TestLoadOrCreateKeyRecoversInterruptedPublication(t *testing.T) {
	dir := t.TempDir()
	incidentKeyPublicationHook = func(point string) error {
		if point == "synced" {
			return errors.New("injected interruption")
		}
		return nil
	}
	_, err := LoadOrCreateKey(dir)
	incidentKeyPublicationHook = nil
	if err == nil {
		t.Fatal("interrupted first publication unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, KeyName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted publication left final artifact: %v", err)
	}
	key, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeySize {
		t.Fatalf("recovered key length = %d, want %d", len(key), KeySize)
	}
}
