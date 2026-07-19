// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"papio/internal/config"
	"papio/internal/nativehost"
)

// Native-host registration constants. The runtime dispatches to native-host
// mode by executable basename (nativehost.ExecName); browsers launch the host
// through the manifest named nativeHostManifestName.
const (
	nativeHostManifestName = "com.orgmentem.papio"
	nativeHostDescription  = "papio native-messaging host for institutional paper-acquisition handoff"
)

// browserFamily determines a browser's native-messaging manifest format and how
// the daemon's origin check identifies it.
type browserFamily int

const (
	// familyChromium browsers (Chrome, Edge, Vivaldi, Brave, Opera, Chromium)
	// use allowed_origins with chrome-extension://<id>/ and share one allowlist.
	familyChromium browserFamily = iota
	// familyGecko browsers (Firefox and forks) use allowed_extensions with the
	// Gecko add-on id.
	familyGecko
)

// browserTarget is one browser's native-messaging registration site. Each
// Chromium fork and Firefox reads its own location, so papio registers the host
// with every browser the user has installed.
type browserTarget struct {
	id      string // stable id: "chrome","edge","vivaldi","brave","opera","chromium","firefox"
	label   string // human label
	family  browserFamily
	dir     string // directory holding the manifest file
	present bool   // browser appears installed (base profile dir exists)
}

// nativeHostManifest is a browser native-messaging host manifest. Metadata
// only: no secrets cross this file. Each browser receives exactly its own
// allowlist field so its native-messaging parser rejects the other format.
type nativeHostManifest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedOrigins    []string `json:"allowed_origins,omitempty"`
	AllowedExtensions []string `json:"allowed_extensions,omitempty"`
}

// selectManifestTargets resolves which browsers to act on. Explicit directory
// overrides pin Chrome and Firefox only (used by tests and power users);
// otherwise every known browser is returned and the caller filters by presence.
func selectManifestTargets(chromeOverride, firefoxOverride string) ([]browserTarget, error) {
	all, err := browserTargets()
	if err != nil {
		return nil, err
	}
	if chromeOverride == "" && firefoxOverride == "" {
		return all, nil
	}
	chrome, _ := findTarget(all, "chrome")
	firefox, _ := findTarget(all, "firefox")
	if chromeOverride != "" {
		chrome.dir = chromeOverride
	}
	if firefoxOverride != "" {
		firefox.dir = firefoxOverride
	}
	chrome.present = true
	firefox.present = true
	return []browserTarget{chrome, firefox}, nil
}

func findTarget(targets []browserTarget, id string) (browserTarget, bool) {
	for _, t := range targets {
		if t.id == id {
			return t, true
		}
	}
	return browserTarget{}, false
}

func chromiumManifest(hostPath string, chromeIDs []string) nativeHostManifest {
	origins := make([]string, len(chromeIDs))
	for i, id := range chromeIDs {
		origins[i] = "chrome-extension://" + id + "/"
	}
	return nativeHostManifest{
		Name:           nativeHostManifestName,
		Description:    nativeHostDescription,
		Path:           hostPath,
		Type:           "stdio",
		AllowedOrigins: origins,
	}
}

func geckoManifest(hostPath, firefoxID string) nativeHostManifest {
	return nativeHostManifest{
		Name:              nativeHostManifestName,
		Description:       nativeHostDescription,
		Path:              hostPath,
		Type:              "stdio",
		AllowedExtensions: []string{firefoxID},
	}
}

