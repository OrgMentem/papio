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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"papio/internal/redact"
)

const (
	capturesDir      = "captures"
	htmlExt          = ".html"
	metadataExt      = ".json"
	pinExt           = ".pin.json"
	pendingIndexName = ".pending.json"

	// RFC3339's clock colons are invalid in Windows filenames. Keep UTC and
	// nanosecond precision here; fixture and JSON timestamps stay RFC3339.
	captureTimestampLayout = "2006-01-02T15-04-05.999999999Z"

	// SanitizerProvenance and SanitizerVersion are the only provenance values
	// accepted for adapter repair. They describe the extension sanitizer whose
	// canonical fixture header is checked before bytes enter this store.
	// Version 2: IPv4 and IPv6 addresses are masked, by the extension sanitizer
	// and again by this store (redact.IPAddresses), so a version 2 capture
	// carries no address even from an older extension. A version 1 capture may
	// still hold the operator's IP address.
	SanitizerProvenance = "papio.extension.sanitizer"
	SanitizerVersion    = "2"
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
	return s.store(ctx, host, scenario, adapterID, adapterVersion, SanitizerProvenance, SanitizerVersion, "", maskIPAddresses(html))
}

// StoreSanitizedPinned writes and pins a decisive capture before count pruning
// under one store lock. The provisional key is opaque and durable, so a lost
// provider outcome cannot evict the first/latest evidence.
func (s *Store) StoreSanitizedPinned(ctx context.Context, jobID, host, scenario, adapterID, adapterVersion string, html []byte) (string, error) {
	if !IsSanitizedFixture(html) {
		return "", errors.New("refusing page capture without a canonical extension-sanitized fixture header")
	}
	return s.store(ctx, host, scenario, adapterID, adapterVersion, SanitizerProvenance, SanitizerVersion, strings.TrimSpace(jobID), maskIPAddresses(html))
}

