// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/config"
)

// runCLI executes the root command with args, returning stdout, stderr, and err.
func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	root := NewRoot(&stdout, &stderr)
	root.SetArgs(args)
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

// writeTestConfig writes a config.toml under a temp PAPIO_CONFIG_DIR and returns
// the config dir and the resolved current-executable path (post EvalSymlinks).
func writeTestConfig(t *testing.T, extensionID string) (string, string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("PAPIO_CONFIG_DIR", configDir)
	cfg := config.Default()
	cfg.AccessMode = config.ModeMaximal
	cfg.Browser.ExtensionID = extensionID
	if err := config.Save(cfg, ""); err != nil {
		t.Fatalf("save config: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	if resolved, rErr := filepath.EvalSymlinks(exe); rErr == nil {
		exe = resolved
	}
	return configDir, exe
}

func TestNativeHostInstallWritesManifestAndSymlink(t *testing.T) {
	extID := strings.Repeat("a", 32) // valid 32-char a-p extension ID
	configDir, exe := writeTestConfig(t, extID)
	manifestDir := t.TempDir()

	_, stderr, err := runCLI(t, "--json", "native-host", "install", "--manifest-dir", manifestDir)
	if err != nil {
		t.Fatalf("install: %v (%s)", err, stderr)
	}

	// Manifest decodes with the expected name, path, and allowed_origins.
	manifestPath := filepath.Join(manifestDir, "com.orgmentem.papio.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest nativeHostManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v (%q)", err, string(data))
	}
	symlinkPath := filepath.Join(configDir, "bin", "papio-native-host")
	if manifest.Name != "com.orgmentem.papio" {
		t.Fatalf("manifest name = %q", manifest.Name)
	}
	if manifest.Type != "stdio" {
		t.Fatalf("manifest type = %q", manifest.Type)
	}
	if manifest.Path != symlinkPath {
		t.Fatalf("manifest path = %q, want %q", manifest.Path, symlinkPath)
	}
	wantOrigins := []string{"chrome-extension://" + extID + "/"}
	if len(manifest.AllowedOrigins) != 1 || manifest.AllowedOrigins[0] != wantOrigins[0] {
		t.Fatalf("allowed_origins = %v, want %v", manifest.AllowedOrigins, wantOrigins)
	}

	// Manifest file is 0644.
	info, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("manifest mode = %v", info.Mode().Perm())
	}

	// Symlink points at the resolved test binary.
	target, err := os.Readlink(symlinkPath)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != exe {
		t.Fatalf("symlink target = %q, want %q", target, exe)
	}

	// A second install is idempotent: no error, same manifest.
	if _, stderr, err := runCLI(t, "native-host", "install", "--manifest-dir", manifestDir); err != nil {
		t.Fatalf("second install: %v (%s)", err, stderr)
	}
	data2, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("manifest changed on re-install:\n%s\n---\n%s", data, data2)
	}
}

func TestNativeHostStatusAndUninstall(t *testing.T) {
	extID := strings.Repeat("b", 32)
	configDir, _ := writeTestConfig(t, extID)
	manifestDir := t.TempDir()
	manifestPath := filepath.Join(manifestDir, "com.orgmentem.papio.json")
	symlinkPath := filepath.Join(configDir, "bin", "papio-native-host")

	// Status before install: absent.
	before := statusJSON(t, manifestDir)
	if before["manifest_present"] != false {
		t.Fatalf("pre-install manifest_present = %v", before["manifest_present"])
	}
	if before["extension_id"] != extID {
		t.Fatalf("pre-install extension_id = %v", before["extension_id"])
	}
	if before["target_exists"] != false {
		t.Fatalf("pre-install target_exists = %v", before["target_exists"])
	}

	if _, stderr, err := runCLI(t, "native-host", "install", "--manifest-dir", manifestDir); err != nil {
		t.Fatalf("install: %v (%s)", err, stderr)
	}

	// Status after install: present, target exists.
	after := statusJSON(t, manifestDir)
	if after["manifest_present"] != true {
		t.Fatalf("post-install manifest_present = %v", after["manifest_present"])
	}
	if after["target_exists"] != true {
		t.Fatalf("post-install target_exists = %v", after["target_exists"])
	}
	if after["symlink_path"] != symlinkPath {
		t.Fatalf("post-install symlink_path = %v", after["symlink_path"])
	}

	// Uninstall removes both artifacts.
	if _, stderr, err := runCLI(t, "native-host", "uninstall", "--manifest-dir", manifestDir); err != nil {
		t.Fatalf("uninstall: %v (%s)", err, stderr)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("manifest still present after uninstall: %v", err)
	}
	if _, err := os.Lstat(symlinkPath); !os.IsNotExist(err) {
		t.Fatalf("symlink still present after uninstall: %v", err)
	}

	// Repeat uninstall is idempotent.
	if _, stderr, err := runCLI(t, "native-host", "uninstall", "--manifest-dir", manifestDir); err != nil {
		t.Fatalf("second uninstall: %v (%s)", err, stderr)
	}

	// Status after uninstall: absent again.
	gone := statusJSON(t, manifestDir)
	if gone["manifest_present"] != false {
		t.Fatalf("post-uninstall manifest_present = %v", gone["manifest_present"])
	}
	if gone["target_exists"] != false {
		t.Fatalf("post-uninstall target_exists = %v", gone["target_exists"])
	}
}

func TestNativeHostInstallRequiresExtensionID(t *testing.T) {
	writeTestConfig(t, "") // no extension_id
	manifestDir := t.TempDir()

	_, _, err := runCLI(t, "native-host", "install", "--manifest-dir", manifestDir)
	if err == nil {
		t.Fatal("install without extension_id succeeded")
	}
	if !strings.Contains(err.Error(), "extension_id") {
		t.Fatalf("error does not mention extension_id: %v", err)
	}
	// Nothing should have been written.
	if _, statErr := os.Stat(filepath.Join(manifestDir, "com.orgmentem.papio.json")); !os.IsNotExist(statErr) {
		t.Fatalf("manifest written despite missing extension_id: %v", statErr)
	}
}

func statusJSON(t *testing.T, manifestDir string) map[string]any {
	t.Helper()
	stdout, stderr, err := runCLI(t, "--json", "native-host", "status", "--manifest-dir", manifestDir)
	if err != nil {
		t.Fatalf("status: %v (%s)", err, stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode status: %v (%q)", err, stdout)
	}
	return out
}
