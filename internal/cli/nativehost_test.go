// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/nativehost"
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
func writeTestConfig(t *testing.T, extensionID, firefoxExtensionID string) (string, string) {
	t.Helper()
	configDir := t.TempDir()
	t.Setenv("PAPIO_CONFIG_DIR", configDir)
	cfg := config.Default()
	cfg.AccessMode = config.ModeDelegated
	cfg.Browser.ExtensionID = extensionID
	cfg.Browser.FirefoxExtensionID = firefoxExtensionID
	if err := config.Save(cfg, ""); err != nil {
		t.Fatalf("save config: %v", err)
	}
	// Install pins the host symlink to the invocation path (not the fully
	// symlink-resolved path), so the expected target is os.Executable() as-is.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	return configDir, exe
}

func TestNativeHostInstallWritesBrowserManifestsAndSymlink(t *testing.T) {
	extID := strings.Repeat("a", 32) // valid 32-char a-p extension ID
	const firefoxID = "papio@orgmentem.com"
	_, exe := writeTestConfig(t, extID, firefoxID)
	manifestDir := t.TempDir()
	firefoxManifestDir := t.TempDir()

	_, stderr, err := runCLI(t, "--json", "native-host", "install", "--manifest-dir", manifestDir, "--firefox-manifest-dir", firefoxManifestDir)
	if err != nil {
		t.Fatalf("install: %v (%s)", err, stderr)
	}

	manifestPath := filepath.Join(manifestDir, nativeHostManifestName+".json")
	firefoxManifestPath := filepath.Join(firefoxManifestDir, nativeHostManifestName+".json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read Chrome manifest: %v", err)
	}
	firefoxData, err := os.ReadFile(firefoxManifestPath)
	if err != nil {
		t.Fatalf("read Firefox manifest: %v", err)
	}
	var chromeKeys, firefoxKeys map[string]json.RawMessage
	if err := json.Unmarshal(data, &chromeKeys); err != nil {
		t.Fatalf("decode Chrome manifest keys: %v", err)
	}
	if err := json.Unmarshal(firefoxData, &firefoxKeys); err != nil {
		t.Fatalf("decode Firefox manifest keys: %v", err)
	}
	if _, ok := chromeKeys["allowed_extensions"]; ok {
		t.Fatal("Chrome manifest contains allowed_extensions")
	}
	if _, ok := firefoxKeys["allowed_origins"]; ok {
		t.Fatal("Firefox manifest contains allowed_origins")
	}

	var chromeManifest, firefoxManifest nativeHostManifest
	if err := json.Unmarshal(data, &chromeManifest); err != nil {
		t.Fatalf("decode Chrome manifest: %v (%q)", err, string(data))
	}
	if err := json.Unmarshal(firefoxData, &firefoxManifest); err != nil {
		t.Fatalf("decode Firefox manifest: %v (%q)", err, string(firefoxData))
	}
	symlinkPath := nativehost.ExecPath()
	for name, manifest := range map[string]nativeHostManifest{
		"Chrome":  chromeManifest,
		"Firefox": firefoxManifest,
	} {
		if manifest.Name != nativeHostManifestName {
			t.Fatalf("%s manifest name = %q", name, manifest.Name)
		}
		if manifest.Type != "stdio" {
			t.Fatalf("%s manifest type = %q", name, manifest.Type)
		}
		if manifest.Path != symlinkPath {
			t.Fatalf("%s manifest path = %q, want %q", name, manifest.Path, symlinkPath)
		}
	}
	wantOrigins := []string{"chrome-extension://" + extID + "/"}
	if len(chromeManifest.AllowedOrigins) != 1 || chromeManifest.AllowedOrigins[0] != wantOrigins[0] {
		t.Fatalf("Chrome allowed_origins = %v, want %v", chromeManifest.AllowedOrigins, wantOrigins)
	}
	if len(firefoxManifest.AllowedExtensions) != 1 || firefoxManifest.AllowedExtensions[0] != firefoxID {
		t.Fatalf("Firefox allowed_extensions = %v, want [%s]", firefoxManifest.AllowedExtensions, firefoxID)
	}

	// Manifest files are 0644.
	for _, path := range []string{manifestPath, firefoxManifestPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("manifest mode = %v, want 0644", info.Mode().Perm())
		}
	}

	// Symlink points at the resolved test binary.
	target, err := os.Readlink(symlinkPath)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != exe {
		t.Fatalf("symlink target = %q, want %q", target, exe)
	}

	// A second install is idempotent: no error, same manifests.
	if _, stderr, err := runCLI(t, "native-host", "install", "--manifest-dir", manifestDir, "--firefox-manifest-dir", firefoxManifestDir); err != nil {
		t.Fatalf("second install: %v (%s)", err, stderr)
	}
	data2, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	firefoxData2, err := os.ReadFile(firefoxManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, data2) || !bytes.Equal(firefoxData, firefoxData2) {
		t.Fatal("manifest changed on re-install")
	}
}

