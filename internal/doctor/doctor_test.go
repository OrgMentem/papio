// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/pdf"
	"papio/internal/store"
	"papio/internal/update"
	"papio/internal/zotio"
)

func executable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "papio")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunReadyProfilePassesWithoutLeakingSecrets(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = data
	cfg.Email = "researcher@example.test"
	cfg.Sources[config.SourceOpenAlex] = config.Source{Enabled: true, APIKey: "SUPER_SECRET_KEY"}
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg.Path, []byte("access_mode='conservative'"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := executable(t)
	report := Run(ctx, cfg, db, pdf.Capability{
		PDFCPU: true, PDFInfo: tool, PDFToText: tool, PDFToPPM: tool, Tesseract: tool,
	}, tool)
	if !report.OK {
		t.Fatalf("ready report failed: %+v", report)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "SUPER_SECRET_KEY") {
		t.Fatalf("doctor leaked credential: %s", encoded)
	}
	var dbPass bool
	for _, c := range report.Checks {
		if c.Name == "database" && c.Status == Pass && strings.Contains(c.Detail, "schema version 14") {
			dbPass = true
		}
	}
	if !dbPass {
		t.Fatalf("database migration check missing: %+v", report.Checks)
	}
}

func TestRunReportsMissingModeCredentialsToolsAndUnsafeConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Sources[config.SourceOpenAlex] = config.Source{Enabled: true}
	cfg.DataDir = t.TempDir()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg.Path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := Run(context.Background(), cfg, nil, pdf.Capability{}, "")
	if report.OK {
		t.Fatalf("unsafe profile passed: %+v", report)
	}
	wantFailures := map[string]bool{
		"access_mode": false, "pdftotext": false, "ocr": false,
		"pdf_worker": false, "source_unpaywall": false, "source_openalex": false,
		"config_permissions": false,
	}
	for _, c := range report.Checks {
		if c.Status == Fail {
			if _, ok := wantFailures[c.Name]; ok {
				wantFailures[c.Name] = true
			}
		}
	}
	for name, found := range wantFailures {
		if !found {
			t.Errorf("missing failure check %s: %+v", name, report.Checks)
		}
	}
}

func TestRunWarnsWhenOCRExplicitlyDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.Email = "a@b.test"
	cfg.Sources[config.SourceOpenAlex] = config.Source{Enabled: false}
	cfg.PDF.OCREnabled = false
	tool := executable(t)
	report := Run(context.Background(), cfg, nil, pdf.Capability{PDFToText: tool}, tool)
	var warned bool
	for _, c := range report.Checks {
		if c.Name == "ocr" && c.Status == Warn {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("disabled OCR warning missing: %+v", report.Checks)
	}
}

func TestRunIntegrationReportsVersionSkewAndSkipsUnconfiguredManifests(t *testing.T) {
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	report := RunIntegration(context.Background(), IntegrationDependencies{
		CLIVersion: "cli-version",
		LoadConfig: func() (config.Config, error) {
			return cfg, nil
		},
		DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
			return DaemonStatus{Status: "ok", Version: "daemon-version"}, nil
		},
		ManifestDir: func(config.Config) (string, error) { return t.TempDir(), nil },
		FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return &zotio.PreflightResult{Version: "1.2.3"}, nil
		},
	})
	if !report.OK {
		t.Fatalf("integration report failed: %+v", report)
	}
	if got := report.Checks[1]; got.Name != "daemon" || got.Status != Warn || !strings.Contains(got.Detail, "daemon-version") {
		t.Fatalf("daemon check = %#v", got)
	}
	if got := report.Checks[3]; got.Name != "native host (Chrome)" || got.Status != Skip {
		t.Fatalf("Chrome manifest check = %#v", got)
	}
}

func TestRunIntegrationFailsOnDanglingHostExecutable(t *testing.T) {
	const extID = "abcdefghijklmnopabcdefghijklmnop"
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	cfg.Browser.ExtensionID = extID
	manifestDir := t.TempDir()
	manifest := `{"name":"com.orgmentem.papio","path":"/gone/papio-native-host","type":"stdio",` +
		`"allowed_origins":["chrome-extension://` + extID + `/"]}`
	if err := os.WriteFile(filepath.Join(manifestDir, "com.orgmentem.papio.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	report := RunIntegration(context.Background(), IntegrationDependencies{
		CLIVersion: "v1",
		LoadConfig: func() (config.Config, error) { return cfg, nil },
		DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
			return DaemonStatus{Status: "ok", Version: "v1"}, nil
		},
		ManifestDir: func(config.Config) (string, error) { return manifestDir, nil },
		FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
		ReadFile:    os.ReadFile,
		// The host symlink points at a binary a brew upgrade removed.
		HostExecutableResolves: func(string) bool { return false },
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return &zotio.PreflightResult{Version: "1.2.3"}, nil
		},
	})
	var chrome Check
	for _, c := range report.Checks {
		if c.Name == "native host (Chrome)" {
			chrome = c
		}
	}
	if chrome.Status != Fail || !strings.Contains(chrome.Detail, "dangling") {
		t.Fatalf("Chrome host check = %#v, want Fail mentioning dangling", chrome)
	}
	if chrome.Remediation != "papio native-host install" {
		t.Fatalf("remediation = %q, want papio native-host install", chrome.Remediation)
	}
	if report.OK {
		t.Fatalf("report must fail when the host executable is missing: %+v", report)
	}
}

