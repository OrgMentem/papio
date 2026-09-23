// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A config written for a release that had the native viewer helper must keep
// loading: the key is dropped, reported once through IgnoredKeys, and never
// written back.
func TestRemovedFieldKeyIsToleratedAndDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "access_mode = \"delegated\"\n\n[browser]\naction_expiry_seconds = 900\nnative_viewer_helper = \"/Users/someone/papio-native-spike\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config with a removed key must load: %v", err)
	}
	if !slices.Equal(cfg.IgnoredKeys, []string{"browser.native_viewer_helper"}) {
		t.Fatalf("IgnoredKeys = %q", cfg.IgnoredKeys)
	}
	if cfg.AccessMode != ModeDelegated || cfg.Browser.ActionExpirySeconds != 900 || cfg.Path != path {
		t.Fatalf("settings beside the removed key were not read: %+v", cfg)
	}

	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "native_viewer_helper") || strings.Contains(string(written), "papio-native-spike") {
		t.Fatalf("save wrote the removed key back:\n%s", written)
	}
	reloaded, err := Load(path)
	if err != nil || len(reloaded.IgnoredKeys) != 0 || reloaded.Browser.ActionExpirySeconds != 900 {
		t.Fatalf("reload after save: %+v, %v", reloaded, err)
	}
}

// Tolerating a removed key must not tolerate a typo beside it.
func TestRemovedFieldKeyDoesNotHideUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "access_mode = \"delegated\"\n\n[browser]\nnative_viewer_helper = \"/x\"\naction_expiry_secs = 900\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "browser.action_expiry_secs") {
		t.Fatalf("unknown key accepted or not named: %v", err)
	}
	if strings.Contains(err.Error(), "(browser.native_viewer_helper") || strings.Contains(err.Error(), ", browser.native_viewer_helper") {
		t.Fatalf("removed key reported as unknown: %v", err)
	}
}