func newNativeHostCommand(opt *options) *cobra.Command {
	command := &cobra.Command{
		Use:         "native-host",
		Short:       "Manage browser native-messaging host registration",
		Annotations: map[string]string{"mcp:hidden": "true"},
	}
	var manifestDir, firefoxManifestDir string

	install := &cobra.Command{
		Use:   "install",
		Short: "Register native-messaging host manifests and the host executable",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			result, err := installNativeHost(cfg, manifestDir, firefoxManifestDir)
			if err != nil {
				return err
			}
			browsers := make([]map[string]any, 0, len(result.Installed))
			lines := ""
			for _, m := range result.Installed {
				browsers = append(browsers, map[string]any{"id": m.ID, "label": m.Label, "manifest_path": m.Path})
				lines += fmt.Sprintf("  %-16s %s\n", m.Label+":", m.Path)
			}
			return opt.printResult(map[string]any{
				"manifest_path":         result.ManifestPath,
				"firefox_manifest_path": result.FirefoxManifestPath,
				"symlink_path":          result.SymlinkPath,
				"executable":            result.Executable,
				"extension_id":          result.ExtensionID,
				"extension_ids":         result.ExtensionIDs,
				"firefox_extension_id":  cfg.Browser.FirefoxExtensionID,
				"browsers":              browsers,
			}, "Installed native host for %d browser(s):\n%s  host:            %s -> %s",
				len(result.Installed), lines, result.SymlinkPath, result.Executable)
		},
	}
	install.Flags().StringVar(&manifestDir, "manifest-dir", "", "override the Chrome native-messaging manifest directory")
	install.Flags().StringVar(&firefoxManifestDir, "firefox-manifest-dir", "", "override the Firefox native-messaging manifest directory")

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove native-messaging host manifests and the host executable",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			targets, err := selectManifestTargets(manifestDir, firefoxManifestDir)
			if err != nil {
				return err
			}
			removed := make([]string, 0, len(targets)+2)
			for _, t := range targets {
				path := filepath.Join(t.dir, nativeHostManifestName+".json")
				if err := os.Remove(path); err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						return err
					}
				} else {
					removed = append(removed, path)
				}
				if err := deregisterManifest(t); err != nil {
					return err
				}
			}
			hostRemoved, err := nativehost.RemoveExecutable()
			if err != nil {
				return err
			}
			removed = append(removed, hostRemoved...)
			chromeT, _ := findTarget(targets, "chrome")
			firefoxT, _ := findTarget(targets, "firefox")
			return opt.printResult(map[string]any{
				"manifest_path":         filepath.Join(chromeT.dir, nativeHostManifestName+".json"),
				"firefox_manifest_path": filepath.Join(firefoxT.dir, nativeHostManifestName+".json"),
				"symlink_path":          nativehost.ExecPath(),
				"removed":               removed,
			}, "Removed %d native host artifact(s)", len(removed))
		},
	}
	uninstall.Flags().StringVar(&manifestDir, "manifest-dir", "", "override the Chrome native-messaging manifest directory")
	uninstall.Flags().StringVar(&firefoxManifestDir, "firefox-manifest-dir", "", "override the Firefox native-messaging manifest directory")

	status := &cobra.Command{
		Use:   "status",
		Short: "Report native-messaging host registration state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			targets, err := selectManifestTargets(manifestDir, firefoxManifestDir)
			if err != nil {
				return err
			}
			browsers := make([]map[string]any, 0, len(targets))
			for _, t := range targets {
				path := filepath.Join(t.dir, nativeHostManifestName+".json")
				_, statErr := os.Stat(path)
				browsers = append(browsers, map[string]any{
					"id":               t.id,
					"label":            t.label,
					"installed":        t.present,
					"manifest_path":    path,
					"manifest_present": statErr == nil,
				})
			}
			chromeT, _ := findTarget(targets, "chrome")
			firefoxT, _ := findTarget(targets, "firefox")
			manifestPath := filepath.Join(chromeT.dir, nativeHostManifestName+".json")
			firefoxManifestPath := filepath.Join(firefoxT.dir, nativeHostManifestName+".json")
			_, manErr := os.Stat(manifestPath)
			manifestPresent := manErr == nil
			_, firefoxManErr := os.Stat(firefoxManifestPath)
			firefoxManifestPresent := firefoxManErr == nil
			symlinkPath := nativehost.ExecPath()
			symlinkTarget, targetExists := nativehost.ExecTarget()
			return opt.printResult(map[string]any{
				"manifest_path":            manifestPath,
				"manifest_present":         manifestPresent,
				"extension_id":             cfg.Browser.ExtensionID,
				"extension_ids":            cfg.Browser.ChromiumExtensionIDs(),
				"firefox_manifest_path":    firefoxManifestPath,
				"firefox_manifest_present": firefoxManifestPresent,
				"firefox_extension_id":     cfg.Browser.FirefoxExtensionID,
				"symlink_path":             symlinkPath,
				"symlink_target":           symlinkTarget,
				"target_exists":            targetExists,
				"browsers":                 browsers,
			}, "manifest: %s (present=%t)\nextension_id: %s\nfirefox manifest: %s (present=%t)\nfirefox_extension_id: %s\nsymlink: %s -> %s (target_exists=%t)",
				manifestPath, manifestPresent, cfg.Browser.ExtensionID, firefoxManifestPath, firefoxManifestPresent, cfg.Browser.FirefoxExtensionID, symlinkPath, symlinkTarget, targetExists)
		},
	}
	status.Flags().StringVar(&manifestDir, "manifest-dir", "", "override the Chrome native-messaging manifest directory")
	status.Flags().StringVar(&firefoxManifestDir, "firefox-manifest-dir", "", "override the Firefox native-messaging manifest directory")

	command.AddCommand(install, uninstall, status)
	return command
}