func TestNativeHostStatusAndUninstall(t *testing.T) {
	extID := strings.Repeat("b", 32)
	const firefoxID = "papio@orgmentem.com"
	writeTestConfig(t, extID, firefoxID)
	manifestDir := t.TempDir()
	firefoxManifestDir := t.TempDir()
	manifestPath := filepath.Join(manifestDir, nativeHostManifestName+".json")
	firefoxManifestPath := filepath.Join(firefoxManifestDir, nativeHostManifestName+".json")
	symlinkPath := nativehost.ExecPath()

	// Status before install: both manifests absent.
	before := statusJSON(t, manifestDir, firefoxManifestDir)
	if before["manifest_present"] != false || before["firefox_manifest_present"] != false {
		t.Fatalf("pre-install manifest presence = Chrome:%v Firefox:%v", before["manifest_present"], before["firefox_manifest_present"])
	}
	if before["extension_id"] != extID || before["firefox_extension_id"] != firefoxID {
		t.Fatalf("pre-install extension IDs = Chrome:%v Firefox:%v", before["extension_id"], before["firefox_extension_id"])
	}
	if before["target_exists"] != false {
		t.Fatalf("pre-install target_exists = %v", before["target_exists"])
	}

	if _, stderr, err := runCLI(t, "native-host", "install", "--manifest-dir", manifestDir, "--firefox-manifest-dir", firefoxManifestDir); err != nil {
		t.Fatalf("install: %v (%s)", err, stderr)
	}

	// Status after install: both manifests present, target exists.
	after := statusJSON(t, manifestDir, firefoxManifestDir)
	if after["manifest_present"] != true || after["firefox_manifest_present"] != true {
		t.Fatalf("post-install manifest presence = Chrome:%v Firefox:%v", after["manifest_present"], after["firefox_manifest_present"])
	}
	if after["firefox_manifest_path"] != firefoxManifestPath {
		t.Fatalf("post-install firefox_manifest_path = %v", after["firefox_manifest_path"])
	}
	if after["target_exists"] != true {
		t.Fatalf("post-install target_exists = %v", after["target_exists"])
	}
	if after["symlink_path"] != symlinkPath {
		t.Fatalf("post-install symlink_path = %v", after["symlink_path"])
	}

	// Uninstall removes both manifests and the symlink.
	if _, stderr, err := runCLI(t, "native-host", "uninstall", "--manifest-dir", manifestDir, "--firefox-manifest-dir", firefoxManifestDir); err != nil {
		t.Fatalf("uninstall: %v (%s)", err, stderr)
	}
	for _, path := range []string{manifestPath, firefoxManifestPath, symlinkPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("artifact still present after uninstall %s: %v", path, err)
		}
	}

	// Repeat uninstall is idempotent.
	if _, stderr, err := runCLI(t, "native-host", "uninstall", "--manifest-dir", manifestDir, "--firefox-manifest-dir", firefoxManifestDir); err != nil {
		t.Fatalf("second uninstall: %v (%s)", err, stderr)
	}

	// Status after uninstall: both manifests absent again.
	gone := statusJSON(t, manifestDir, firefoxManifestDir)
	if gone["manifest_present"] != false || gone["firefox_manifest_present"] != false {
		t.Fatalf("post-uninstall manifest presence = Chrome:%v Firefox:%v", gone["manifest_present"], gone["firefox_manifest_present"])
	}
	if gone["target_exists"] != false {
		t.Fatalf("post-uninstall target_exists = %v", gone["target_exists"])
	}
}

