// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"papio/internal/config"
)

// Native-host registration constants. The runtime dispatches to native-host
// mode by executable basename (nativeHostBinaryName); Chrome launches the host
// through the manifest named nativeHostManifestName.
const (
	nativeHostManifestName = "com.orgmentem.papio"
	nativeHostBinaryName   = "papio-native-host"
	nativeHostDescription  = "papio native-messaging host for ordinary-Chrome institutional paper-acquisition handoff"
)

// nativeHostManifest is Chrome's native-messaging host manifest. Metadata only:
// no secrets cross this file. allowed_origins is pinned to the single fixed
// extension ID so no other extension can reach the host.
type nativeHostManifest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Path           string   `json:"path"`
	Type           string   `json:"type"`
	AllowedOrigins []string `json:"allowed_origins"`
}

// nativeHostSymlinkPath is the fixed-name executable Chrome invokes. The
// installer points it at the current papio binary; runtime dispatch keys off
// its basename.
func nativeHostSymlinkPath() string {
	return filepath.Join(config.Dir(), "bin", nativeHostBinaryName)
}

// defaultManifestDir is Chrome's per-user NativeMessagingHosts directory.
func defaultManifestDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts"), nil
	case "linux":
		return filepath.Join(home, ".config", "google-chrome", "NativeMessagingHosts"), nil
	default:
		return "", fmt.Errorf("native-messaging host install is not supported on %s (register the manifest manually)", runtime.GOOS)
	}
}

// resolveManifestDir prefers an explicit override (tests, custom Chrome dirs).
func resolveManifestDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return defaultManifestDir()
}

func newNativeHostCommand(opt *options) *cobra.Command {
	command := &cobra.Command{
		Use:   "native-host",
		Short: "Manage the Chrome native-messaging host registration",
	}
	var manifestDir string

	install := &cobra.Command{
		Use:   "install",
		Short: "Register the native-messaging host manifest and executable symlink",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			extID := cfg.Browser.ExtensionID
			if extID == "" {
				return fmt.Errorf("browser.extension_id is not set in %s: set it before installing the native host (32 chars a-p; the fixed Chrome extension ID)", cfg.Path)
			}

			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if resolved, rErr := filepath.EvalSymlinks(exe); rErr == nil {
				exe = resolved
			}

			symlinkPath := nativeHostSymlinkPath()
			if err := os.MkdirAll(filepath.Dir(symlinkPath), 0o755); err != nil {
				return err
			}
			if err := os.Remove(symlinkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.Symlink(exe, symlinkPath); err != nil {
				return err
			}

			dir, err := resolveManifestDir(manifestDir)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			manifestPath := filepath.Join(dir, nativeHostManifestName+".json")
			manifest := nativeHostManifest{
				Name:           nativeHostManifestName,
				Description:    nativeHostDescription,
				Path:           symlinkPath,
				Type:           "stdio",
				AllowedOrigins: []string{"chrome-extension://" + extID + "/"},
			}
			if err := writeManifestAtomic(manifestPath, manifest); err != nil {
				return err
			}
			if err := verifyNativeHost(manifestPath, symlinkPath); err != nil {
				return err
			}
			return opt.printResult(map[string]any{
				"manifest_path": manifestPath,
				"symlink_path":  symlinkPath,
				"executable":    exe,
				"extension_id":  extID,
			}, "Installed native host:\n  manifest: %s\n  symlink:  %s -> %s", manifestPath, symlinkPath, exe)
		},
	}
	install.Flags().StringVar(&manifestDir, "manifest-dir", "", "override the Chrome native-messaging manifest directory")

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the native-messaging host manifest and executable symlink",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := resolveManifestDir(manifestDir)
			if err != nil {
				return err
			}
			manifestPath := filepath.Join(dir, nativeHostManifestName+".json")
			symlinkPath := nativeHostSymlinkPath()
			removed := make([]string, 0, 2)
			for _, path := range []string{manifestPath, symlinkPath} {
				if err := os.Remove(path); err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						return err
					}
					continue
				}
				removed = append(removed, path)
			}
			return opt.printResult(map[string]any{
				"manifest_path": manifestPath,
				"symlink_path":  symlinkPath,
				"removed":       removed,
			}, "Removed %d native host artifact(s)", len(removed))
		},
	}
	uninstall.Flags().StringVar(&manifestDir, "manifest-dir", "", "override the Chrome native-messaging manifest directory")

	status := &cobra.Command{
		Use:   "status",
		Short: "Report native-messaging host registration state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			dir, err := resolveManifestDir(manifestDir)
			if err != nil {
				return err
			}
			manifestPath := filepath.Join(dir, nativeHostManifestName+".json")
			symlinkPath := nativeHostSymlinkPath()

			_, manErr := os.Stat(manifestPath)
			manifestPresent := manErr == nil

			var symlinkTarget string
			targetExists := false
			if target, rErr := os.Readlink(symlinkPath); rErr == nil {
				symlinkTarget = target
				if _, sErr := os.Stat(symlinkPath); sErr == nil {
					targetExists = true
				}
			}
			return opt.printResult(map[string]any{
				"manifest_path":    manifestPath,
				"manifest_present": manifestPresent,
				"extension_id":     cfg.Browser.ExtensionID,
				"symlink_path":     symlinkPath,
				"symlink_target":   symlinkTarget,
				"target_exists":    targetExists,
			}, "manifest: %s (present=%t)\nextension_id: %s\nsymlink: %s -> %s (target_exists=%t)",
				manifestPath, manifestPresent, cfg.Browser.ExtensionID, symlinkPath, symlinkTarget, targetExists)
		},
	}
	status.Flags().StringVar(&manifestDir, "manifest-dir", "", "override the Chrome native-messaging manifest directory")

	command.AddCommand(install, uninstall, status)
	return command
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
// JSON-decode with the expected name, and the symlink target must exist.
func verifyNativeHost(manifestPath, symlinkPath string) error {
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
	target, err := os.Readlink(symlinkPath)
	if err != nil {
		return fmt.Errorf("verifying native host symlink %s: %w", symlinkPath, err)
	}
	if _, err := os.Lstat(target); err != nil {
		return fmt.Errorf("native host symlink target %s is unreachable: %w", target, err)
	}
	return nil
}
