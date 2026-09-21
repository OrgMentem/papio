// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSaveIfUnchangedRejectsStaleEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	cfg := credentialConfig()
	missing, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveIfUnchanged(cfg, path, missing); err != nil {
		t.Fatal(err)
	}
	if err := SaveIfUnchanged(cfg, path, missing); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("creation raced snapshot: %v", err)
	}
	snapshot, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(path)
	// Even comments count: they are an external edit, not a semantic no-op.
	external := append(append([]byte{}, old...), []byte("\n# operator edit\n")...)
	if err := os.WriteFile(path, external, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveIfUnchanged(cfg, path, snapshot); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("external bytes overwritten: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, external) {
		t.Fatal("external edit was lost")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := SaveIfUnchanged(cfg, path, snapshot); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("deleted config recreated: %v", err)
	}
	if err := SaveIfUnchanged(cfg, path, Snapshot{}); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("zero snapshot accepted: %v", err)
	}
	if err := SaveIfUnchanged(cfg, filepath.Join(t.TempDir(), "different.toml"), missing); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("snapshot applied to another target: %v", err)
	}
}

func TestSavePublicationInterruptionPreservesPriorBytes(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{true: "existing", false: "absent"}[existing], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			cfg := credentialConfig()
			if existing {
				if err := Save(cfg, path); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(path)
			snapshot, err := ReadSnapshot(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Email = "replacement@example.test"
			interrupted := errors.New("publication interrupted")
			if err := save(cfg, path, &snapshot, func() error { return interrupted }); !errors.Is(err, interrupted) {
				t.Fatalf("interruption: %v", err)
			}
			after, err := os.ReadFile(path)
			if existing && (err != nil || !bytes.Equal(before, after)) || !existing && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("interruption changed prior config")
			}
			assertNoConfigTemporaryFiles(t, filepath.Dir(path))
			if err := SaveIfUnchanged(cfg, path, snapshot); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestSaveDetectsExternalEditDuringStaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := credentialConfig()
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	external := []byte("access_mode='assisted'\n# concurrent edit during staging\n")
	err = save(cfg, path, &snapshot, func() error { return os.WriteFile(path, external, 0o600) })
	if !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("edit not detected: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(external, got) {
		t.Fatal("staged config replaced concurrent edit")
	}
	assertNoConfigTemporaryFiles(t, filepath.Dir(path))
}

func assertNoConfigTemporaryFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".config-") || strings.HasSuffix(entry.Name(), ".bak") {
			t.Fatal("save left plaintext staging/backup file")
		}
	}
}

func TestSaveRejectsNonregularDestination(t *testing.T) {
	dir := t.TempDir()
	cfg := credentialConfig()
	if _, err := ReadSnapshot(dir); err == nil {
		t.Fatal("snapshot accepted directory")
	}
	if err := Save(cfg, dir); err == nil {
		t.Fatal("save accepted directory")
	}
	path := filepath.Join(dir, "config.toml")
	target := filepath.Join(dir, "target.toml")
	old := []byte("must remain untouched")
	if err := os.WriteFile(target, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink privilege is unavailable")
		}
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(path); err == nil {
		t.Fatal("snapshot followed symlink")
	}
	if err := Save(cfg, path); err == nil {
		t.Fatal("save replaced symlink")
	}
	got, _ := os.ReadFile(target)
	if !bytes.Equal(got, old) {
		t.Fatal("symlink target modified")
	}
}

func TestConfigLockProcessHelper(t *testing.T) {
	if os.Getenv("PAPIO_CONFIG_LOCK_HELPER") != "1" {
		return
	}
	file, err := lockConfig(os.Getenv("PAPIO_CONFIG_LOCK_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	_, _ = os.Stdout.WriteString("locked\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestSaveUsesCrossProcessLockAndRecoversAfterExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := credentialConfig()
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestConfigLockProcessHelper$")
	cmd.Env = append(os.Environ(), "PAPIO_CONFIG_LOCK_HELPER=1", "PAPIO_CONFIG_LOCK_PATH="+filepath.Join(filepath.Dir(snapshot.path), ".config.toml.lock"))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "locked\n" {
			t.Fatalf("child did not lock: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child lock timed out")
	}
	start := time.Now()
	if err := Save(cfg, path); !errors.Is(err, ErrConfigBusy) {
		t.Fatalf("Save skipped lock: %v", err)
	}
	if err := SaveIfUnchanged(cfg, path, snapshot); !errors.Is(err, ErrConfigBusy) {
		t.Fatalf("conditional save skipped lock: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("lock did not fail fast")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true
	if err := SaveIfUnchanged(cfg, path, snapshot); err != nil {
		t.Fatalf("OS did not release crashed writer lock: %v", err)
	}
}

func TestConfigSnapshotResolvesParentAliases(t *testing.T) {
	dir := t.TempDir()
	actual := filepath.Join(dir, "actual")
	if err := os.Mkdir(actual, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink privilege is unavailable")
		}
		t.Fatal(err)
	}
	path := filepath.Join(actual, "nested", "config.toml")
	snapshot, err := ReadSnapshot(filepath.Join(alias, "nested", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveIfUnchanged(credentialConfig(), path, snapshot); err != nil {
		t.Fatalf("alias snapshot disagrees: %v", err)
	}
}
