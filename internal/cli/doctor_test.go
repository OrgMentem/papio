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
	"papio/internal/zotio"
)

func TestDoctorAllGreen(t *testing.T) {
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
	command := newDoctorCommandWithDependencies(&options{out: &out}, doctorDependencies{
		LoadConfig: func(*options) (config.Config, error) { return cfg, nil },
		DaemonStatus: func(context.Context, *options, config.Config) (map[string]string, error) {
			return map[string]string{"status": "ok", "version": api.Version}, nil
		},
		ManifestDir: func() (string, error) { return chromeDir, nil },
		FirefoxDir:  func() (string, error) { return firefoxDir, nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return &zotio.PreflightResult{Version: "1.2.3", Capabilities: []zotio.Capability{{Path: "items get"}}}, nil
		},
	})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	got := out.String()
	for _, name := range []string{"config", "daemon", "native host (Chrome)", "native host (Firefox)", "zotio"} {
		if !strings.Contains(got, "PASS") || !strings.Contains(got, name) {
			t.Fatalf("doctor output missing passing %q: %q", name, got)
		}
	}
}

func TestDoctorConfigFailureSkipsDaemon(t *testing.T) {
	daemonCalled := false
	report := runIntegrationDoctor(context.Background(), &options{}, doctorDependencies{
		LoadConfig: func(*options) (config.Config, error) {
			return config.Config{}, errors.New("parsing config: unknown field \"browser.new_field\"")
		},
		DaemonStatus: func(context.Context, *options, config.Config) (map[string]string, error) {
			daemonCalled = true
			return nil, nil
		},
		ManifestDir: func() (string, error) { return "", nil },
		FirefoxDir:  func() (string, error) { return "", nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return nil, nil
		},
	})
	if report.OK || daemonCalled {
		t.Fatalf("report/daemon call = %#v / %t", report, daemonCalled)
	}
	if got := report.Checks[0]; got.Status != doctorFail || got.Fix != "update papio or remove the unrecognized field" {
		t.Fatalf("config check = %#v", got)
	}
	if got := report.Checks[1]; got.Name != "daemon" || got.Status != doctorSkip {
		t.Fatalf("daemon check = %#v", got)
	}
}

func TestDoctorFailureReturnsCommandError(t *testing.T) {
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	cfg.Browser.ExtensionID = "abcdefghijklmnopabcdefghijklmnop"
	var out bytes.Buffer
	command := newDoctorCommandWithDependencies(&options{out: &out}, doctorDependencies{
		LoadConfig: func(*options) (config.Config, error) { return cfg, nil },
		DaemonStatus: func(context.Context, *options, config.Config) (map[string]string, error) {
			return map[string]string{"status": "ok", "version": api.Version}, nil
		},
		ManifestDir: func() (string, error) { return t.TempDir(), nil },
		FirefoxDir:  func() (string, error) { return t.TempDir(), nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return &zotio.PreflightResult{Version: "1.2.3"}, nil
		},
	})
	if err := command.ExecuteContext(context.Background()); !errors.Is(err, errDoctorFailed) {
		t.Fatalf("doctor error = %v, want failure", err)
	}
	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "fix: papio native-host install") {
		t.Fatalf("doctor output = %q", out.String())
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
