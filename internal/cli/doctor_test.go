// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/zotio"
)

func TestDoctorAllGreenRendersReadinessBeforeIntegration(t *testing.T) {
	chromeDir := t.TempDir()
	firefoxDir := t.TempDir()
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	cfg.Browser.ExtensionID = "abcdefghijklmnopabcdefghijklmnop"
	cfg.Browser.FirefoxExtensionID = "papio@example.test"
	writeDoctorManifest(t, filepath.Join(chromeDir, nativeHostManifestName+".json"), nativeHostManifest{
		AllowedOrigins: []string{"chrome-extension://" + cfg.Browser.ExtensionID + "/"},
	})
	writeDoctorManifest(t, filepath.Join(firefoxDir, nativeHostManifestName+".json"), nativeHostManifest{
		AllowedExtensions: []string{cfg.Browser.FirefoxExtensionID},
	})

	var out bytes.Buffer
	command := newDoctorCommandWithDependencies(&options{out: &out}, doctor.IntegrationDependencies{
		CLIVersion: api.Version,
		LoadConfig: func() (config.Config, error) { return cfg, nil },
		DaemonStatus: func(context.Context, config.Config) (doctor.DaemonStatus, error) {
			return doctor.DaemonStatus{Status: "ok", Version: api.Version, ExtensionConnected: true, ExtensionVersion: "1.2.3"}, nil
		},
		ManifestDir: func(config.Config) (string, error) { return chromeDir, nil },
		FirefoxDir:  func(config.Config) (string, error) { return firefoxDir, nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return &zotio.PreflightResult{Version: "1.2.3", Capabilities: []zotio.Capability{{Path: "items get"}}}, nil
		},
	}, func(context.Context, config.Config) doctor.Report {
		return doctor.Report{OK: true, Checks: []doctor.Check{{Name: "access_mode", Status: doctor.Pass, Detail: "explicit access mode configured"}}}
	})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	got := out.String()
	if readiness, integration := strings.Index(got, "access_mode"), strings.Index(got, "config"); readiness < 0 || integration < 0 || readiness > integration {
		t.Fatalf("doctor report order = %q", got)
	}
	for _, name := range []string{"config", "daemon", "extension", "native host (Chrome)", "native host (Firefox)", "zotio"} {
		if !strings.Contains(got, "PASS") || !strings.Contains(got, name) {
			t.Fatalf("doctor output missing passing %q: %q", name, got)
		}
	}
	if !strings.Contains(got, "connected (v1.2.3)") {
		t.Fatalf("doctor output missing extension version: %q", got)
	}
}

func TestDoctorExtensionNotConnectedWarnsWithSetupFix(t *testing.T) {
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	report := doctor.RunIntegration(context.Background(), testDoctorDependencies(t, cfg, doctor.DaemonStatus{Status: "ok", Version: api.Version}))
	got := report.Checks[2]
	if got.Name != "extension" || got.Status != doctor.Warn || got.Detail != "extension has not connected since daemon start" || !strings.Contains(got.Remediation, "browser extension") || !strings.Contains(got.Remediation, "papio init") {
		t.Fatalf("extension check = %#v", got)
	}
}

func TestDoctorDaemonDownCollapsesIntegrationSkips(t *testing.T) {
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	deps := testDoctorDependencies(t, cfg, doctor.DaemonStatus{})
	deps.DaemonStatus = func(context.Context, config.Config) (doctor.DaemonStatus, error) {
		return doctor.DaemonStatus{}, errors.New("dial ipc daemon: no such file or directory")
	}
	report := doctor.RunIntegration(context.Background(), deps)
	daemonCheck := report.Checks[1]
	if daemonCheck.Name != "daemon" || daemonCheck.Status != doctor.Fail || !strings.Contains(daemonCheck.Detail, "not running or unreachable") || !strings.Contains(daemonCheck.Remediation, "--start") {
		t.Fatalf("daemon check = %#v", daemonCheck)
	}
	got := report.Checks[2]
	if got.Name != "integrations" || got.Status != doctor.Skip || got.Detail != "skipped: daemon is unreachable (extension, native hosts, zotio)" {
		t.Fatalf("integrations check = %#v", got)
	}
	// The single collapsed skip replaces the old four-line cascade.
	for _, check := range report.Checks {
		if check.Status == doctor.Skip && check.Name != "integrations" && !strings.HasPrefix(check.Name, "updates") {
			t.Fatalf("unexpected extra skip check %#v", check)
		}
	}
}

func TestDoctorConfigFailureSkipsDaemon(t *testing.T) {
	daemonCalled := false
	deps := testDoctorDependencies(t, config.Config{}, doctor.DaemonStatus{})
	deps.LoadConfig = func() (config.Config, error) {
		return config.Config{}, errors.New("parsing config: unknown field \"browser.new_field\"")
	}
	deps.DaemonStatus = func(context.Context, config.Config) (doctor.DaemonStatus, error) {
		daemonCalled = true
		return doctor.DaemonStatus{}, nil
	}
	report := doctor.RunIntegration(context.Background(), deps)
	if report.OK || daemonCalled {
		t.Fatalf("report/daemon call = %#v / %t", report, daemonCalled)
	}
	if got := report.Checks[0]; got.Status != doctor.Fail || got.Remediation != "update papio or remove the unrecognized field" {
		t.Fatalf("config check = %#v", got)
	}
	if got := report.Checks[1]; got.Name != "daemon" || got.Status != doctor.Skip {
		t.Fatalf("daemon check = %#v", got)
	}
}

func TestDoctorReadinessFailureReturnsCommandError(t *testing.T) {
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	var out bytes.Buffer
	command := newDoctorCommandWithDependencies(&options{out: &out}, testDoctorDependencies(t, cfg, doctor.DaemonStatus{Status: "ok", Version: api.Version}), func(context.Context, config.Config) doctor.Report {
		return doctor.Report{OK: false, Checks: []doctor.Check{{Name: "pdf_worker", Status: doctor.Fail, Detail: "not runnable", Remediation: "rebuild papio"}}}
	})
	if err := command.ExecuteContext(context.Background()); !errors.Is(err, errDoctorFailed) {
		t.Fatalf("doctor error = %v, want failure", err)
	}
	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "fix: rebuild papio") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func testDoctorDependencies(t *testing.T, cfg config.Config, status doctor.DaemonStatus) doctor.IntegrationDependencies {
	t.Helper()
	return doctor.IntegrationDependencies{
		CLIVersion: api.Version,
		LoadConfig: func() (config.Config, error) { return cfg, nil },
		DaemonStatus: func(context.Context, config.Config) (doctor.DaemonStatus, error) {
			return status, nil
		},
		ManifestDir: func(config.Config) (string, error) { return t.TempDir(), nil },
		FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return &zotio.PreflightResult{Version: "1.2.3"}, nil
		},
	}
}

func writeDoctorManifest(t *testing.T, path string, manifest nativeHostManifest) {
	t.Helper()
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
