// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRetractionTagsDefaultOffAndRequiresExecutable(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	if cfg.Zotio.RetractionTags {
		t.Fatal("default zotio.retraction_tags = true, want false")
	}
	cfg.Zotio.Executable = ""
	cfg.Zotio.RetractionTags = true
	err := Save(cfg, filepath.Join(t.TempDir(), "config.toml"))
	if err == nil || !strings.Contains(err.Error(), "zotio.retraction_tags requires zotio.executable") {
		t.Fatalf("retraction_tags without executable err = %v", err)
	}
}
