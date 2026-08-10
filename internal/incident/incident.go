// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package incident provides local, redacted failure-shape identities. The
// fingerprint is deliberately keyed: it is useful for joining observations on
// one installation but is not a public hash of provider strings.
package incident

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

const (
	KeyName         = "incident.key"
	KeySize         = 32
	FingerprintSize = 16
)

// FingerprintInput is the redacted failure shape used to group incidents.
// HostFamily must be a route family, not a URL or an identifier-bearing path.
type FingerprintInput struct {
	SafetyDomain  string
	HostFamily    string
	OutcomeKind   string
	MarkerClasses []string
}

// Fingerprint returns a 16-byte, lowercase hexadecimal HMAC-SHA256 digest.
// Length-prefixing keeps fields unambiguous while the marker list is sorted so
// event traversal order cannot change the identity.
func Fingerprint(key []byte, in FingerprintInput) string {
	mac := hmac.New(sha256.New, key)
	markers := append([]string(nil), in.MarkerClasses...)
	for i := range markers {
		markers[i] = strings.TrimSpace(markers[i])
	}
	sort.Strings(markers)
	markers = unique(markers)
	writeField := func(value string) {
		value = strings.TrimSpace(value)
		fmt.Fprintf(mac, "%d:", len(value))
		mac.Write([]byte(value))
	}
	writeField(strings.TrimSpace(in.SafetyDomain))
	writeField(NormalizeHostFamily(in.HostFamily))
	writeField(strings.TrimSpace(in.OutcomeKind))
	writeField(strings.Join(markers, "\x1f"))
	return hex.EncodeToString(mac.Sum(nil)[:FingerprintSize])
}

// LoadOrCreateKey loads the per-installation incident key, creating it once
// with restrictive permissions when needed.
func LoadOrCreateKey(dataDir string) ([]byte, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("incident data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating incident data directory: %w", err)
	}
	path := filepath.Join(dataDir, KeyName)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("incident key must not be a symlink")
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		key := make([]byte, KeySize)
		if _, err := rand.Read(key); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("generating incident key: %w", err)
		}
		if _, err := file.Write(key); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("writing incident key: %w", err)
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("restricting incident key: %w", err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("closing incident key: %w", err)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("creating incident key: %w", err)
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading incident key: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("incident key has length %d, want %d", len(key), KeySize)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("restricting incident key: %w", err)
	}
	return key, nil
}

// NormalizeHostFamily extracts a registrable domain and discards path/query
// identifiers. Unknown/private suffixes fall back to the final two labels.
func NormalizeHostFamily(raw string) string {
	raw = strings.TrimSpace(raw)
	if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
		raw = parsed.Hostname()
	} else if slash := strings.IndexAny(raw, "/?#"); slash >= 0 {
		raw = raw[:slash]
	}
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	raw = strings.Trim(strings.ToLower(strings.TrimSpace(raw)), ".")
	if raw == "" {
		return "unknown"
	}
	if net.ParseIP(raw) != nil {
		return "ip"
	}
	if domain, err := publicsuffix.EffectiveTLDPlusOne(raw); err == nil {
		return domain
	}
	parts := strings.Split(raw, ".")
	if len(parts) > 2 {
		return strings.Join(parts[len(parts)-2:], ".")
	}
	return raw
}

