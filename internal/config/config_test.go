// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Email = "researcher@example.test"
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Sources[SourceOpenAlex] = Source{Enabled: true, APIKey: "secret", RatePerSec: 2, Burst: 1}
	cfg.Zotio.Executable = filepath.Join(t.TempDir(), "zotio")
	cfg.Zotio.AttachmentMode = "linked-file"
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", info.Mode().Perm())
	}
	parent, _ := os.Stat(filepath.Dir(path))
	if parent.Mode().Perm() != 0o700 {
		t.Fatalf("config dir mode = %v, want 0700", parent.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessMode != cfg.AccessMode || got.Email != cfg.Email ||
		got.Sources[SourceOpenAlex].APIKey != "secret" ||
		got.Zotio.Executable != cfg.Zotio.Executable ||
		got.Zotio.AttachmentMode != "linked-file" || got.Path != path {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestSaveRequiresExplicitAccessMode(t *testing.T) {
	err := Save(Default(), filepath.Join(t.TempDir(), "config.toml"))
	var unset *ErrAccessModeUnset
	if !errors.As(err, &unset) {
		t.Fatalf("save err = %v, want ErrAccessModeUnset", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\nunknown_option=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown config field accepted")
	}
}

func TestSaveRejectsInvalidZotioAttachmentMode(t *testing.T) {
	cfg := Default()
	cfg.AccessMode = ModeConservative
	cfg.Zotio.AttachmentMode = "copy"
	if err := Save(cfg, filepath.Join(t.TempDir(), "config.toml")); err == nil {
		t.Fatal("invalid Zotio attachment mode accepted")
	}
}

func TestZotioAutoImportDefaultsOffAndLoadsTrue(t *testing.T) {
	if Default().Zotio.AutoImport {
		t.Fatal("default zotio.auto_import = true, want false")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("access_mode='conservative'\n[zotio]\nauto_import=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Zotio.AutoImport {
		t.Fatal("loaded zotio.auto_import = false, want true")
	}
}