// maskIPAddresses applies the version 2 address rule at the trusted ingress,
// with the extension sanitizer's own placeholder.
func maskIPAddresses(html []byte) []byte {
	return []byte(redact.IPAddresses(string(html), "TOKEN"))
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

// addPendingIndexLocked records the job's provisional lease association. It
// reports whether this call introduced the entry, so a rollback undoes only its
// own side effect and never deletes an association that an earlier successful
// capture committed.
func (s *Store) addPendingIndexLocked(jobID string) (bool, error) {
	data, err := os.ReadFile(pendingIndexPath(s.root))
	index := map[string]string{}
	if err == nil {
		if err := json.Unmarshal(data, &index); err != nil {
			return false, fmt.Errorf("decoding capture pending index: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	fingerprint := pendingFingerprint(jobID)
	trimmed := strings.TrimSpace(jobID)
	previous, existed := index[fingerprint]
	if existed && previous == trimmed {
		return false, nil
	}
	index[fingerprint] = trimmed
	if err := s.writePendingIndexLocked(index); err != nil {
		return false, err
	}
	return !existed, nil
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

// pendingRoleLocked reports whether a capture becomes the incident's first
// decisive evidence or its new latest. The scan reads pins only; it publishes
// nothing, so a failure here leaves no side effect to undo.
func (s *Store) pendingRoleLocked(ctx context.Context, fingerprint string) (PinRole, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return PinFirstDecisive, nil
	}
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, err := scanHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name())
		if err != nil {
			return "", err
		}
		for _, candidate := range files {
			pin, ok := readPin(candidate.Path)
			if ok && pin.Fingerprint == fingerprint && pin.Role == PinFirstDecisive {
				return PinLatest, nil
			}
		}
	}
	return PinFirstDecisive, nil
}

// pinPendingLocked pins a capture the caller has just written and records the
// durable lease association for its job. It is all-or-nothing: a failure undoes
// the pin sidecar and the index entry this call published, and it displaces a
// prior latest marker only after both are durable. A caller that sees an error
// therefore never observes a demoted earlier capture, an unenumerable lease, or
// a pin that exempts the capture from retention forever. The index is published
// before the pin so a crash between the two leaves an index entry without a pin
// rather than a pin without an index (invisible to PendingJobs and exempt from
// retention sweeps forever). The capture metadata already links the bytes to
// the job, so ReconcilePendingPins restores the missing pin for an active lease
// and retention exempts the indexed capture until then.
func (s *Store) pinPendingLocked(ctx context.Context, path, jobID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := s.captureFile(path)
	if err != nil {
		return err
	}
	fingerprint := pendingFingerprint(jobID)
	role, err := s.pendingRoleLocked(ctx, fingerprint)
	if err != nil {
		return err
	}
	indexed, err := s.addPendingIndexLocked(jobID)
	if err != nil {
		return err
	}
	if err := s.writePinLocked(file, fingerprint, jobID, role); err != nil {
		if indexed {
			_ = s.removePendingIndexLocked(jobID)
		}
		return err
	}
	if role != PinLatest {
		return nil
	}
	// The previous latest marker is displaced last. Demoting it before the two
	// writes above would leave the incident with no latest evidence whenever
	// either of them failed.
	if err := s.removeIncidentRoleLocked(ctx, fingerprint, PinLatest, file.Path); err != nil {
		s.discardPinLocked(file.Path)
		if indexed {
			_ = s.removePendingIndexLocked(jobID)
		}
		return err
	}
	return nil
}

// discardPinLocked drops a pin sidecar this call published. The caller derived
// the path from capture bytes it created in the same call, so no marker from an
// earlier successful capture can be removed here.
func (s *Store) discardPinLocked(path string) {
	_ = os.Remove(pinPath(path))
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
	pendingJobID := strings.TrimSpace(jobID)
	if scenario == "observed" {
		pendingJobID = ""
	}
	metadata, err := json.Marshal(captureMetadata{
		Host: verbatimHost, AdapterID: adapterID, AdapterVersion: adapterVersion,
		SHA256: hex.EncodeToString(sum[:]), SanitizerProvenance: sanitizerProvenance,
		SanitizerVersion: sanitizerVersion, PendingJobID: pendingJobID,
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
			// Without its pin and lease entry the capture would be
			// unreleasable: pruneHost keeps any pinned file regardless of age
			// or count, and PendingJobs enumerates the index alone. Undo the
			// bytes and metadata this call wrote and report no path, so the
			// caller cannot mistake the discarded capture for stored evidence.
			s.discardPinLocked(path)
			_ = os.Remove(path)
			_ = os.Remove(metadataPath)
			return "", err
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
	return s.writePinLocked(file, strings.TrimSpace(fingerprint), "", role)
}

// PinIncident pins the first decisive and latest captures for one open
// incident. A capture may serve both roles when the paths are equal. The
// replacement of a prior latest marker is performed while the store lock is
// held, so retention cannot observe two latest captures for one incident.
// It is all-or-nothing: the prior latest marker is displaced only after both
// new markers are durable, and a failure puts back the sidecars this call
// overwrote, so a caller that sees an error never loses its prior evidence.
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
	type pinTarget struct {
		file captureFile
		role PinRole
	}
	targets := []pinTarget{{first, PinFirstDecisive}}
	if filepath.Clean(latest.Path) != filepath.Clean(first.Path) {
		targets = append(targets, pinTarget{latest, PinLatest})
	}
	var written []pinSnapshot
	undo := func() {
		for _, snapshot := range slices.Backward(written) {
			snapshot.restore()
		}
	}
	for _, target := range targets {
		snapshot, err := snapshotPin(target.file)
		if err != nil {
			undo()
			return err
		}
		if err := s.writePinLocked(target.file, fingerprint, "", target.role); err != nil {
			undo()
			return err
		}
		written = append(written, snapshot)
	}
	// The previous latest marker is displaced last. Demoting it before the
	// writes above would leave the incident with no latest evidence whenever
	// either of them failed. The first decisive capture is immutable, so the
	// displacement is role-scoped.
	if err := s.removeIncidentRoleLocked(ctx, fingerprint, PinLatest, latest.Path); err != nil {
		undo()
		return err
	}
	return nil
}

// pinSnapshot is a pin sidecar as it was before a write, so a failed
// multi-marker publication can put it back byte for byte.
type pinSnapshot struct {
	file    captureFile
	data    []byte
	existed bool
}

func snapshotPin(file captureFile) (pinSnapshot, error) {
	data, err := os.ReadFile(pinPath(file.Path))
	if errors.Is(err, fs.ErrNotExist) {
		return pinSnapshot{file: file}, nil
	}
	if err != nil {
		return pinSnapshot{}, err
	}
	return pinSnapshot{file: file, data: data, existed: true}, nil
}

// restore is best-effort: it runs only on a path that already returns the
// original error to the caller.
func (p pinSnapshot) restore() {
	if !p.existed {
		_ = os.Remove(pinPath(p.file.Path))
		return
	}
	_ = writeAtomically(filepath.Dir(p.file.Path), pinPath(p.file.Path), p.data)
}

func (s *Store) writePinLocked(file captureFile, fingerprint, jobID string, role PinRole) error {
	fingerprint = strings.TrimSpace(fingerprint)
	jobID = strings.TrimSpace(jobID)
	if jobID == "" && strings.HasPrefix(fingerprint, "pending:") {
		// Recover the job for a pending lease from the durable index so a pin
		// rewritten without its caller (PinIncident, displacement restore)
		// still carries the link the reconciler needs when the index is lost.
		if data, err := os.ReadFile(pendingIndexPath(s.root)); err == nil {
			var index map[string]string
			if json.Unmarshal(data, &index) == nil {
				jobID = strings.TrimSpace(index[fingerprint])
			}
		}
	}
	data, err := json.Marshal(capturePin{Fingerprint: fingerprint, Role: role, JobID: jobID})
	if err != nil {
		return fmt.Errorf("encoding capture pin: %w", err)
	}
	return writeAtomically(filepath.Dir(file.Path), pinPath(file.Path), data)
}

// displacedPin remembers a role marker verbatim so a failed displacement can
// republish it instead of leaving the incident without that role.
type displacedPin struct {
	file captureFile
	pin  capturePin
}

// findIncidentRoleLocked collects every marker for one incident role except the
// capture at keepPath. It only reads.
func (s *Store) findIncidentRoleLocked(ctx context.Context, fingerprint string, role PinRole, keepPath string) ([]displacedPin, error) {
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []displacedPin
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, err := scanHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name())
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if filepath.Clean(file.Path) == filepath.Clean(keepPath) {
				continue
			}
			pin, ok := readPin(file.Path)
			if ok && pin.Fingerprint == fingerprint && pin.Role == role {
				found = append(found, displacedPin{file: file, pin: pin})
			}
		}
	}
	return found, nil
}

