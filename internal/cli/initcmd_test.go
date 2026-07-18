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
	if cfg.Email != "reader@example.test" || cfg.AccessMode != config.ModeConservative || !cfg.Updates.Check {
		t.Fatalf("config = %+v, want email, conservative access mode, and enabled update checks", cfg)
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

func TestProquestAccountIDFromInput(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
		wantErr           bool
	}{
		{name: "bare id", input: "12345", want: "12345"},
		{name: "blank disables", input: "  ", want: ""},
		{name: "extract from proquest url", input: "https://www.proquest.com/openurl/handler?url_ver=Z39.88-2004&accountid=12345", want: "12345"},
		{name: "extract when first param", input: "https://x.example/?accountid=42&foo=bar", want: "42"},
		{name: "non-numeric id rejected", input: "abc", wantErr: true},
		{name: "url without accountid rejected", input: "https://x.example/openurl?url_ver=Z39.88-2004", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := proquestAccountIDFromInput(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("input %q: want error, got %q", test.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("input %q: %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("input %q = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestInitInstitutionFlagsExtractAccountIDFromPastedURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "papio", "config.toml")
	deps := initTestDependencies(t)

	out, err := runInitForTest(t, path, deps,
		"--non-interactive", "--email", "reader@example.test", "--skip-browser",
		"--openurl-base", "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXL_INST:61EXL_NDE",
		"--shibboleth-entity-id", "https://idp.example.edu/entity",
		"--proquest-account-id", "https://www.proquest.com/openurl/handler?url_ver=Z39.88-2004&accountid=12345")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Browser.OpenURLBase != "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXL_INST:61EXL_NDE" {
		t.Fatalf("openurl base = %q", cfg.Browser.OpenURLBase)
	}
	if cfg.Browser.ShibbolethEntityID != "https://idp.example.edu/entity" {
		t.Fatalf("entity id = %q", cfg.Browser.ShibbolethEntityID)
	}
	if cfg.Browser.ProquestAccountID != "12345" {
		t.Fatalf("proquest account id = %q, want extracted 12345", cfg.Browser.ProquestAccountID)
	}
}

func runInitStdin(t *testing.T, path string, deps initDependencies, stdin string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	opt := &options{configPath: path, out: &out, errOut: &errOut}
	command := newInitCommandWithDependencies(opt, deps)
	command.SetOut(&out)
	command.SetErr(&errOut)
	command.SetIn(strings.NewReader(stdin))
	command.SetArgs(nil)
	if err := command.Execute(); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

func TestInitExtensionIDFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "papio", "config.toml")
	deps := initTestDependencies(t)
	if _, err := runInitForTest(t, path, deps,
		"--non-interactive", "--email", "reader@example.test", "--skip-browser",
		"--extension-id", "abcdefghijklmnopabcdefghijklmnop",
		"--firefox-extension-id", "papio@orgmentem.com"); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browser.ExtensionID != "abcdefghijklmnopabcdefghijklmnop" {
		t.Fatalf("extension_id = %q", cfg.Browser.ExtensionID)
	}
	if cfg.Browser.FirefoxExtensionID != "papio@orgmentem.com" {
		t.Fatalf("firefox_extension_id = %q", cfg.Browser.FirefoxExtensionID)
	}
}

func TestInitInteractiveCapturesExtensionIDsAndInstalls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "papio", "config.toml")
	var installedID string
	deps := initTestDependencies(t)
	deps.InstallNative = func(cfg config.Config) error {
		installedID = cfg.Browser.ExtensionID
		return nil
	}
	// email, zotio exec, attachment, browser=yes, chrome id, firefox id (blank
	// -> gecko default), openurl base (blank -> no institution).
	answers := strings.Join([]string{
		"reader@example.test",
		"zotio",
		"stored",
		"yes",
		"abcdefghijklmnopabcdefghijklmnop",
		"",
		"",
		"",
	}, "\n") + "\n"
	if _, err := runInitStdin(t, path, deps, answers); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Browser.ExtensionID != "abcdefghijklmnopabcdefghijklmnop" {
		t.Fatalf("extension_id = %q", cfg.Browser.ExtensionID)
	}
	if cfg.Browser.FirefoxExtensionID != defaultFirefoxExtensionID {
		t.Fatalf("firefox_extension_id = %q, want default %q", cfg.Browser.FirefoxExtensionID, defaultFirefoxExtensionID)
	}
	// The captured Chrome ID reaches the native-host install in the same run.
	if installedID != "abcdefghijklmnopabcdefghijklmnop" {
		t.Fatalf("native host installed with extension_id %q", installedID)
	}
}

func TestInitUpdateCheckPromptWritesBothAnswers(t *testing.T) {
	for _, test := range []struct {
		name, answer string
		want         bool
	}{
		{name: "default yes", answer: "", want: true},
		{name: "no", answer: "n", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			path := filepath.Join(home, ".config", "papio", "config.toml")
			answers := strings.Join([]string{
				"reader@example.test",
				"zotio",
				"stored",
				"no",
				test.answer,
			}, "\n") + "\n"
			out, err := runInitStdin(t, path, initTestDependencies(t), answers)
			if err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Updates.Check != test.want {
				t.Fatalf("updates.check = %t, want %t", cfg.Updates.Check, test.want)
			}
			if !strings.Contains(out, "Check for papio updates once a day? Queries GitHub releases only; nothing else is sent. [Y/n]") {
				t.Fatalf("update prompt missing from output: %q", out)
			}
		})
	}
}
