// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"bytes"
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
	// ErrPublicationUncertain means the new file replaced the old one but its
	// directory entry could not be flushed to stable storage. The new content
	// is visible now and a power loss may still bring the old content back.
	// Callers that hold recovery state for the new content must keep it.
	ErrPublicationUncertain = errors.New("configuration was replaced, but could not be confirmed on stable storage")
)

// syncConfigDir is the seam for the directory flush after the rename.
var syncConfigDir = SyncDir

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
// It backs up an existing file like SaveWithBackup, discarding the report.
func Save(cfg Config, path string) error {
	_, err := saveBackup(cfg, path, nil, nil)
	return err
}

// SaveWithBackup is Save that also reports the backup path ("" when none).
func SaveWithBackup(cfg Config, path string) (string, error) {
	return saveBackup(cfg, path, nil, nil)
}

// SaveIfUnchanged publishes only if current bytes/existence still match the
// snapshot. Failed publication keeps the prior config. An external edit
// observed while staging is checked again just before rename; external
// programs are not required to honor Papio's lock.
//
// The save re-encodes the config from the struct, so comments and layout in an
// existing file are lost. Before replacing an existing file whose bytes differ,
// the previous bytes are written to BackupPath(path) (mode 0600, replacing any
// older backup) through a temp file and rename. If that backup cannot be
// written, the save fails and the config is untouched. The backup is
// plaintext: it may hold literal legacy secrets. Mode 0600 matches the
// original file, so it adds no exposure to other local users, but it is a
// second copy that the user must delete when they migrate secrets. A fresh
// file, or an unchanged one, creates no backup.
func SaveIfUnchanged(cfg Config, path string, snapshot Snapshot) error {
	_, err := saveBackup(cfg, path, &snapshot, nil)
	return err
}

// SaveIfUnchangedWithBackup is SaveIfUnchanged that also reports the backup
// path ("" when none was written).
func SaveIfUnchangedWithBackup(cfg Config, path string, snapshot Snapshot) (string, error) {
	return saveBackup(cfg, path, &snapshot, nil)
}

// BackupPath is where a save keeps the previous bytes of the config at path.
func BackupPath(path string) string { return path + ".bak" }

// writeBackup atomically writes old to dest with mode 0600.
func writeBackup(dir, dest string, old []byte) error {
	tmp, err := os.CreateTemp(dir, ".config-bak-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(old); err != nil {
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
	return os.Rename(name, dest)
}

// beforePublish is an internal test seam for interruption and edit oracles.
func save(cfg Config, path string, expected *Snapshot, beforePublish func() error) error {
	_, err := saveBackup(cfg, path, expected, beforePublish)
	return err
}

func saveBackup(cfg Config, path string, expected *Snapshot, beforePublish func() error) (string, error) {
	cfg = cfg.clone()
	if _, err := cfg.RequireAccessMode(); err != nil {
		return "", err
	}
	if err := cfg.validate(); err != nil {
		return "", err
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("encoding config: %w", err)
	}
	path, err = configTarget(path)
	if err != nil {
		return "", err
	}
	if expected != nil && (expected.path == "" || expected.path != path) {
		return "", ErrConfigChanged
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	lock, err := lockConfig(filepath.Join(dir, "."+filepath.Base(path)+".lock"))
	if err != nil {
		return "", err
	}
	defer lock.Close() // OS releases the lock even if the process exits.
	initial, err := readSnapshot(path)
	if err != nil {
		return "", err
	}
	if expected != nil && initial != *expected {
		return "", ErrConfigChanged
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			return "", err
		}
	}
	current, err := readSnapshot(path)
	if err != nil {
		return "", err
	}
	if current != initial {
		return "", ErrConfigChanged
	}
	backup := ""
	if old, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(old, data) {
			dest := BackupPath(path)
			if err := writeBackup(dir, dest, old); err != nil {
				return "", fmt.Errorf("saving previous config to %s: %w", dest, err)
			}
			backup = dest
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(name, path); err != nil {
		return backup, err
	}
	// The temporary file's data is durable, but the rename lives in the
	// directory. Until the directory is flushed a power loss can restore the
	// old config after this function has reported success.
	if err := syncConfigDir(dir); err != nil {
		return backup, fmt.Errorf("%w: %w", ErrPublicationUncertain, err)
	}
	return backup, nil
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
