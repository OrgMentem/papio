// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/store"
)

type initTestCloser struct{}

func (initTestCloser) Close() error { return nil }

func initTestDependencies(t *testing.T) initDependencies {
	t.Helper()
	return initDependencies{
		Bootstrap: func(ctx context.Context, cfg config.Config) (io.Closer, error) {
			return bootstrap.New(ctx, cfg)
		},
		CheckZotio: func(context.Context, string) error { return nil },
		InstallNative: func(config.Config) error {
			t.Fatal("native installer must not run in a --skip-browser test")
			return nil
		},
		RunDoctor: func(context.Context, *options) (doctor.Report, error) {
			return doctor.Report{OK: true, Checks: []doctor.Check{{Name: "database", Status: doctor.Pass, Detail: "ok"}}}, nil
		},
	}
}

func runInitForTest(t *testing.T, path string, deps initDependencies, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	opt := &options{configPath: path, out: &out, errOut: &errOut}
	command := newInitCommandWithDependencies(opt, deps)
	command.SetOut(&out)
	command.SetErr(&errOut)
	command.SetArgs(args)
	if err := command.Execute(); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

func TestInitFreshWritesConfigAndAppliesMigrations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "papio", "config.toml")
	deps := initTestDependencies(t)

	out, err := runInitForTest(t, path, deps, "--non-interactive", "--email", "reader@example.test", "--skip-browser")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Email != "reader@example.test" || cfg.AccessMode != config.ModeConservative {
		t.Fatalf("config = %+v, want email and conservative access mode", cfg)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "papio.db")); err != nil {
		t.Fatalf("migration bootstrap did not create database: %v", err)
	}
	db, err := store.Open(context.Background(), cfg.DataDir)
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	defer db.Close()
	version, err := db.UserVersion(context.Background())
	if err != nil || version == 0 {
		t.Fatalf("schema version = %d, %v; want a nonzero applied migration", version, err)
	}
	if !strings.Contains(out, "✓ Configuration:") || !strings.Contains(out, "✓ Data:") || !strings.Contains(out, "PASS  database") {
		t.Fatalf("init output does not render setup and doctor findings:\n%s", out)
	}
}

func TestInitRerunPreservesValuesAndFlagOverridesOneField(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "papio", "config.toml")
	deps := initTestDependencies(t)
	if out, err := runInitForTest(t, path, deps, "--non-interactive", "--email", "first@example.test", "--skip-browser"); err != nil {
		t.Fatalf("first init: %v\n%s", err, out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Email = "custom@example.test"
	cfg.Zotio.Executable = filepath.Join(home, "tools", "custom-zotio")
	cfg.Zotio.AttachmentMode = "stored"
	if err := config.Save(cfg, path); err != nil {
		t.Fatalf("customize config: %v", err)
	}

	if out, err := runInitForTest(t, path, deps, "--non-interactive", "--attachment-mode", "linked-file", "--skip-browser"); err != nil {
		t.Fatalf("rerun init: %v\n%s", err, out)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "custom@example.test" {
		t.Fatalf("email changed on rerun: %q", got.Email)
	}
	if got.Zotio.Executable != cfg.Zotio.Executable {
		t.Fatalf("zotio path changed on rerun: %q, want %q", got.Zotio.Executable, cfg.Zotio.Executable)
	}
	if got.Zotio.AttachmentMode != "linked-file" {
		t.Fatalf("attachment mode = %q, want linked-file", got.Zotio.AttachmentMode)
	}
}

func TestInitZotioWarningAndRequiredFailureExitContract(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	deps := initTestDependencies(t)
	deps.CheckZotio = func(context.Context, string) error { return errors.New("zotio not found") }
	path := filepath.Join(home, ".config", "papio", "config.toml")

	out, err := runInitForTest(t, path, deps, "--non-interactive", "--email", "reader@example.test", "--skip-browser")
	if err != nil {
		t.Fatalf("zotio warning must not fail init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "✗ Zotio:") || !strings.Contains(out, "Zotero features are disabled") {
		t.Fatalf("zotio warning missing from output:\n%s", out)
	}

	invalidPath := filepath.Join(home, "invalid", "config.toml")
	if _, err := runInitForTest(t, invalidPath, deps, "--non-interactive", "--email", "not-an-email", "--skip-browser"); err == nil {
		t.Fatal("invalid required email succeeded")
	}

	migrationDeps := initTestDependencies(t)
	migrationDeps.Bootstrap = func(context.Context, config.Config) (io.Closer, error) {
		return initTestCloser{}, errors.New("database unavailable")
	}
	migrationPath := filepath.Join(home, "migration-fails", "config.toml")
	if _, err := runInitForTest(t, migrationPath, migrationDeps, "--non-interactive", "--email", "reader@example.test", "--skip-browser"); err == nil {
		t.Fatal("migration failure succeeded")
	}
}

func TestRootRegistersInit(t *testing.T) {
	root := NewRoot(io.Discard, io.Discard)
	command, _, err := root.Find([]string{"init"})
	if err != nil || command == nil || command.Name() != "init" {
		t.Fatalf("root init command = %v, %v", command, err)
	}
}
