// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package captures keeps sanitized diagnostic pages with their provider context
// under papio's data directory, rather than leaking fixtures into Downloads.
package captures

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	capturesDir      = "captures"
	htmlExt          = ".html"
	metadataExt      = ".json"
	pinExt           = ".pin.json"
	pendingIndexName = ".pending.json"

	// SanitizerProvenance and SanitizerVersion are the only provenance values
	// accepted for adapter repair. They describe the extension sanitizer whose
	// canonical fixture header is checked before bytes enter this store.
	SanitizerProvenance = "papio.extension.sanitizer"
	SanitizerVersion    = "1"
)

var sanitizerFixtureHeader = regexp.MustCompile(`^<!-- papio-fixture provider="([^"]+)" scenario="([^"]+)" origin="([^"]+)" captured="([^"]+)" -->$`)

// hostDirName returns a filesystem-safe, injective directory name for the
// verbatim host. Valid bare-origin hosts (lowercase hostname with optional
// port) are stored verbatim except for ':' which is encoded to keep the name
// safe on Windows. Any byte outside [a-z0-9.-] is percent-encoded, so
// distinct hosts such as "foo/bar" and "foo-bar" map to distinct buckets
// ("foo%2Fbar" vs "foo-bar"). The mapping is injective; the verbatim host is
// recovered from the per-capture metadata sidecar, not the directory name.
// Existing valid-host directories like "sagepub.com" are unchanged, so old
// captures remain readable. Hosts containing ':' previously collapsed to '-';
// those directories are not migrated automatically — they remain under the old
// sanitized name and are still listed (host reported as the sanitized name)
// but new captures for the same verbatim host will use the encoded name. This
// is the minimal break noted in the fix report.
func hostDirName(host string) string {
	normalized := strings.ToLower(strings.TrimSpace(host))
	if normalized == "" {
		return "host"
	}
	var b strings.Builder
	b.Grow(len(normalized) * 3)
	for i := 0; i < len(normalized); i++ {
		c := normalized[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-' {
			b.WriteByte(c)
			continue
		}
		// Percent-encode any other byte (including ':', '/', '%', ' ').
		fmt.Fprintf(&b, "%%%02X", c)
	}
	result := b.String()
	if result == "" || result == "." || result == ".." {
		return "host"
	}
	if len(result) > 200 {
		h := sha256.Sum256([]byte(normalized))
		prefix := result
		if len(prefix) > 100 {
			prefix = prefix[:100]
		}
		return prefix + "-" + hex.EncodeToString(h[:])[:16]
	}
	if len(result) > 253 {
		result = result[:253]
	}
	return result
}

// IsSanitizedFixture verifies the daemon-recognized provenance marker before
// any bytes are persisted. The extension performs secret removal; the daemon
// must nevertheless reject raw or hand-authored HTML that lacks its canonical
// marker.
func IsSanitizedFixture(html []byte) bool {
	first, _, ok := strings.Cut(string(html), "\n")
	if !ok {
		return false
	}
	first = strings.TrimSuffix(first, "\r")
	match := sanitizerFixtureHeader.FindStringSubmatch(first)
	if len(match) != 5 || !validScenario(match[2]) {
		return false
	}
	origin := strings.TrimSpace(match[3])
	if strings.ContainsAny(origin, "?#") {
		return false
	}
	if !strings.HasPrefix(origin, "https://") && !strings.HasPrefix(origin, "http://") {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, match[4]); err != nil {
		return false
	}
	return true
}

// PinRole identifies why a capture is retained for an open incident.
type PinRole string

const (
	PinFirstDecisive PinRole = "first_decisive"
	PinLatest        PinRole = "latest"
)

// Retention bounds diagnostic captures per provider host.
type Retention struct {
	MaxPerHost int
	MaxAge     time.Duration
}

// Capture preserves provider context that would otherwise be lost with HTML alone.
type Capture struct {
	Host                string `json:"host"`
	Scenario            string `json:"scenario"`
	AdapterID           string `json:"adapter_id,omitempty"`
	AdapterVersion      string `json:"adapter_version,omitempty"`
	SHA256              string `json:"sha256,omitempty"`
	SanitizerProvenance string `json:"sanitizer_provenance,omitempty"`
	SanitizerVersion    string `json:"sanitizer_version,omitempty"`
	// IndependentEvidence is false for a caller-labelled capture. It becomes
	// true only when UpdateJob binds the capture to a durable provider outcome.
	IndependentEvidence bool      `json:"independent_evidence,omitempty"`
	Timestamp           time.Time `json:"timestamp"`
	Path                string    `json:"path"`
	Size                int64     `json:"size"`
}

