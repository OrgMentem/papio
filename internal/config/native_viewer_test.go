// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeViewerHelperConfiguration(t *testing.T) {
	if Default().Browser.NativeViewerHelper != "" {
		t.Fatal("native viewer helper must require explicit configuration")
	}
	for _, path := range []string{"", filepath.Join(t.TempDir(), "papio-native-helper"), "~/bin/papio-native-helper"} {
		cfg := Default()
		cfg.AccessMode = ModeDelegated
		cfg.Browser.NativeViewerHelper = path
		configPath := filepath.Join(t.TempDir(), "config.toml")
		if err := Save(cfg, configPath); err != nil {
			t.Fatalf("save configured helper: %v", err)
		}
		loaded, err := Load(configPath)
		if err != nil || loaded.Browser.NativeViewerHelper != expandHome(path) {
			t.Fatalf("helper round trip: got %q, error %v", loaded.Browser.NativeViewerHelper, err)
		}
	}
	for _, path := range []string{"papio-native-helper", "./helper", "/tmp/helper\nargument", "/tmp/helper\x00"} {
		cfg := Default()
		cfg.AccessMode = ModeDelegated
		cfg.Browser.NativeViewerHelper = path
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "browser.native_viewer_helper") {
			t.Fatalf("invalid helper path accepted: %q, %v", path, err)
		}
	}
}