type installedManifest struct {
	ID    string
	Label string
	Path  string
}

type nativeHostInstallResult struct {
	ManifestPath        string
	FirefoxManifestPath string
	SymlinkPath         string
	Executable          string
	ExtensionID         string
	ExtensionIDs        []string
	Installed           []installedManifest
}

// installNativeHost is the shared registration implementation used by both the
// native-host subcommand and first-run setup. It writes and registers the host
// manifest with every present browser: the shared Chromium allowlist for
// Chrome/Edge/Vivaldi/Brave/Opera/Chromium, and the Gecko manifest for Firefox.
func installNativeHost(cfg config.Config, manifestDir, firefoxManifestDir string) (nativeHostInstallResult, error) {
	chromeIDs := cfg.Browser.ChromiumExtensionIDs()
	if len(chromeIDs) == 0 {
		return nativeHostInstallResult{}, fmt.Errorf("browser.extension_id is not set in %s: set it before installing the native host (32 chars a-p; the fixed Chrome extension ID)", cfg.Path)
	}

	exe, err := os.Executable()
	if err != nil {
		return nativeHostInstallResult{}, err
	}
	if resolved, rErr := filepath.EvalSymlinks(exe); rErr == nil {
		exe = resolved
	}

	hostPath, err := nativehost.InstallExecutable(exe)
	if err != nil {
		return nativeHostInstallResult{}, err
	}

	targets, err := selectManifestTargets(manifestDir, firefoxManifestDir)
	if err != nil {
		return nativeHostInstallResult{}, err
	}
	firefoxID := cfg.Browser.FirefoxExtensionID

	result := nativeHostInstallResult{
		SymlinkPath:  hostPath,
		Executable:   exe,
		ExtensionID:  chromeIDs[0],
		ExtensionIDs: chromeIDs,
	}
	for _, t := range targets {
		if !t.present {
			continue
		}
		var manifest nativeHostManifest
		switch t.family {
		case familyChromium:
			manifest = chromiumManifest(hostPath, chromeIDs)
		case familyGecko:
			if firefoxID == "" {
				continue
			}
			manifest = geckoManifest(hostPath, firefoxID)
		default:
			continue
		}
		if err := os.MkdirAll(t.dir, 0o755); err != nil {
			return nativeHostInstallResult{}, err
		}
		path := filepath.Join(t.dir, nativeHostManifestName+".json")
		if err := writeManifestAtomic(path, manifest); err != nil {
			return nativeHostInstallResult{}, err
		}
		if err := registerManifest(t, path); err != nil {
			return nativeHostInstallResult{}, err
		}
		if err := verifyNativeHost(path); err != nil {
			return nativeHostInstallResult{}, err
		}
		result.Installed = append(result.Installed, installedManifest{ID: t.id, Label: t.label, Path: path})
		switch t.id {
		case "chrome":
			result.ManifestPath = path
		case "firefox":
			result.FirefoxManifestPath = path
		}
	}
	return result, nil
}

// writeManifestAtomic writes the manifest as a 0644 file via a same-dir temp
// file and rename, so a concurrent Chrome read never sees a partial manifest.
func writeManifestAtomic(path string, manifest nativeHostManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".papio-native-host-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o644); err != nil {
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
	return os.Rename(name, path)
}

// verifyNativeHost is the install smoke test: the manifest must re-read and
// JSON-decode with the expected name, and the installed host executable must
// resolve to a real target.
func verifyNativeHost(manifestPath string) error {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("verifying native host manifest: %w", err)
	}
	var manifest nativeHostManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("verifying native host manifest %s: %w", manifestPath, err)
	}
	if manifest.Name != nativeHostManifestName {
		return fmt.Errorf("native host manifest name = %q, want %q", manifest.Name, nativeHostManifestName)
	}
	if _, ok := nativehost.ExecTarget(); !ok {
		return fmt.Errorf("native host executable %s does not resolve to a real target", nativehost.ExecPath())
	}
	return nil
}