// removeIncidentRoleLocked drops every marker for one incident role except the
// capture at keepPath. It is all-or-nothing: a failure part way through
// republishes the markers already removed, so no caller can lose a prior
// latest capture to a partially applied displacement. A displaced pending-lease
// capture also drops its metadata lease link, so retention treats it as
// ordinary evidence again instead of keeping every intermediate capture for an
// active job forever. The link clear is best-effort: the pins are already
// displaced, so a metadata write failure only leaks retention, never evidence.
func (s *Store) removeIncidentRoleLocked(ctx context.Context, fingerprint string, role PinRole, keepPath string) error {
	displaced, err := s.findIncidentRoleLocked(ctx, fingerprint, role, keepPath)
	if err != nil {
		return err
	}
	for i, marker := range displaced {
		if err := os.Remove(pinPath(marker.file.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.restorePinsLocked(displaced[:i])
			return err
		}
	}
	for _, marker := range displaced {
		s.clearPendingLinkLocked(marker.file, fingerprint)
	}
	return nil
}

// clearPendingLinkLocked drops the metadata lease link when a capture stops
// being first/latest evidence for its fingerprint. Best-effort: callers have
// already displaced the pin, so a failure only delays eviction. Callers hold
// s.mu.
func (s *Store) clearPendingLinkLocked(file captureFile, fingerprint string) {
	metadata, err := readMetadata(file.metadataPath)
	if err != nil || strings.TrimSpace(metadata.PendingJobID) == "" {
		return
	}
	if pendingFingerprint(metadata.PendingJobID) != fingerprint {
		return
	}
	metadata.PendingJobID = ""
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return
	}
	_ = writeAtomically(filepath.Dir(file.metadataPath), file.metadataPath, encoded)
}