// InputFromEvents derives a redacted fingerprint shape from the existing
// durable event forms. It records detail key names, never detail values, as
// marker classes; only the bounded outcome/latch vocabulary is retained.
func InputFromEvents(events []map[string]any) FingerprintInput {
	var in FingerprintInput
	markerSet := make(map[string]struct{})
	for _, event := range events {
		kind, _ := event["kind"].(string)
		if kind != "browser.provider_outcome" && kind != "job.latch" && kind != "browser.page_capture" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		for key := range detail {
			markerSet[key] = struct{}{}
		}
		if host, _ := detail["host"].(string); host != "" {
			in.HostFamily = NormalizeHostFamily(host)
		}
		if domain, _ := detail["safety_domain"].(string); domain != "" {
			in.SafetyDomain = strings.TrimSpace(domain)
		}
		if outcome, _ := detail["outcome"].(string); outcome != "" && kind == "browser.provider_outcome" {
			in.OutcomeKind = strings.TrimSpace(outcome)
		}
		if in.OutcomeKind == "" {
			if latch, _ := detail["kind"].(string); kind == "job.latch" && latch != "" {
				in.OutcomeKind = "latch_" + strings.TrimSpace(latch)
			}
		}
		if in.OutcomeKind == "" {
			if scenario, _ := detail["scenario"].(string); kind == "browser.page_capture" && scenario != "" {
				in.OutcomeKind = "capture_" + strings.TrimSpace(scenario)
			}
		}
		if in.SafetyDomain == "" {
			if adapter, _ := detail["adapter_id"].(string); adapter != "" {
				in.SafetyDomain = strings.TrimSpace(adapter)
			}
		}
	}
	for marker := range markerSet {
		in.MarkerClasses = append(in.MarkerClasses, marker)
	}
	sort.Strings(in.MarkerClasses)
	if in.HostFamily == "" {
		in.HostFamily = "unknown"
	}
	if in.SafetyDomain == "" {
		in.SafetyDomain = "unknown"
	}
	if in.OutcomeKind == "" {
		in.OutcomeKind = "unknown"
	}
	return in
}

func unique(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

// JobObservation is the minimal event/read-model input needed to aggregate
// incidents without importing the job package.
type JobObservation struct {
	JobID     string
	State     string
	UpdatedAt time.Time
	Events    []map[string]any
}

// Group is the redacted operator-facing incident aggregate.
type Group struct {
	Fingerprint  string    `json:"fingerprint"`
	SafetyDomain string    `json:"safety_domain"`
	HostFamily   string    `json:"host_family"`
	Outcome      string    `json:"outcome"`
	Jobs         int       `json:"jobs"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
}

// Aggregate groups observations by keyed failure shape. Jobs with no decisive
// observation are omitted rather than assigned a misleading generic incident.
func Aggregate(key []byte, observations []JobObservation) []Group {
	type aggregate struct {
		Group
		seen map[string]struct{}
	}
	groups := make(map[string]*aggregate)
	for _, observation := range observations {
		input := InputFromEvents(observation.Events)
		if input.OutcomeKind == "unknown" {
			continue
		}
		var firstSeen, lastSeen time.Time
		for _, event := range observation.Events {
			at, _ := event["at"].(string)
			timestamp, err := time.Parse(time.RFC3339Nano, at)
			if err != nil {
				continue
			}
			if firstSeen.IsZero() {
				firstSeen, lastSeen = timestamp, timestamp
				continue
			}
			if timestamp.Before(firstSeen) {
				firstSeen = timestamp
			}
			if timestamp.After(lastSeen) {
				lastSeen = timestamp
			}
		}
		if firstSeen.IsZero() {
			firstSeen, lastSeen = observation.UpdatedAt, observation.UpdatedAt
		}
		fingerprint := Fingerprint(key, input)
		group := groups[fingerprint]
		if group == nil {
			group = &aggregate{Group: Group{
				Fingerprint: fingerprint, SafetyDomain: input.SafetyDomain,
				HostFamily: input.HostFamily, Outcome: input.OutcomeKind,
				FirstSeen: firstSeen, LastSeen: lastSeen,
			}, seen: make(map[string]struct{})}
			groups[fingerprint] = group
		}
		if _, ok := group.seen[observation.JobID]; !ok {
			group.seen[observation.JobID] = struct{}{}
			group.Jobs++
		}
		if firstSeen.Before(group.FirstSeen) {
			group.FirstSeen = firstSeen
		}
		if lastSeen.After(group.LastSeen) {
			group.LastSeen = lastSeen
		}
	}
	out := make([]Group, 0, len(groups))
	for _, group := range groups {
		group.seen = nil
		out = append(out, group.Group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].Fingerprint < out[j].Fingerprint
		}
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}
