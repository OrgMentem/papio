// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package captures keeps sanitized diagnostic pages with their provider context
// under papio's data directory, rather than leaking fixtures into Downloads.
package captures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	capturesDir = "captures"
	htmlExt     = ".html"
	metadataExt = ".json"
	pinExt      = ".pin.json"
)

// PinRole identifies why a capture is retained for an open incident.
type PinRole string

const (
	PinFirstDecisive PinRole = "first_decisive"
	PinLatest        PinRole = "latest"
)

// Retention bounds diagnostics so a provider repeatedly changing its page
// cannot silently fill the user's data volume.
type Retention struct {
	MaxPerHost int
	MaxAge     time.Duration
}

// Capture preserves provider context that would otherwise be lost with HTML alone.
type Capture struct {
	Host           string    `json:"host"`
	Scenario       string    `json:"scenario"`
	AdapterID      string    `json:"adapter_id,omitempty"`
	AdapterVersion string    `json:"adapter_version,omitempty"`
	Timestamp      time.Time `json:"timestamp"`
	Path           string    `json:"path"`
	Size           int64     `json:"size"`
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
// an unbounded volume of stale pages.
func (s *Store) Store(ctx context.Context, host, scenario, adapterID, adapterVersion string, html []byte) (string, error) {
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

	hostDir := filepath.Join(s.root, safeHost(host))
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		return "", fmt.Errorf("creating capture directory: %w", err)
	}
	path, _, err := s.nextPath(ctx, hostDir, scenario, s.now().UTC())
	if err != nil {
		return "", err
	}
	metadata, err := json.Marshal(captureMetadata{AdapterID: adapterID, AdapterVersion: adapterVersion})
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
	if err := s.pruneHost(ctx, hostDir, safeHost(host)); err != nil {
		return path, err
	}
	return path, nil
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

	hostDir := filepath.Join(s.root, safeHost(host))
	files, err := scanHost(ctx, hostDir, safeHost(host))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	if err := os.RemoveAll(hostDir); err != nil {
		return 0, fmt.Errorf("purging captures for %s: %w", safeHost(host), err)
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
// incident. A capture may serve both roles when the paths are equal.
func (s *Store) PinIncident(ctx context.Context, fingerprint, firstPath, latestPath string) error {
	if err := s.Pin(ctx, firstPath, fingerprint, PinFirstDecisive); err != nil {
		return err
	}
	if filepath.Clean(latestPath) == filepath.Clean(firstPath) {
		return nil
	}
	return s.Pin(ctx, latestPath, fingerprint, PinLatest)
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
	AdapterID      string `json:"adapter_id,omitempty"`
	AdapterVersion string `json:"adapter_version,omitempty"`
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
			file.AdapterID = metadata.AdapterID
			file.AdapterVersion = metadata.AdapterVersion
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

func safeHost(host string) string {
	var segment strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(host)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			segment.WriteRune(r)
		default:
			segment.WriteByte('-')
		}
	}
	result := strings.Trim(segment.String(), ".-")
	if result == "" || result == "." || result == ".." {
		return "host"
	}
	if len(result) > 253 {
		return result[:253]
	}
	return result
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