func TestRunIntegrationUpdates(t *testing.T) {
	baseConfig := func() config.Config {
		cfg := config.Default()
		cfg.Path = filepath.Join(t.TempDir(), "config.toml")
		cfg.DataDir = t.TempDir()
		return cfg
	}
	depsFor := func(cfg config.Config) IntegrationDependencies {
		return IntegrationDependencies{
			CLIVersion: "1.2.3",
			LoadConfig: func() (config.Config, error) { return cfg, nil },
			DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
				return DaemonStatus{Status: "ok", Version: "1.2.3"}, nil
			},
			ManifestDir: func(config.Config) (string, error) { return t.TempDir(), nil },
			FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
			ReadFile:    os.ReadFile,
			ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
				return &zotio.PreflightResult{Version: "1.2.3"}, nil
			},
			CheckUpdates: func(context.Context, config.Config) (*update.Info, error) {
				return &update.Info{LatestVersion: "1.2.3", URL: "https://example.test/papio"}, nil
			},
			CheckZotioUpdates: func(context.Context, config.Config) (*update.Info, error) {
				return &update.Info{LatestVersion: "1.2.3", URL: "https://example.test/zotio"}, nil
			},
		}
	}
	find := func(report Report, name string) Check {
		for _, check := range report.Checks {
			if check.Name == name {
				return check
			}
		}
		t.Fatalf("%s check missing: %+v", name, report.Checks)
		return Check{}
	}

	t.Run("disabled", func(t *testing.T) {
		cfg := baseConfig()
		deps := depsFor(cfg)
		deps.CheckUpdates = func(context.Context, config.Config) (*update.Info, error) {
			t.Fatal("disabled papio update check was invoked")
			return nil, nil
		}
		deps.CheckZotioUpdates = func(context.Context, config.Config) (*update.Info, error) {
			t.Fatal("disabled zotio update check was invoked")
			return nil, nil
		}
		report := RunIntegration(context.Background(), deps)
		for _, name := range []string{"updates (papio)", "updates (zotio)"} {
			got := find(report, name)
			if got.Status != Skip || got.Detail != "update check disabled ([updates] check = false)" {
				t.Fatalf("%s check = %#v", name, got)
			}
		}
	})

	t.Run("current", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Updates.Check = true
		report := RunIntegration(context.Background(), depsFor(cfg))
		for _, name := range []string{"updates (papio)", "updates (zotio)"} {
			got := find(report, name)
			if got.Status != Pass {
				t.Fatalf("%s check = %#v", name, got)
			}
		}
	})

	t.Run("zotio behind", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Updates.Check = true
		cfg.Zotio.Executable = "/opt/homebrew/bin/zotio"
		deps := depsFor(cfg)
		deps.CheckZotioUpdates = func(context.Context, config.Config) (*update.Info, error) {
			return &update.Info{LatestVersion: "1.2.4", URL: "https://example.test/zotio"}, nil
		}
		got := find(RunIntegration(context.Background(), deps), "updates (zotio)")
		if got.Status != Warn || got.Detail != "zotio 1.2.4 available (you have 1.2.3)" || got.Remediation != "brew upgrade zotio" {
			t.Fatalf("zotio update check = %#v", got)
		}
	})

	t.Run("zotio preflight failed", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Updates.Check = true
		deps := depsFor(cfg)
		deps.ZotioPreflight = func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return nil, errors.New("zotio not found")
		}
		deps.CheckZotioUpdates = func(context.Context, config.Config) (*update.Info, error) {
			t.Fatal("zotio update check ran despite failed preflight")
			return nil, nil
		}
		got := find(RunIntegration(context.Background(), deps), "updates (zotio)")
		if got.Status != Skip || got.Detail != "skipped: zotio preflight failed" {
			t.Fatalf("zotio update check = %#v", got)
		}
	})
}

func TestRunIntegrationSkipsZotioWhenUnconfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = ""
	cfg.Updates.Check = true
	deps := IntegrationDependencies{
		CLIVersion: "1.2.3",
		LoadConfig: func() (config.Config, error) { return cfg, nil },
		DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
			return DaemonStatus{Status: "ok", Version: "1.2.3"}, nil
		},
		ManifestDir: func(config.Config) (string, error) { return t.TempDir(), nil },
		FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			t.Fatal("zotio preflight ran despite empty executable")
			return nil, nil
		},
		CheckUpdates: func(context.Context, config.Config) (*update.Info, error) {
			return &update.Info{LatestVersion: "1.2.3", URL: "https://example.test/papio"}, nil
		},
		CheckZotioUpdates: func(context.Context, config.Config) (*update.Info, error) {
			t.Fatal("zotio update check ran despite empty executable")
			return nil, nil
		},
	}
	report := RunIntegration(context.Background(), deps)
	var zotioCheck, zotioUpdates Check
	for _, check := range report.Checks {
		switch check.Name {
		case "zotio":
			zotioCheck = check
		case "updates (zotio)":
			zotioUpdates = check
		}
	}
	if zotioCheck.Status != Skip || !strings.Contains(zotioCheck.Detail, "not configured") {
		t.Fatalf("zotio check = %#v, want Skip not-configured", zotioCheck)
	}
	if !report.OK {
		t.Fatalf("unconfigured zotio must not fail doctor: %+v", report)
	}
	if zotioUpdates.Status != Skip || zotioUpdates.Detail != "skipped: zotio is not configured" {
		t.Fatalf("zotio updates check = %#v", zotioUpdates)
	}
}