// Store serializes persistence so concurrent diagnostics cannot evade retention.
type Store struct {
	root      string
	retention Retention
	mu        sync.Mutex
	now       func() time.Time
}

// New defers layout creation so disabled intake does not leave unexplained files.
func New(dataDir string, retention Retention) *Store {
	return &Store{
		root:      filepath.Join(dataDir, capturesDir),
		retention: retention,
		now:       time.Now,
	}
}

// Store keeps diagnostic HTML usable while preventing one provider from retaining
// an unbounded volume of stale pages. Bytes written through this generic method
// deliberately carry no sanitizer provenance and cannot be used for adapter
// repair.
func (s *Store) Store(ctx context.Context, host, scenario, adapterID, adapterVersion string, html []byte) (string, error) {
	return s.store(ctx, host, scenario, adapterID, adapterVersion, "", "", "", html)
}

// StoreSanitized is the sole trusted extension-ingress path. It accepts only
// the extension's canonical fixture header and records fixed daemon-owned
// sanitizer provenance beside the exact bytes.
func (s *Store) StoreSanitized(ctx context.Context, host, scenario, adapterID, adapterVersion string, html []byte) (string, error) {
	if !IsSanitizedFixture(html) {
		return "", errors.New("refusing page capture without a canonical extension-sanitized fixture header")
	}
	return s.store(ctx, host, scenario, adapterID, adapterVersion, SanitizerProvenance, SanitizerVersion, "", html)
}

// StoreSanitizedPinned writes and pins a decisive capture before count pruning
// under one store lock. The provisional key is opaque and durable, so a lost
// provider outcome cannot evict the first/latest evidence.
func (s *Store) StoreSanitizedPinned(ctx context.Context, jobID, host, scenario, adapterID, adapterVersion string, html []byte) (string, error) {
	if !IsSanitizedFixture(html) {
		return "", errors.New("refusing page capture without a canonical extension-sanitized fixture header")
	}
	return s.store(ctx, host, scenario, adapterID, adapterVersion, SanitizerProvenance, SanitizerVersion, strings.TrimSpace(jobID), html)
}

// ReleaseJob releases a pre-outcome capture lease. It is safe to call on
// terminal resolution, explicit retry, or reopen even when no lease exists.
func (s *Store) ReleaseJob(ctx context.Context, jobID string) error {
	if strings.TrimSpace(jobID) == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.releaseIncidentLocked(ctx, pendingFingerprint(jobID)); err != nil {
		return err
	}
	return s.removePendingIndexLocked(jobID)
}

// PendingJobs enumerates durable provisional lease associations. The index is
// local daemon state; callers still re-read each job before releasing it.
func (s *Store) PendingJobs(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingJobsLocked()
}

func pendingFingerprint(jobID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(jobID)))
	return "pending:" + hex.EncodeToString(sum[:])
}
func pendingIndexPath(root string) string {
	return filepath.Join(root, pendingIndexName)
}

