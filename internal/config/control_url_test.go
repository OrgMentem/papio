// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadUpdatesTOML(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[updates]\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestUpdatesControlURLDefaultsEmptyAndLoads(t *testing.T) {
	if got := Default().Updates.ControlURL; got != "" {
		t.Fatalf("default updates.control_url = %q, want empty (the repository file)", got)
	}
	cfg, err := loadUpdatesTOML(t, "check=true\ncontrol_url='https://example.org/control.json'\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Updates.ControlURL != "https://example.org/control.json" {
		t.Fatalf("control_url = %q", cfg.Updates.ControlURL)
	}
	if _, err := loadUpdatesTOML(t, "control_url='http://127.0.0.1:8080/control.json'\n"); err != nil {
		t.Fatalf("a loopback http URL must load: %v", err)
	}
}

func TestUpdatesControlURLRejectsInsecureAndOddURLs(t *testing.T) {
	for _, bad := range []string{"http://example.org/c.json", "ftp://example.org/c.json", "https://user:pw@example.org/c.json", "https://example.org/c.json#x", "not a url"} {
		_, err := loadUpdatesTOML(t, "control_url='"+bad+"'\n")
		if err == nil || !strings.Contains(err.Error(), "updates.control_url") {
			t.Errorf("control_url %q: err = %v, want an updates.control_url error", bad, err)
		}
	}
}
