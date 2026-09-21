// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

var (
	// ErrConfigChanged means a staged edit was based on different file bytes.
	ErrConfigChanged = errors.New("configuration changed; reload it before saving")
	// ErrConfigBusy means another Papio writer holds the publication lock.
	ErrConfigBusy = errors.New("configuration is being saved by another process; retry")
)

// Snapshot is a content fingerprint for a particular config path. It contains
// no plaintext secrets. Only ReadSnapshot can produce a usable snapshot.
// Detecting edits does not claim to lock out non-cooperating external editors.
type Snapshot struct {
	path   string
	exists bool
	digest [sha256.Size]byte
}

// ReadSnapshot observes current bytes (or absence) without parsing or modifying
// them. SaveIfUnchanged later compares the same observation under the save lock.
func ReadSnapshot(path string) (Snapshot, error) {
	path, err := configTarget(path)
	if err != nil {
		return Snapshot{}, err
	}
	return readSnapshot(path)
}

func readSnapshot(path string) (Snapshot, error) {
	s := Snapshot{path: path}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return Snapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return Snapshot{}, errors.New("config destination must be a regular file, not a symlink or directory")
	}
	file, err := os.Open(path)
	if err != nil {
		return Snapshot{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return Snapshot{}, err
	}
	if !os.SameFile(info, opened) {
		return Snapshot{}, ErrConfigChanged
	}
	// Read through the checked file descriptor, not a second path lookup.
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return Snapshot{}, err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return Snapshot{}, ErrConfigChanged
	}
	s.exists = true
	copy(s.digest[:], h.Sum(nil))
	return s, nil
}

// Save validates and atomically publishes a config, serialized with all other
// Papio config writers. Temporary/final file modes retain their prior behavior.
// Use SaveIfUnchanged for a read-modify-write operation such as migration.
func Save(cfg Config, path string) error {
	return save(cfg, path, nil, nil)
}

// SaveIfUnchanged publishes only if current bytes/existence still match the
// snapshot. No plaintext backup is created, and failed publication keeps the
// prior config. An external edit observed while staging is checked again just
// before rename; external programs are not required to honor Papio's lock.
func SaveIfUnchanged(cfg Config, path string, snapshot Snapshot) error {
	return save(cfg, path, &snapshot, nil)
}

// beforePublish is an internal test seam for interruption and edit oracles.
func save(cfg Config, path string, expected *Snapshot, beforePublish func() error) error {
	cfg = cfg.clone()
	if _, err := cfg.RequireAccessMode(); err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	path, err = configTarget(path)
	if err != nil {
		return err
	}
	if expected != nil && (expected.path == "" || expected.path != path) {
		return ErrConfigChanged
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	lock, err := lockConfig(filepath.Join(dir, "."+filepath.Base(path)+".lock"))
	if err != nil {
		return err
	}
	defer lock.Close() // OS releases the lock even if the process exits.
	initial, err := readSnapshot(path)
	if err != nil {
		return err
	}
	if expected != nil && initial != *expected {
		return ErrConfigChanged
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			return err
		}
	}
	current, err := readSnapshot(path)
	if err != nil {
		return err
	}
	if current != initial {
		return ErrConfigChanged
	}
	return os.Rename(name, path)
}

// configTarget folds relative paths and symlinked parent-directory aliases onto
// one lock path, while the final component must itself remain a regular file.
func configTarget(path string) (string, error) {
	if path == "" {
		path = filepath.Join(Dir(), "config.toml")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(absolute)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return normalizeConfigTarget(filepath.Join(resolved, filepath.Base(absolute))), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", err
		}
		missing = append(missing, filepath.Base(dir))
		dir = parent
	}
}
