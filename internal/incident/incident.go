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
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

var (
	incidentKeyPublicationHook func(string) error
	incidentKeyMu              sync.Mutex
)

func keyPublicationPoint(point string) error {
	if incidentKeyPublicationHook == nil {
		return nil
	}
	return incidentKeyPublicationHook(point)
}

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

// readPublishedKey accepts only a complete regular key artifact. The boolean
// reports whether the final path was present, including a malformed regular
// artifact that the caller may safely replace.
func readPublishedKey(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("checking incident key: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, true, errors.New("incident key must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("incident key must be a regular file, got %s", info.Mode())
	}

	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("opening incident key: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, true, fmt.Errorf("stating incident key: %w", err)
	}
	if !openedInfo.Mode().IsRegular() {
		return nil, true, fmt.Errorf("incident key must be a regular file, got %s", openedInfo.Mode())
	}
	if !os.SameFile(info, openedInfo) {
		// The path changed between validation and open. Never trust bytes
		// obtained through that race; let the caller retry the path.
		return nil, false, nil
	}
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(file, key); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, true, nil
		}
		return nil, true, fmt.Errorf("reading incident key: %w", err)
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 {
		return nil, true, nil
	} else if !errors.Is(err, io.EOF) {
		return nil, true, fmt.Errorf("checking incident key length: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, true, fmt.Errorf("restricting incident key: %w", err)
	}
	return key, true, nil
}

// LoadOrCreateKey loads the per-installation incident key, creating it once
// with restrictive permissions when needed. A key is never published until a
// same-directory temporary file has been completely written, synced, closed,
// and then linked into place with exclusive semantics.
func LoadOrCreateKey(dataDir string) ([]byte, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("incident data directory is required")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating incident data directory: %w", err)
	}
	incidentKeyMu.Lock()
	defer incidentKeyMu.Unlock()
	path := filepath.Join(dataDir, KeyName)
	for {
		key, present, err := readPublishedKey(path)
		if err != nil {
			return nil, err
		}
		if present {
			if key != nil {
				return key, nil
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("removing invalid incident key: %w", err)
			}
			continue
		}
		temp, err := os.CreateTemp(dataDir, "."+KeyName+".tmp-*")
		if err != nil {
			return nil, fmt.Errorf("creating incident key temporary file: %w", err)
		}
		tempPath := temp.Name()
		cleanup := func() {
			_ = temp.Close()
			_ = os.Remove(tempPath)
		}
		fail := func(message string, err error) ([]byte, error) {
			cleanup()
			return nil, fmt.Errorf("%s: %w", message, err)
		}
		if err := keyPublicationPoint("temp_created"); err != nil {
			return fail("incident key publication interrupted", err)
		}
		newKey := make([]byte, KeySize)
		if _, err := rand.Read(newKey); err != nil {
			return fail("generating incident key", err)
		}
		n, err := temp.Write(newKey)
		if err != nil {
			return fail("writing incident key", err)
		}
		if n != len(newKey) {
			return fail("writing incident key", io.ErrShortWrite)
		}
		if err := keyPublicationPoint("written"); err != nil {
			return fail("incident key publication interrupted", err)
		}
		if err := temp.Chmod(0o600); err != nil {
			return fail("restricting incident key", err)
		}
		if err := keyPublicationPoint("chmod"); err != nil {
			return fail("incident key publication interrupted", err)
		}
		if err := temp.Sync(); err != nil {
			return fail("syncing incident key", err)
		}
		if err := keyPublicationPoint("synced"); err != nil {
			return fail("incident key publication interrupted", err)
		}
		if err := temp.Close(); err != nil {
			_ = os.Remove(tempPath)
			return nil, fmt.Errorf("closing incident key: %w", err)
		}
		if err := keyPublicationPoint("closed"); err != nil {
			_ = os.Remove(tempPath)
			return nil, fmt.Errorf("incident key publication interrupted: %w", err)
		}
		err = os.Link(tempPath, path)
		if err == nil {
			_ = os.Remove(tempPath)
			if err := keyPublicationPoint("published"); err != nil {
				return nil, fmt.Errorf("incident key publication interrupted: %w", err)
			}
			dir, openErr := os.Open(dataDir)
			if openErr != nil {
				return nil, fmt.Errorf("opening incident key directory: %w", openErr)
			}
			syncErr := dir.Sync()
			closeErr := dir.Close()
			if syncErr != nil {
				return nil, fmt.Errorf("syncing incident key directory: %w", syncErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("closing incident key directory: %w", closeErr)
			}
			return newKey, nil
		}
		_ = os.Remove(tempPath)
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("publishing incident key: %w", err)
		}
	}
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