func TestNativeHostInstallWithoutFirefoxWritesChromeOnly(t *testing.T) {
	extID := strings.Repeat("c", 32)
	writeTestConfig(t, extID, "")
	manifestDir := t.TempDir()
	firefoxManifestDir := t.TempDir()

	stdout, stderr, err := runCLI(t, "--json", "native-host", "install", "--manifest-dir", manifestDir, "--firefox-manifest-dir", firefoxManifestDir)
	if err != nil {
		t.Fatalf("install: %v (%s)", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(manifestDir, nativeHostManifestName+".json")); err != nil {
		t.Fatalf("Chrome manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(firefoxManifestDir, nativeHostManifestName+".json")); !os.IsNotExist(err) {
		t.Fatalf("Firefox manifest written without Firefox extension ID: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode install result: %v", err)
	}
	if result["firefox_manifest_path"] != "" {
		t.Fatalf("firefox_manifest_path = %v, want empty", result["firefox_manifest_path"])
	}
}

func TestNativeHostInstallRequiresExtensionID(t *testing.T) {
	writeTestConfig(t, "", "papio@orgmentem.com") // no Chrome extension_id
	manifestDir := t.TempDir()
	firefoxManifestDir := t.TempDir()

	_, _, err := runCLI(t, "native-host", "install", "--manifest-dir", manifestDir, "--firefox-manifest-dir", firefoxManifestDir)
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
	if _, statErr := os.Stat(filepath.Join(firefoxManifestDir, nativeHostManifestName+".json")); !os.IsNotExist(statErr) {
		t.Fatalf("Firefox manifest written despite missing Chrome extension_id: %v", statErr)
	}
}

func statusJSON(t *testing.T, manifestDir, firefoxManifestDir string) map[string]any {
	t.Helper()
	stdout, stderr, err := runCLI(t, "--json", "native-host", "status", "--manifest-dir", manifestDir, "--firefox-manifest-dir", firefoxManifestDir)
	if err != nil {
		t.Fatalf("status: %v (%s)", err, stderr)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode status: %v (%q)", err, stdout)
	}
	return out
}

func TestNativeHostInstallMultipleChromiumIDs(t *testing.T) {
	const primary = "abcdefghijklmnopabcdefghijklmnop"
	const secondary = "ponmlkjihgfedcbaponmlkjihgfedcba"
	configDir := t.TempDir()
	t.Setenv("PAPIO_CONFIG_DIR", configDir)
	cfg := config.Default()
	cfg.AccessMode = config.ModeDelegated
	cfg.Browser.ExtensionID = primary
	cfg.Browser.ExtensionIDs = []string{secondary}
	if err := config.Save(cfg, ""); err != nil {
		t.Fatalf("save config: %v", err)
	}
	manifestDir := t.TempDir()
	if _, stderr, err := runCLI(t, "native-host", "install", "--manifest-dir", manifestDir); err != nil {
		t.Fatalf("install: %v (%s)", err, stderr)
	}
	data, err := os.ReadFile(filepath.Join(manifestDir, nativeHostManifestName+".json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m nativeHostManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	want := []string{"chrome-extension://" + primary + "/", "chrome-extension://" + secondary + "/"}
	if strings.Join(m.AllowedOrigins, ",") != strings.Join(want, ",") {
		t.Fatalf("allowed_origins = %v, want %v", m.AllowedOrigins, want)
	}
}

func TestBrowserTargetsDetectsInstalledForks(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("fork detection assertions are unix-path specific")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	targets, err := browserTargets()
	if err != nil {
		t.Fatalf("browserTargets: %v", err)
	}
	edge, ok := findTarget(targets, "edge")
	if !ok {
		t.Fatal("edge is not modeled")
	}
	if edge.present {
		t.Fatal("edge must be absent before its profile dir exists")
	}
	if err := os.MkdirAll(filepath.Dir(edge.dir), 0o755); err != nil {
		t.Fatalf("create edge profile dir: %v", err)
	}

	targets, err = browserTargets()
	if err != nil {
		t.Fatalf("browserTargets: %v", err)
	}
	for _, tc := range []struct {
		id          string
		wantPresent bool
	}{
		{"chrome", true},   // baseline, always present
		{"firefox", true},  // baseline, always present
		{"edge", true},     // profile dir now exists
		{"brave", false},   // no profile dir
		{"vivaldi", false}, // no profile dir
	} {
		target, ok := findTarget(targets, tc.id)
		if !ok {
			t.Fatalf("%s not modeled", tc.id)
		}
		if target.present != tc.wantPresent {
			t.Fatalf("%s present = %t, want %t", tc.id, target.present, tc.wantPresent)
		}
	}
}
