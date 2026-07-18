// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/zotio"
)

const (
	doctorPass = "pass"
	doctorWarn = "warn"
	doctorFail = "fail"
	doctorSkip = "skip"
)

var errDoctorFailed = errors.New("doctor found failing checks")

type integrationDoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type integrationDoctorReport struct {
	OK     bool                     `json:"ok"`
	Checks []integrationDoctorCheck `json:"checks"`
}

type doctorDaemonStatus struct {
	Status             string `json:"status"`
	Version            string `json:"version"`
	ExtensionConnected bool   `json:"extension_connected"`
	ExtensionVersion   string `json:"extension_version"`
}

type doctorDependencies struct {
	LoadConfig     func(*options) (config.Config, error)
	DaemonStatus   func(context.Context, *options, config.Config) (doctorDaemonStatus, error)
	ManifestDir    func() (string, error)
	FirefoxDir     func() (string, error)
	ReadFile       func(string) ([]byte, error)
	ZotioPreflight func(context.Context, config.Config) (*zotio.PreflightResult, error)
}

func defaultDoctorDependencies() doctorDependencies {
	return doctorDependencies{
		LoadConfig: func(opt *options) (config.Config, error) {
			return opt.loadConfig()
		},
		DaemonStatus: func(ctx context.Context, opt *options, _ config.Config) (doctorDaemonStatus, error) {
			// callExisting deliberately avoids daemon.NewAutostarter: doctor must
			// diagnose a stopped daemon, not hide it by starting one.
			var status doctorDaemonStatus
			if err := opt.callExisting(ctx, "ping", struct{}{}, &status); err != nil {
				return doctorDaemonStatus{}, err
			}
			return status, nil
		},
		ManifestDir: defaultManifestDir,
		FirefoxDir:  defaultFirefoxManifestDir,
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(ctx context.Context, cfg config.Config) (*zotio.PreflightResult, error) {
			return zotio.New(cfg.Zotio).Preflight(ctx)
		},
	}
}

func newDoctorCommand(opt *options) *cobra.Command {
	return newDoctorCommandWithDependencies(opt, defaultDoctorDependencies())
}

func newDoctorCommandWithDependencies(opt *options, deps doctorDependencies) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the local integration chain",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := runIntegrationDoctor(cmd.Context(), opt, deps)
			if opt.jsonOutput {
				if err := opt.printJSON(report); err != nil {
					return err
				}
			} else if err := renderIntegrationDoctorReport(opt.out, report); err != nil {
				return err
			}
			if !report.OK {
				return errDoctorFailed
			}
			return nil
		},
	}
}

func runIntegrationDoctor(ctx context.Context, opt *options, deps doctorDependencies) integrationDoctorReport {
	report := integrationDoctorReport{OK: true}
	add := func(name, status, detail, fix string) {
		report.Checks = append(report.Checks, integrationDoctorCheck{Name: name, Status: status, Detail: detail, Fix: fix})
		if status == doctorFail {
			report.OK = false
		}
	}
	skipRemaining := func(reason string) {
		add("daemon", doctorSkip, reason, "")
		add("extension", doctorSkip, reason, "")
		add("native host (Chrome)", doctorSkip, reason, "")
		add("native host (Firefox)", doctorSkip, reason, "")
		add("zotio", doctorSkip, reason, "")
	}

	if !doctorDependenciesComplete(deps) {
		add("doctor", doctorFail, "doctor command dependencies are incomplete", "reinstall papio")
		return report
	}
	cfg, err := deps.LoadConfig(opt)
	if err != nil {
		fix := "correct the configuration error above"
		if strings.Contains(strings.ToLower(err.Error()), "unknown field") {
			fix = "update papio or remove the unrecognized field"
		}
		add("config", doctorFail, err.Error(), fix)
		skipRemaining("skipped: configuration did not parse")
		return report
	}
	add("config", doctorPass, "parsed "+cfg.Path, "")

	status, err := deps.DaemonStatus(ctx, opt, cfg)
	if err != nil {
		add("daemon", doctorFail, err.Error(), "papio status")
		add("extension", doctorSkip, "skipped: daemon is unreachable", "")
		add("native host (Chrome)", doctorSkip, "skipped: daemon is unreachable", "")
		add("native host (Firefox)", doctorSkip, "skipped: daemon is unreachable", "")
		add("zotio", doctorSkip, "skipped: daemon is unreachable", "")
		return report
	}
	if status.Status != "ok" || strings.TrimSpace(status.Version) == "" {
		add("daemon", doctorFail, fmt.Sprintf("unexpected daemon status %q (version %q)", status.Status, status.Version), "papio status")
		add("extension", doctorSkip, "skipped: daemon status is invalid", "")
		add("native host (Chrome)", doctorSkip, "skipped: daemon status is invalid", "")
		add("native host (Firefox)", doctorSkip, "skipped: daemon status is invalid", "")
		add("zotio", doctorSkip, "skipped: daemon status is invalid", "")
		return report
	}
	if status.Version != api.Version {
		add("daemon", doctorWarn, fmt.Sprintf("reachable; daemon %s, CLI %s", status.Version, api.Version), "papio daemon stop (next command autostarts the new daemon)")
	} else {
		add("daemon", doctorPass, "reachable; version "+status.Version, "")
	}
	if status.ExtensionConnected {
		detail := "connected"
		if status.ExtensionVersion != "" {
			detail += " (v" + status.ExtensionVersion + ")"
		}
		add("extension", doctorPass, detail, "")
	} else {
		add("extension", doctorWarn, "extension has not connected since daemon start", "install and enable the browser extension, then run papio init to install the native-host manifest")
	}

	runManifestDoctorChecks(cfg, deps, add)

	preflight, err := deps.ZotioPreflight(ctx, cfg)
	if err != nil {
		add("zotio", doctorFail, err.Error(), "install or update zotio, then rerun papio doctor")
		return report
	}
	add("zotio", doctorPass, fmt.Sprintf("version %s; %d required capabilities available", preflight.Version, len(preflight.Capabilities)), "")
	return report
}