// InputFromEvents returns the first immutable provider-outcome shape. It is
// retained as a compatibility helper, but deliberately does not project later
// events into an earlier incident.
func InputFromEvents(events []map[string]any) FingerprintInput {
	for index, event := range events {
		if kind, _ := event["kind"].(string); kind != "browser.provider_outcome" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		if strings.TrimSpace(stringDetail(detail, "outcome")) == "" {
			continue
		}
		return decisiveInput(events, index)
	}
	return unknownInput()
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

// decisiveObservation is an immutable shape/window extracted at the instant a
// provider outcome was recorded. Nothing after that outcome can participate.
type decisiveObservation struct {
	input FingerprintInput
	at    time.Time
}

func eventTime(event map[string]any) (time.Time, bool) {
	raw, _ := event["at"].(string)
	at, err := time.Parse(time.RFC3339Nano, raw)
	return at, err == nil
}

func eventEpoch(event map[string]any) string {
	for _, key := range []string{"drive_attempt_id", "epoch", "drive_epoch"} {
		if value, ok := event[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	detail, _ := event["detail"].(map[string]any)
	for _, key := range []string{"drive_attempt_id", "epoch", "drive_epoch"} {
		if value, ok := detail[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stringDetail(detail map[string]any, key string) string {
	value, _ := detail[key].(string)
	return value
}

func unknownInput() FingerprintInput {
	return FingerprintInput{
		SafetyDomain: "unknown",
		HostFamily:   "unknown",
		OutcomeKind:  "unknown",
	}
}

func decisiveInput(events []map[string]any, index int) FingerprintInput {
	detail, _ := events[index]["detail"].(map[string]any)
	input := FingerprintInput{
		OutcomeKind:  strings.TrimSpace(stringDetail(detail, "outcome")),
		HostFamily:   NormalizeHostFamily(stringDetail(detail, "host")),
		SafetyDomain: strings.ToLower(strings.TrimSpace(stringDetail(detail, "safety_domain"))),
	}
	if input.SafetyDomain == "" || input.SafetyDomain == "unknown" {
		input.SafetyDomain = strings.ToLower(strings.TrimSpace(stringDetail(detail, "adapter_id")))
	}
	markers := make(map[string]struct{}, len(detail))
	for key := range detail {
		markers[key] = struct{}{}
	}
	epoch := eventEpoch(events[index])
	adapterID := strings.TrimSpace(stringDetail(detail, "adapter_id"))
	adapterVersion := strings.TrimSpace(stringDetail(detail, "adapter_version"))
	// A capture belongs to this outcome only while walking backward inside
	// the current drive epoch. The prior provider outcome is the durable
	// boundary even for histories that predate explicit epoch fields.
	for prior := index - 1; prior >= 0; prior-- {
		previous := events[prior]
		if kind, _ := previous["kind"].(string); kind == "browser.provider_outcome" {
			break
		}
		if kind, _ := previous["kind"].(string); kind != "browser.page_capture" {
			continue
		}
		captureEpoch := eventEpoch(previous)
		if epoch != "" && captureEpoch != "" && captureEpoch != epoch {
			continue
		}
		capture, _ := previous["detail"].(map[string]any)
		captureID := strings.TrimSpace(stringDetail(capture, "adapter_id"))
		captureVersion := strings.TrimSpace(stringDetail(capture, "adapter_version"))
		if adapterID != "" && captureID != "" && captureID != adapterID {
			continue
		}
		if adapterVersion != "" && captureVersion != "" && captureVersion != adapterVersion {
			continue
		}
		if input.HostFamily == "unknown" || input.HostFamily == "" {
			input.HostFamily = NormalizeHostFamily(stringDetail(capture, "host"))
		}
		if input.SafetyDomain == "" || input.SafetyDomain == "unknown" {
			captureDomain := strings.TrimSpace(stringDetail(capture, "safety_domain"))
			if captureDomain == "" {
				captureDomain = captureID
			}
			input.SafetyDomain = strings.ToLower(captureDomain)
		}
		for key := range capture {
			markers[key] = struct{}{}
		}
		// The nearest compatible capture is the complete context. Older
		// captures are prior navigation state and must not rewrite it.
		break
	}
	if input.HostFamily == "" {
		input.HostFamily = "unknown"
	}
	if input.SafetyDomain == "" {
		input.SafetyDomain = "unknown"
	}
	if input.OutcomeKind == "" {
		input.OutcomeKind = "unknown"
	}
	for marker := range markers {
		input.MarkerClasses = append(input.MarkerClasses, marker)
	}
	sort.Strings(input.MarkerClasses)
	return input
}

func decisiveObservations(events []map[string]any) []decisiveObservation {
	var out []decisiveObservation
	for index, event := range events {
		if kind, _ := event["kind"].(string); kind != "browser.provider_outcome" {
			continue
		}
		at, ok := eventTime(event)
		if !ok {
			continue
		}
		input := decisiveInput(events, index)
		if input.OutcomeKind == "unknown" {
			continue
		}
		out = append(out, decisiveObservation{input: input, at: at})
	}
	return out
}

// Aggregate groups immutable decisive provider observations by keyed failure
// shape. Capture-only history and non-decisive transitions are ignored.
func Aggregate(key []byte, observations []JobObservation) []Group {
	type aggregate struct {
		Group
		seen map[string]struct{}
	}
	groups := make(map[string]*aggregate)
	for _, observation := range observations {
		for _, decisive := range decisiveObservations(observation.Events) {
			fingerprint := Fingerprint(key, decisive.input)
			group := groups[fingerprint]
			if group == nil {
				group = &aggregate{Group: Group{
					Fingerprint:  fingerprint,
					SafetyDomain: decisive.input.SafetyDomain,
					HostFamily:   decisive.input.HostFamily,
					Outcome:      decisive.input.OutcomeKind,
					FirstSeen:    decisive.at,
					LastSeen:     decisive.at,
				}, seen: make(map[string]struct{})}
				groups[fingerprint] = group
			}
			if _, ok := group.seen[observation.JobID]; !ok {
				group.seen[observation.JobID] = struct{}{}
				group.Jobs++
			}
			if decisive.at.Before(group.FirstSeen) {
				group.FirstSeen = decisive.at
			}
			if decisive.at.After(group.LastSeen) {
				group.LastSeen = decisive.at
			}
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