func (s *Store) restorePinsLocked(displaced []displacedPin) {
	for _, marker := range displaced {
		_ = s.writePinLocked(marker.file, marker.pin.Fingerprint, marker.pin.JobID, marker.pin.Role)
	}
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

// ReleaseOrphanPendingPins drops pending-lease pins that have no durable index
// entry and no active lease to re-index them. Callers reconcile active leases
// with ReconcilePendingPins first, so a pin that survives here belongs to no
// awaiting job: it predates index-first ordering (a crash between the pin write
// and the index write) or its job already left, and it is invisible to
// PendingJobs while exempt from retention sweeps. Pins with an index entry are
// left alone; their jobs are released through PendingJobs/ReleaseJob once
// terminal. The store lock is held throughout, so no concurrent
// pinPendingLocked can be mid-flight while orphans are collected.
func (s *Store) ReleaseOrphanPendingPins(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(pendingIndexPath(s.root))
	index := map[string]string{}
	if err == nil {
		if err := json.Unmarshal(data, &index); err != nil {
			return 0, fmt.Errorf("decoding capture pending index: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading capture directory: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		info, err := entry.Info()
		if err != nil {
			return removed, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, err := scanHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name())
		if err != nil {
			return removed, err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			pin, ok := readPin(file.Path)
			if !ok || !strings.HasPrefix(pin.Fingerprint, "pending:") {
				continue
			}
			if _, ok := index[pin.Fingerprint]; ok {
				continue
			}
			if err := os.Remove(pinPath(file.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return removed, err
			}
			removed++
			// The file stays ordinary: drop its lease link too, so a later
			// retry with the same job ID cannot resurrect it through
			// ReconcilePendingPins. Best-effort like displacement clearing.
			s.clearPendingLinkLocked(file, pin.Fingerprint)
		}
	}
	return removed, nil
}

// ReconcilePendingPins restores interrupted provisional-lease writes for jobs
// that are still active, before retention or orphan cleanup can evict their
// evidence. For each active job it re-indexes a pin whose index write never
// landed and restores pins for captures whose pin write never landed (found
// through the metadata lease link written with the capture bytes). Jobs absent
// from active are untouched: their index entries are released through
// PendingJobs/ReleaseJob and their indexless pins through
// ReleaseOrphanPendingPins, so genuinely orphan state is still collected. The
// store lock is held throughout, so no concurrent pinPendingLocked can be
// mid-flight while leases are reconciled.
//
// It reports reindexed index entries and restored pin sidecars. A corrupt
// pending index fails instead of guessing; a missing index or no active jobs
// is a no-op.
func (s *Store) ReconcilePendingPins(ctx context.Context, activeJobIDs []string) (reindexed, restored int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	active := make(map[string]string, len(activeJobIDs))
	for _, jobID := range activeJobIDs {
		trimmed := strings.TrimSpace(jobID)
		if trimmed == "" {
			continue
		}
		active[pendingFingerprint(trimmed)] = trimmed
	}
	if len(active) == 0 {
		return 0, 0, nil
	}
	index, err := s.readPendingIndexLocked()
	if err != nil {
		return 0, 0, err
	}
	type leaseFile struct {
		file     captureFile
		pinned   bool
		pin      capturePin
		metadata captureMetadata
	}
	pinsByFingerprint := make(map[string]int)
	byFingerprint := make(map[string][]leaseFile)
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return 0, 0, fmt.Errorf("reading capture directory: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return reindexed, restored, err
		}
		info, err := entry.Info()
		if err != nil {
			return reindexed, restored, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, err := scanHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name())
		if err != nil {
			return reindexed, restored, err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return reindexed, restored, err
			}
			pin, pinned := readPin(file.Path)
			metadata, _ := readMetadata(file.metadataPath)
			if pinned && strings.HasPrefix(pin.Fingerprint, "pending:") {
				pinsByFingerprint[pin.Fingerprint]++
			}
			fingerprints := map[string]bool{}
			if pinned && strings.HasPrefix(pin.Fingerprint, "pending:") {
				fingerprints[pin.Fingerprint] = true
			}
			if strings.TrimSpace(metadata.PendingJobID) != "" {
				fingerprints[pendingFingerprint(metadata.PendingJobID)] = true
			}
			if pin.JobID != "" && strings.HasPrefix(pin.Fingerprint, "pending:") {
				fingerprints[pendingFingerprint(pin.JobID)] = true
			}
			for fingerprint := range fingerprints {
				byFingerprint[fingerprint] = append(byFingerprint[fingerprint], leaseFile{file: file, pinned: pinned, pin: pin, metadata: metadata})
			}
		}
	}
	indexChanged := false
	for fingerprint, jobID := range active {
		if err := ctx.Err(); err != nil {
			return reindexed, restored, err
		}
		if _, ok := index[fingerprint]; !ok && len(byFingerprint[fingerprint]) > 0 {
			index[fingerprint] = jobID
			indexChanged = true
			reindexed++
		}
		// Restore every unpinned capture that still links to this lease, in
		// timestamp order. A displaced intermediate lost its link when its
		// latest pin moved, so it is not a candidate; a crash victim kept its
		// link and comes back with the role pendingRoleLocked computes now
		// (first when no first pin exists, otherwise latest, displacing the
		// prior latest exactly like the original write would have).
		var unpinned []captureFile
		for _, candidate := range byFingerprint[fingerprint] {
			if candidate.pinned {
				continue
			}
			if strings.TrimSpace(candidate.metadata.PendingJobID) == "" {
				continue
			}
			if pendingFingerprint(candidate.metadata.PendingJobID) != fingerprint {
				continue
			}
			unpinned = append(unpinned, candidate.file)
		}
		if len(unpinned) == 0 {
			continue
		}
		sort.Slice(unpinned, func(i, j int) bool {
			if unpinned[i].Timestamp.Equal(unpinned[j].Timestamp) {
				return unpinned[i].Path < unpinned[j].Path
			}
			return unpinned[i].Timestamp.Before(unpinned[j].Timestamp)
		})
		for _, file := range unpinned {
			if err := ctx.Err(); err != nil {
				return reindexed, restored, err
			}
			role, err := s.pendingRoleLocked(ctx, fingerprint)
			if err != nil {
				return reindexed, restored, err
			}
			// The capture bytes prove the file still IS the capture the
			// metadata describes; restoring a pin for modified bytes would
			// protect evidence that no longer matches its recorded hash.
			metadata, err := readMetadata(file.metadataPath)
			if err != nil || strings.TrimSpace(metadata.SHA256) == "" {
				continue
			}
			data, err := os.ReadFile(file.Path)
			if err != nil {
				continue
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != metadata.SHA256 {
				continue
			}
			if err := s.writePinLocked(file, fingerprint, jobID, role); err != nil {
				return reindexed, restored, err
			}
			restored++
			pinsByFingerprint[fingerprint]++
			if role == PinLatest {
				if err := s.removeIncidentRoleLocked(ctx, fingerprint, PinLatest, file.Path); err != nil {
					s.discardPinLocked(file.Path)
					restored--
					pinsByFingerprint[fingerprint]--
					return reindexed, restored, err
				}
			}
		}
	}
	if indexChanged {
		if err := s.writePendingIndexLocked(index); err != nil {
			return 0, 0, err
		}
	}
	return reindexed, restored, nil
}

// PendingLeaseCandidates lists every job ID referenced by a provisional-lease
// trace on disk: pending index entries, pending pin sidecars, and capture
// metadata lease links. The poll uses it to complete the active set when the
// awaiting-job page is truncated: a pin-first crash leaves no index entry, so
// a job past the page cap would otherwise miss reconciliation and lose its pin
// to orphan cleanup. A corrupt index fails instead of guessing. Pins without
// any resolvable job ID (legacy pins with no JobID and no metadata link) are
// skipped; they stay genuine orphans.
func (s *Store) PendingLeaseCandidates(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make(map[string]struct{})
	data, err := os.ReadFile(pendingIndexPath(s.root))
	if err == nil {
		var index map[string]string
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, fmt.Errorf("decoding capture pending index: %w", err)
		}
		for _, jobID := range index {
			if trimmed := strings.TrimSpace(jobID); trimmed != "" {
				set[trimmed] = struct{}{}
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return nil, fmt.Errorf("reading capture directory: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, err := scanHost(ctx, filepath.Join(s.root, entry.Name()), entry.Name())
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			pin, ok := readPin(file.Path)
			if ok && strings.HasPrefix(pin.Fingerprint, "pending:") && strings.TrimSpace(pin.JobID) != "" {
				set[strings.TrimSpace(pin.JobID)] = struct{}{}
			}
			if metadata, err := readMetadata(file.metadataPath); err == nil {
				if trimmed := strings.TrimSpace(metadata.PendingJobID); trimmed != "" {
					set[trimmed] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for jobID := range set {
		out = append(out, jobID)
	}
	sort.Strings(out)
	return out, nil
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
	// PendingJobID links a pre-outcome capture to its provisional lease. It is
	// written with the capture bytes before the pending index and pin sidecars,
	// so a crash between those two writes still leaves a durable link that the
	// reconciler uses to restore the missing sidecar. Empty for observed
	// captures and legacy captures written before this link existed.
	PendingJobID string `json:"pending_job_id,omitempty"`
}

type capturePin struct {
	Fingerprint string  `json:"fingerprint"`
	Role        PinRole `json:"role"`
	// JobID recovers the pending index entry when the index write never landed.
	// Old pins carry only the opaque fingerprint; the reconciler falls back to
	// the capture metadata link in that case.
	JobID string `json:"job_id,omitempty"`
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
			// A released lease must not resurrect: a retry with the same job
			// ID would otherwise restore these captures through their
			// metadata lease link. Read failures are ignored (a missing or
			// corrupt sidecar carries no link to resurrect); write failures
			// abort so the caller retries instead of leaving a stale link.
			metadata, err := readMetadata(file.metadataPath)
			if err != nil || strings.TrimSpace(metadata.PendingJobID) == "" {
				continue
			}
			if pendingFingerprint(metadata.PendingJobID) != fingerprint {
				continue
			}
			metadata.PendingJobID = ""
			encoded, err := json.Marshal(metadata)
			if err != nil {
				return fmt.Errorf("encoding capture metadata: %w", err)
			}
			if err := writeAtomically(filepath.Dir(file.metadataPath), file.metadataPath, encoded); err != nil {
				return err
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
	// An index entry without its pin sidecar is an interrupted write for a
	// live lease, not an ordinary capture. The metadata link keeps it exempt
	// from retention until ReconcilePendingPins restores the pin (or
	// ReleaseJob drops the lease). Without this, the next store for the host
	// would prune the very evidence the lease exists to protect.
	index, err := s.readPendingIndexLocked()
	if err != nil {
		return err
	}

	cutoff := s.now().UTC().Add(-s.retention.MaxAge)
	kept := files[:0]
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.isLeaseExemptLocked(file, index) {
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
			if !s.isLeaseExemptLocked(file, index) {
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

// readPendingIndexLocked returns the durable lease map. A missing index means
// no provisional leases; a corrupt one fails retention rather than silently
// evicting leased evidence. Callers hold s.mu.
func (s *Store) readPendingIndexLocked() (map[string]string, error) {
	data, err := os.ReadFile(pendingIndexPath(s.root))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	index := map[string]string{}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("decoding capture pending index: %w", err)
	}
	return index, nil
}

// isLeaseExemptLocked reports whether retention must keep a capture: any pin
// sidecar, or a metadata lease link whose fingerprint is still indexed. The
// second clause covers the crash window after .pending.json is durable but
// before the pin sidecar lands. Metadata or pin read failures mean no
// exemption; retention then treats the file as ordinary. Callers hold s.mu.
func (s *Store) isLeaseExemptLocked(file captureFile, index map[string]string) bool {
	if pinned, _ := readPin(file.Path); pinned.Fingerprint != "" {
		return true
	}
	metadata, err := readMetadata(file.metadataPath)
	if err != nil || strings.TrimSpace(metadata.PendingJobID) == "" {
		return false
	}
	_, ok := index[pendingFingerprint(metadata.PendingJobID)]
	return ok
}
func (s *Store) nextPath(ctx context.Context, hostDir, scenario string, timestamp time.Time) (string, time.Time, error) {
	for candidate := timestamp; ; candidate = candidate.Add(time.Nanosecond) {
		if err := ctx.Err(); err != nil {
			return "", time.Time{}, err
		}
		base := candidate.Format(captureTimestampLayout) + "-" + scenario
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
	timestamp, err := time.Parse(captureTimestampLayout, base[:separator+1])
	if err != nil {
		// Captures already stored under RFC3339 names remain readable and
		// participate in the same retention and pinning rules.
		timestamp, err = time.Parse(time.RFC3339Nano, base[:separator+1])
	}
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