func (s *Store) pendingJobsLocked() ([]string, error) {
	data, err := os.ReadFile(pendingIndexPath(s.root))
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var index map[string]string
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("decoding capture pending index: %w", err)
	}
	out := make([]string, 0, len(index))
	for _, jobID := range index {
		if strings.TrimSpace(jobID) != "" {
			out = append(out, jobID)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) writePendingIndexLocked(index map[string]string) error {
	if len(index) == 0 {
		if err := os.Remove(pendingIndexPath(s.root)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	return writeAtomically(s.root, pendingIndexPath(s.root), data)
}

func (s *Store) addPendingIndexLocked(jobID string) error {
	data, err := os.ReadFile(pendingIndexPath(s.root))
	index := map[string]string{}
	if err == nil {
		if err := json.Unmarshal(data, &index); err != nil {
			return fmt.Errorf("decoding capture pending index: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	index[pendingFingerprint(jobID)] = strings.TrimSpace(jobID)
	return s.writePendingIndexLocked(index)
}

func (s *Store) removePendingIndexLocked(jobID string) error {
	data, err := os.ReadFile(pendingIndexPath(s.root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var index map[string]string
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("decoding capture pending index: %w", err)
	}
	delete(index, pendingFingerprint(jobID))
	return s.writePendingIndexLocked(index)
}
func (s *Store) pinPendingLocked(ctx context.Context, path, jobID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := s.captureFile(path)
	if err != nil {
		return err
	}
	fingerprint := pendingFingerprint(jobID)
	role := PinFirstDecisive
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		if err := s.writePinLocked(file, fingerprint, role); err != nil {
			return err
		}
		return s.addPendingIndexLocked(jobID)
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, scanErr := scanHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name())
		if scanErr != nil {
			return scanErr
		}
		for _, candidate := range files {
			pin, ok := readPin(candidate.Path)
			if ok && pin.Fingerprint == fingerprint && pin.Role == PinFirstDecisive {
				role = PinLatest
				break
			}
		}
		if role == PinLatest {
			break
		}
	}
	if role == PinLatest {
		if err := s.removeIncidentRoleLocked(ctx, fingerprint, PinLatest, file.Path); err != nil {
			return err
		}
	}
	if err := s.writePinLocked(file, fingerprint, role); err != nil {
		return err
	}
	return s.addPendingIndexLocked(jobID)
}

func (s *Store) store(ctx context.Context, host, scenario, adapterID, adapterVersion, sanitizerProvenance, sanitizerVersion, jobID string, html []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(html) == 0 {
		return "", errors.New("refusing empty capture")
	}
	if !validScenario(scenario) {
		return "", fmt.Errorf("invalid capture scenario %q", scenario)
	}
	if err := s.validateRetention(); err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	verbatimHost := host
	hostDir := filepath.Join(s.root, hostDirName(host))
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		return "", fmt.Errorf("creating capture directory: %w", err)
	}
	path, _, err := s.nextPath(ctx, hostDir, scenario, s.now().UTC())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(html)
	metadata, err := json.Marshal(captureMetadata{
		Host: verbatimHost, AdapterID: adapterID, AdapterVersion: adapterVersion,
		SHA256: hex.EncodeToString(sum[:]), SanitizerProvenance: sanitizerProvenance,
		SanitizerVersion: sanitizerVersion,
	})
	if err != nil {

		return "", fmt.Errorf("encoding capture metadata: %w", err)
	}
	metadataPath := metadataPath(path)
	if err := writeAtomically(hostDir, metadataPath, metadata); err != nil {
		return "", err
	}
	if err := writeAtomically(hostDir, path, html); err != nil {
		_ = os.Remove(metadataPath)
		return "", err
	}
	if strings.TrimSpace(jobID) != "" && scenario != "observed" {
		if err := s.pinPendingLocked(ctx, path, jobID); err != nil {
			return path, err
		}
	}
	if err := s.pruneHost(ctx, hostDir, verbatimHost); err != nil {
		return path, err
	}
	return path, nil
}

// UpdateJob records the first and latest decisive captures for an in-flight
// job under its provisional opaque key. Repeated outcomes therefore demote the
// previous latest atomically without losing the first capture. The correlated
// outcome also upgrades the capture's evidence label; a caller-provided
// scenario alone never does so.
func (s *Store) UpdateJob(ctx context.Context, jobID, firstPath, latestPath string) error {
	if strings.TrimSpace(jobID) == "" || firstPath == "" || latestPath == "" {
		return nil
	}
	if err := s.PinIncident(ctx, pendingFingerprint(jobID), firstPath, latestPath); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, path := range []string{firstPath, latestPath} {
		if err := s.markIndependentLocked(path); err != nil {
			return err
		}
	}
	return nil
}

// markIndependentLocked is called only after the daemon has recorded the
// correlated provider outcome. It intentionally refuses a missing/corrupt
// metadata sidecar rather than upgrading a hand-authored file.
func (s *Store) markIndependentLocked(path string) error {
	file, err := s.captureFile(path)
	if err != nil {
		return fmt.Errorf("locate capture for independent evidence: %w", err)
	}
	metadata, err := readMetadata(file.metadataPath)
	if err != nil {
		return err
	}
	if metadata.SHA256 == "" {
		return errors.New("capture metadata has no content hash")
	}
	// The sidecar proves what was captured; only the bytes on disk prove the
	// file still IS that capture. Promoting on the sidecar alone would label a
	// modified or truncated file as independent provider evidence.
	data, err := os.ReadFile(file.Path)
	if err != nil {
		return fmt.Errorf("reading capture %q: %w", file.Path, err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != metadata.SHA256 {
		return errors.New("capture content no longer matches its recorded hash: refusing to mark a modified capture as independent evidence")
	}
	metadata.IndependentEvidence = true
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encoding independent evidence metadata: %w", err)
	}
	return writeAtomically(filepath.Dir(file.metadataPath), file.metadataPath, encoded)
}

// List makes recent provider observations discoverable without filesystem spelunking.
func (s *Store) List(ctx context.Context) ([]Capture, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(ctx)
}

// Purge permits deliberate cleanup instead of forcing users to delete unknown files.
func (s *Store) Purge(ctx context.Context, host string) (removed int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if host == "" {
		all, err := s.list(ctx)
		if err != nil {
			return 0, err
		}
		if err := os.RemoveAll(s.root); err != nil {
			return 0, fmt.Errorf("purging captures: %w", err)
		}
		return len(all), nil
	}
	hostDir := filepath.Join(s.root, hostDirName(host))
	files, err := scanHost(ctx, hostDir, host)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if err := os.RemoveAll(hostDir); err != nil {
		return 0, fmt.Errorf("purging captures for %s: %w", host, err)
	}
	return len(files), nil
}

// Pin retains one capture outside ordinary age/count eviction. The marker is
// kept beside the capture so this retention survives daemon restarts without a
// schema change.
func (s *Store) Pin(ctx context.Context, path, fingerprint string, role PinRole) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(fingerprint) == "" {
		return errors.New("capture pin requires an incident fingerprint")
	}
	if role != PinFirstDecisive && role != PinLatest {
		return fmt.Errorf("invalid capture pin role %q", role)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := s.captureFile(path)
	if err != nil {
		return err
	}
	marker := capturePin{Fingerprint: strings.TrimSpace(fingerprint), Role: role}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encoding capture pin: %w", err)
	}
	return writeAtomically(filepath.Dir(file.Path), pinPath(file.Path), data)
}

// PinIncident pins the first decisive and latest captures for one open
// incident. A capture may serve both roles when the paths are equal. The
// replacement of a prior latest marker is performed while the store lock is
// held, so retention cannot observe two latest captures for one incident.
func (s *Store) PinIncident(ctx context.Context, fingerprint, firstPath, latestPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return errors.New("capture pin requires an incident fingerprint")
	}
	if firstPath == "" || latestPath == "" {
		return errors.New("capture incident requires first and latest paths")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	first, err := s.captureFile(firstPath)
	if err != nil {
		return err
	}
	latest, err := s.captureFile(latestPath)
	if err != nil {
		return err
	}
	// Remove the old latest marker before publishing the new one. This is
	// intentionally role-scoped: the first decisive capture is immutable.
	if err := s.removeIncidentRoleLocked(ctx, fingerprint, PinLatest, latest.Path); err != nil {
		return err
	}
	firstRole := PinFirstDecisive
	if filepath.Clean(first.Path) == filepath.Clean(latest.Path) {
		firstRole = PinFirstDecisive
	}
	if err := s.writePinLocked(first, fingerprint, firstRole); err != nil {
		return err
	}
	if filepath.Clean(latest.Path) != filepath.Clean(first.Path) {
		if err := s.writePinLocked(latest, fingerprint, PinLatest); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writePinLocked(file captureFile, fingerprint string, role PinRole) error {
	data, err := json.Marshal(capturePin{Fingerprint: fingerprint, Role: role})
	if err != nil {
		return fmt.Errorf("encoding capture pin: %w", err)
	}
	return writeAtomically(filepath.Dir(file.Path), pinPath(file.Path), data)
}

func (s *Store) removeIncidentRoleLocked(ctx context.Context, fingerprint string, role PinRole, keepPath string) error {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, err := scanHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name())
		if err != nil {
			return err
		}
		for _, file := range files {
			if filepath.Clean(file.Path) == filepath.Clean(keepPath) {
				continue
			}
			pin, ok := readPin(file.Path)
			if ok && pin.Fingerprint == fingerprint && pin.Role == role {
				if err := os.Remove(pinPath(file.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
	}
	return nil
}

// ReleaseIncident removes all retention markers for an incident. The next
// Sweep applies normal age/count eviction to the formerly pinned captures.
func (s *Store) ReleaseIncident(ctx context.Context, fingerprint string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return errors.New("incident fingerprint is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseIncidentLocked(ctx, fingerprint)
}

// Sweep applies retention to every host, including captures that became
// unpinned after an incident resolved.
func (s *Store) Sweep(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading capture directory: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if err := s.pruneHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

type captureFile struct {
	Capture
	metadataPath string
}

type captureMetadata struct {
	Host                string `json:"host,omitempty"`
	AdapterID           string `json:"adapter_id,omitempty"`
	AdapterVersion      string `json:"adapter_version,omitempty"`
	SHA256              string `json:"sha256,omitempty"`
	SanitizerProvenance string `json:"sanitizer_provenance,omitempty"`
	SanitizerVersion    string `json:"sanitizer_version,omitempty"`
	IndependentEvidence bool   `json:"independent_evidence,omitempty"`
}

type capturePin struct {
	Fingerprint string  `json:"fingerprint"`
	Role        PinRole `json:"role"`
}

func pinPath(path string) string {
	return strings.TrimSuffix(path, htmlExt) + pinExt
}

func (s *Store) captureFile(path string) (captureFile, error) {
	clean := filepath.Clean(path)
	relative, err := filepath.Rel(s.root, clean)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return captureFile{}, errors.New("capture path is outside the capture directory")
	}
	files, err := scanHost(context.Background(), filepath.Dir(clean), filepath.Base(filepath.Dir(clean)))
	if err != nil {
		return captureFile{}, err
	}
	for _, file := range files {
		if file.Path == clean {
			return file, nil
		}
	}
	return captureFile{}, fs.ErrNotExist
}

func readPin(path string) (capturePin, bool) {
	data, err := os.ReadFile(pinPath(path))
	if err != nil {
		return capturePin{}, false
	}
	var pin capturePin
	if json.Unmarshal(data, &pin) != nil || strings.TrimSpace(pin.Fingerprint) == "" {
		return capturePin{}, false
	}
	return pin, true
}

func (s *Store) releaseIncidentLocked(ctx context.Context, fingerprint string) error {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, err := scanHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name())
		if err != nil {
			return err
		}
		for _, file := range files {
			pin, ok := readPin(file.Path)
			if ok && pin.Fingerprint == fingerprint {
				if err := os.Remove(pinPath(file.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Store) list(ctx context.Context) ([]Capture, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return []Capture{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading capture directory: %w", err)
	}

	out := make([]Capture, 0)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("reading capture host: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, err := scanHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name())
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			metadata, err := readMetadata(file.metadataPath)
			if err != nil {
				return nil, err
			}
			// Recover verbatim host from metadata when available; legacy
			// captures written before host was stored fall back to the
			// filesystem-derived host (directory name).
			if metadata.Host != "" {
				file.Host = metadata.Host
			}
			data, err := os.ReadFile(file.Path)
			if err != nil {
				return nil, fmt.Errorf("reading capture %q: %w", file.Path, err)
			}
			sum := sha256.Sum256(data)
			file.SHA256 = hex.EncodeToString(sum[:])
			file.AdapterID = metadata.AdapterID
			file.AdapterVersion = metadata.AdapterVersion
			file.SanitizerProvenance = metadata.SanitizerProvenance
			file.SanitizerVersion = metadata.SanitizerVersion
			file.IndependentEvidence = metadata.IndependentEvidence
			out = append(out, file.Capture)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Path > out[j].Path
		}
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out, nil
}

func (s *Store) pruneHost(ctx context.Context, hostDir, host string) error {
	files, err := scanHost(ctx, hostDir, host)
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Timestamp.Equal(files[j].Timestamp) {
			return files[i].Path < files[j].Path
		}
		return files[i].Timestamp.Before(files[j].Timestamp)
	})

	cutoff := s.now().UTC().Add(-s.retention.MaxAge)
	kept := files[:0]
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pinned, _ := readPin(file.Path); pinned.Fingerprint != "" {
			kept = append(kept, file)
			continue
		}
		if file.Timestamp.Before(cutoff) {
			if err := removeCapture(file); err != nil {
				return err
			}
			continue
		}
		kept = append(kept, file)
	}
	for len(kept) > s.retention.MaxPerHost {
		evict := -1
		for i, file := range kept {
			if _, pinned := readPin(file.Path); !pinned {
				evict = i
				break
			}
		}
		if evict < 0 {
			break
		}
		if err := removeCapture(kept[evict]); err != nil {
			return err
		}
		kept = append(kept[:evict], kept[evict+1:]...)
	}
	return nil
}
func (s *Store) nextPath(ctx context.Context, hostDir, scenario string, timestamp time.Time) (string, time.Time, error) {
	for candidate := timestamp; ; candidate = candidate.Add(time.Nanosecond) {
		if err := ctx.Err(); err != nil {
			return "", time.Time{}, err
		}
		base := candidate.Format(time.RFC3339Nano) + "-" + scenario
		path := filepath.Join(hostDir, base+htmlExt)
		if _, err := os.Lstat(path); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", time.Time{}, fmt.Errorf("checking capture path: %w", err)
		}
		if _, err := os.Lstat(metadataPath(path)); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", time.Time{}, fmt.Errorf("checking capture metadata path: %w", err)
		}
		return path, candidate, nil
	}
}

func (s *Store) validateRetention() error {
	if s.retention.MaxPerHost < 1 {
		return errors.New("capture retention max per host must be positive")
	}
	if s.retention.MaxAge <= 0 {
		return errors.New("capture retention max age must be positive")
	}
	return nil
}

func scanHost(ctx context.Context, hostDir, host string) ([]captureFile, error) {
	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return nil, fmt.Errorf("reading captures for %s: %w", host, err)
	}
	files := make([]captureFile, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("reading capture %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		timestamp, scenario, ok := parseCaptureName(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(hostDir, entry.Name())
		files = append(files, captureFile{
			Capture: Capture{
				Host: host, Scenario: scenario, Timestamp: timestamp, Path: path, Size: info.Size(),
			},
			metadataPath: metadataPath(path),
		})
	}
	return files, nil
}

func parseCaptureName(name string) (time.Time, string, bool) {
	if !strings.HasSuffix(name, htmlExt) {
		return time.Time{}, "", false
	}
	base := strings.TrimSuffix(name, htmlExt)
	separator := strings.Index(base, "Z-")
	if separator < 0 {
		return time.Time{}, "", false
	}
	timestamp, err := time.Parse(time.RFC3339Nano, base[:separator+1])
	if err != nil {
		return time.Time{}, "", false
	}
	scenario := base[separator+2:]
	if !validScenario(scenario) {
		return time.Time{}, "", false
	}
	return timestamp.UTC(), scenario, true
}

func validScenario(scenario string) bool {
	switch scenario {
	case "observed", "success", "login-return", "no-entitlement", "drift", "terms":
		return true
	default:
		return false
	}
}

func metadataPath(path string) string {
	return strings.TrimSuffix(path, htmlExt) + metadataExt
}

func readMetadata(path string) (captureMetadata, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return captureMetadata{}, nil
	}
	if err != nil {
		return captureMetadata{}, fmt.Errorf("reading capture metadata: %w", err)
	}
	var metadata captureMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return captureMetadata{}, fmt.Errorf("decoding capture metadata: %w", err)
	}
	return metadata, nil
}

func removeCapture(file captureFile) error {
	if err := os.Remove(file.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing capture: %w", err)
	}
	if err := os.Remove(file.metadataPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing capture metadata: %w", err)
	}
	if err := os.Remove(pinPath(file.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing capture pin: %w", err)
	}
	return nil
}

func writeAtomically(dir, path string, data []byte) (err error) {
	temporary, err := os.CreateTemp(dir, ".capture-*.tmp")
	if err != nil {
		return fmt.Errorf("creating capture file: %w", err)
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("securing capture file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("writing capture file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("syncing capture file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("closing capture file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("publishing capture file: %w", err)
	}
	return nil
}