func doctorDependenciesComplete(deps doctorDependencies) bool {
	return deps.LoadConfig != nil && deps.DaemonStatus != nil && deps.ManifestDir != nil && deps.FirefoxDir != nil && deps.ReadFile != nil && deps.ZotioPreflight != nil
}

func runManifestDoctorChecks(cfg config.Config, deps doctorDependencies, add func(string, string, string, string)) {
	runManifestDoctorCheck("native host (Chrome)", cfg.Browser.ExtensionID, "chrome-extension://", deps.ManifestDir, deps.ReadFile, add)
	runManifestDoctorCheck("native host (Firefox)", cfg.Browser.FirefoxExtensionID, "", deps.FirefoxDir, deps.ReadFile, add)
}

func runManifestDoctorCheck(name, extensionID, originPrefix string, manifestDir func() (string, error), readFile func(string) ([]byte, error), add func(string, string, string, string)) {
	if extensionID == "" {
		add(name, doctorSkip, "skipped: extension ID is not configured", "")
		return
	}
	dir, err := manifestDir()
	if err != nil {
		add(name, doctorFail, err.Error(), "register the native host manually for this browser")
		return
	}
	path := filepath.Join(dir, nativeHostManifestName+".json")
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			add(name, doctorFail, "manifest is missing at "+path, "papio native-host install")
			return
		}
		add(name, doctorFail, fmt.Sprintf("reading manifest %s: %v", path, err), "papio native-host install")
		return
	}
	var manifest nativeHostManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		add(name, doctorFail, fmt.Sprintf("parsing manifest %s: %v", path, err), "papio native-host install")
		return
	}
	if originPrefix != "" {
		allowedOrigin := originPrefix + extensionID + "/"
		if !containsString(manifest.AllowedOrigins, allowedOrigin) {
			add(name, doctorFail, "manifest does not allow "+allowedOrigin, "papio native-host install")
			return
		}
	} else if !containsString(manifest.AllowedExtensions, extensionID) {
		add(name, doctorFail, "manifest does not allow "+extensionID, "papio native-host install")
		return
	}
	add(name, doctorPass, "manifest allows configured extension", "")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func renderIntegrationDoctorReport(out io.Writer, report integrationDoctorReport) error {
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out, "%-4s  %-24s %s\n", strings.ToUpper(check.Status), check.Name, check.Detail); err != nil {
			return err
		}
		if check.Fix != "" {
			if _, err := fmt.Fprintf(out, "      fix: %s\n", check.Fix); err != nil {
				return err
			}
		}
	}
	return nil
}

func renderDoctorReport(out io.Writer, report doctor.Report) error {
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out, "%-4s  %-24s %s\n", strings.ToUpper(check.Status), check.Name, check.Detail); err != nil {
			return err
		}
		if check.Remediation != "" {
			if _, err := fmt.Fprintf(out, "      %s\n", check.Remediation); err != nil {
				return err
			}
		}
	}
	return nil
}
