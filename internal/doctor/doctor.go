// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package doctor reports actionable, secret-free readiness checks for the
// Phase 1 acquisition core.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"papio/internal/config"
	"papio/internal/pdf"
	"papio/internal/store"
	"papio/internal/update"
	"papio/internal/zotio"
)

// Status values are stable CLI/agent output.
const (
	Pass = "pass"
	Warn = "warn"
	Fail = "fail"
	Skip = "skip"
)

// Check is one deterministic diagnostic. Detail never contains a credential.
type Check struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

// Report is the doctor command result.
type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// Run evaluates config, filesystem, database, executable, source credentials,
// and PDF helper capabilities. A nil store skips DB checks with a warning.
func Run(ctx context.Context, cfg config.Config, db *store.Store, capability pdf.Capability, workerBinary string) Report {
	var checks []Check
	add := func(name, status, detail, remediation string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
	}

	if _, err := cfg.RequireAccessMode(); err != nil {
		add("access_mode", Fail, "no explicit access mode is configured", "set access_mode to conservative, assisted, or maximal")
	} else {
		add("access_mode", Pass, "explicit access mode configured", "")
	}
	if cfg.Fetch.AllowHTTPLoopback {
		add("fetch_policy", Warn, "HTTP loopback development override is enabled", "disable fetch.allow_http_loopback outside fixture tests")
	} else {
		add("fetch_policy", Pass, "HTTPS-only production policy", "")
	}

	if err := checkDataDir(cfg.DataDir); err != nil {
		add("data_dir", Fail, "data directory is not private and writable", err.Error())
	} else {
		add("data_dir", Pass, "private writable data directory", "")
	}
	if cfg.Path != "" {
		if info, err := os.Stat(cfg.Path); err == nil {
			if info.Mode().Perm()&0o077 != 0 {
				add("config_permissions", Fail, "configuration is readable by group or others", "chmod 600 "+cfg.Path)
			} else {
				add("config_permissions", Pass, "configuration permissions are user-only", "")
			}
		} else if os.IsNotExist(err) {
			add("config_permissions", Warn, "configuration file does not exist", "create "+cfg.Path)
		} else {
			add("config_permissions", Fail, "configuration metadata cannot be read", "check file ownership and permissions")
		}
	}

	if db == nil {
		add("database", Warn, "database not opened for this doctor run", "run doctor through the daemon for integrity status")
	} else if err := db.IntegrityCheck(ctx); err != nil {
		add("database", Fail, "SQLite integrity check failed", "restore from a verified backup before acquisition")
	} else {
		version, err := db.UserVersion(ctx)
		if err != nil {
			add("database", Fail, "database schema version could not be read", "inspect database permissions")
		} else {
			add("database", Pass, fmt.Sprintf("SQLite integrity ok; schema version %d", version), "")
		}
	}

	if workerBinary == "" {
		add("pdf_worker", Fail, "papio worker executable path is missing", "run doctor from the papio binary")
	} else if info, err := os.Stat(workerBinary); err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		add("pdf_worker", Fail, "papio worker executable is not runnable", "install or rebuild papio and retry")
	} else {
		add("pdf_worker", Pass, "isolated pdfcpu worker is runnable", "")
	}
	if capability.PDFToText == "" {
		add("pdftotext", Fail, "Poppler pdftotext is unavailable", "install poppler")
	} else {
		add("pdftotext", Pass, "Poppler semantic extraction available", "")
	}
	if capability.PDFInfo == "" {
		add("pdfinfo", Warn, "Poppler pdfinfo cross-check is unavailable", "install poppler for independent page-count checks")
	} else {
		add("pdfinfo", Pass, "Poppler structural cross-check available", "")
	}
	if cfg.PDF.OCREnabled {
		if capability.PDFToPPM == "" || capability.Tesseract == "" {
			add("ocr", Fail, "OCR is enabled but pdftoppm or tesseract is unavailable", "install poppler and tesseract, or explicitly disable OCR")
		} else {
			add("ocr", Pass, "bounded OCR fallback available", "")
		}
	} else {
		add("ocr", Warn, "OCR fallback is explicitly disabled", "image-only papers will require review")
	}

	checkSourceCredentials(cfg, add)
	sort.SliceStable(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	out := Report{OK: true, Checks: checks}
	for _, c := range checks {
		if c.Status == Fail {
			out.OK = false
		}
	}
	return out
}

func checkDataDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("set data_dir")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("chmod %s to 0700: %w", path, err)
	}
	probe, err := os.CreateTemp(path, ".doctor-write-*")
	if err != nil {
		return fmt.Errorf("make %s writable by its owner", path)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func checkSourceCredentials(cfg config.Config, add func(string, string, string, string)) {
	if cfg.SourcePolicy(config.SourceUnpaywall).Enabled {
		if strings.TrimSpace(cfg.Email) == "" {
			add("source_unpaywall", Fail, "Unpaywall is enabled without a contact email", "set email in config.toml")
		} else {
			add("source_unpaywall", Pass, "Unpaywall contact identity configured", "")
		}
	}
	if cfg.SourcePolicy(config.SourceOpenAlex).Enabled {
		if strings.TrimSpace(cfg.Email) == "" || strings.TrimSpace(cfg.SourcePolicy(config.SourceOpenAlex).APIKey) == "" {
			add("source_openalex", Fail, "OpenAlex is enabled without contact email and API key", "set email and sources.openalex.api_key, or disable the source")
		} else {
			add("source_openalex", Pass, "OpenAlex credentials configured", "")
		}
	}
	for _, source := range []string{config.SourceCORE, config.SourceCrossrefTDM} {
		p := cfg.SourcePolicy(source)
		if !p.Enabled {
			continue
		}
		name := "source_" + strings.ReplaceAll(source, "_", "-")
		if strings.TrimSpace(p.APIKey) == "" {
			add(name, Fail, source+" is enabled without its API credential", "configure the API key/token, or disable the source")
		} else {
			add(name, Pass, source+" credential configured", "")
		}
	}
}

// DefaultWorkerPath resolves the current executable for pdf worker re-exec.
func DefaultWorkerPath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	path, _ = filepath.Abs(path)
	return path
}

// DaemonStatus is the daemon information returned by the ping status RPC.
type DaemonStatus struct {
	Status             string `json:"status"`
	Version            string `json:"version"`
	ExtensionConnected bool   `json:"extension_connected"`
	ExtensionVersion   string `json:"extension_version"`
}

// IntegrationDependencies supplies the local integration checks. The functions
// are deliberately independent of command-line plumbing so other callers can
// reuse RunIntegration.
type IntegrationDependencies struct {
	CLIVersion        string
	LoadConfig        func() (config.Config, error)
	DaemonStatus      func(context.Context, config.Config) (DaemonStatus, error)
	ManifestDir       func(config.Config) (string, error)
	FirefoxDir        func(config.Config) (string, error)
	ReadFile          func(string) ([]byte, error)
	ZotioPreflight    func(context.Context, config.Config) (*zotio.PreflightResult, error)
	CheckUpdates      func(context.Context, config.Config) (*update.Info, error)
	CheckZotioUpdates func(context.Context, config.Config) (*update.Info, error)
}

// RunIntegration checks the daemon, browser extension, native-host manifests,
// and zotio. Configuration is loaded by the supplied seam so this function can
// report parse errors as part of the same diagnostic report.
func RunIntegration(ctx context.Context, deps IntegrationDependencies) Report {
	report := Report{OK: true}
	add := func(name, status, detail, remediation string) {
		report.Checks = append(report.Checks, Check{
			Name: name, Status: status, Detail: detail, Remediation: remediation,
		})
		if status == Fail {
			report.OK = false
		}
	}
	skipRemaining := func(reason string) {
		add("daemon", Skip, reason, "")
		add("extension", Skip, reason, "")
		add("native host (Chrome)", Skip, reason, "")
		add("native host (Firefox)", Skip, reason, "")
		add("zotio", Skip, reason, "")
		add("updates (papio)", Skip, reason, "")
		add("updates (zotio)", Skip, reason, "")
	}

	if !integrationDependenciesComplete(deps) {
		add("doctor", Fail, "doctor command dependencies are incomplete", "reinstall papio")
		return report
	}
	cfg, err := deps.LoadConfig()
	if err != nil {
		remediation := "correct the configuration error above"
		if strings.Contains(strings.ToLower(err.Error()), "unknown field") {
			remediation = "update papio or remove the unrecognized field"
		}
		add("config", Fail, err.Error(), remediation)
		skipRemaining("skipped: configuration did not parse")
		return report
	}
	add("config", Pass, "parsed "+cfg.Path, "")

	status, err := deps.DaemonStatus(ctx, cfg)
	if err != nil {
		add("daemon", Fail, err.Error(), "papio status")
		skipRemainingAfterDaemon(add, "skipped: daemon is unreachable")
		runUpdateChecks(ctx, cfg, deps, nil, add)
		return report
	}
	if status.Status != "ok" || strings.TrimSpace(status.Version) == "" {
		add("daemon", Fail, fmt.Sprintf("unexpected daemon status %q (version %q)", status.Status, status.Version), "papio status")
		skipRemainingAfterDaemon(add, "skipped: daemon status is invalid")
		runUpdateChecks(ctx, cfg, deps, nil, add)
		return report
	}
	if status.Version != deps.CLIVersion {
		add("daemon", Warn, fmt.Sprintf("reachable; daemon %s, CLI %s", status.Version, deps.CLIVersion), "papio daemon stop (next command autostarts the new daemon)")
	} else {
		add("daemon", Pass, "reachable; version "+status.Version, "")
	}
	if status.ExtensionConnected {
		detail := "connected"
		if status.ExtensionVersion != "" {
			detail += " (v" + status.ExtensionVersion + ")"
		}
		add("extension", Pass, detail, "")
	} else {
		add("extension", Warn, "extension has not connected since daemon start", "install and enable the browser extension, then run papio init to install the native-host manifest")
	}

	runManifestChecks(cfg, deps, add)

	preflight, err := deps.ZotioPreflight(ctx, cfg)
	if err != nil || preflight == nil {
		detail := "zotio preflight returned no result"
		if err != nil {
			detail = err.Error()
		}
		preflight = nil
		add("zotio", Fail, detail, "install or update zotio, then rerun papio doctor")
	} else {
		add("zotio", Pass, fmt.Sprintf("version %s; %d required capabilities available", preflight.Version, len(preflight.Capabilities)), "")
		update.NewZotio(cfg.DataDir).RememberInstalledVersion(preflight.Version)
	}
	runUpdateChecks(ctx, cfg, deps, preflight, add)
	return report
}

func runUpdateChecks(ctx context.Context, cfg config.Config, deps IntegrationDependencies, preflight *zotio.PreflightResult, add func(string, string, string, string)) {
	if !cfg.Updates.Check {
		add("updates (papio)", Skip, "update check disabled ([updates] check = false)", "")
		add("updates (zotio)", Skip, "update check disabled ([updates] check = false)", "")
		return
	}
	runPapioUpdateCheck(ctx, cfg, deps, add)
	runZotioUpdateCheck(ctx, cfg, deps, preflight, add)
}

func runPapioUpdateCheck(ctx context.Context, cfg config.Config, deps IntegrationDependencies, add func(string, string, string, string)) {
	if deps.CheckUpdates == nil {
		add("updates (papio)", Skip, "skipped: update checker is not configured", "")
		return
	}
	info, err := deps.CheckUpdates(ctx, cfg)
	if err != nil || info == nil {
		add("updates (papio)", Warn, "could not check for papio updates", "rerun papio doctor later")
		return
	}
	if !update.IsNewer(info.LatestVersion, deps.CLIVersion) {
		add("updates (papio)", Pass, fmt.Sprintf("papio %s is current", deps.CLIVersion), "")
		return
	}
	executable, err := os.Executable()
	if err != nil {
		executable = ""
	}
	add(
		"updates (papio)",
		Warn,
		fmt.Sprintf("papio %s available (you have %s)", info.LatestVersion, deps.CLIVersion),
		update.UpgradeHint(executable, info.URL),
	)
}

func runZotioUpdateCheck(ctx context.Context, cfg config.Config, deps IntegrationDependencies, preflight *zotio.PreflightResult, add func(string, string, string, string)) {
	if preflight == nil || strings.TrimSpace(preflight.Version) == "" {
		add("updates (zotio)", Skip, "skipped: zotio preflight failed", "")
		return
	}
	if deps.CheckZotioUpdates == nil {
		add("updates (zotio)", Skip, "skipped: update checker is not configured", "")
		return
	}
	info, err := deps.CheckZotioUpdates(ctx, cfg)
	if err != nil || info == nil {
		add("updates (zotio)", Warn, "could not check for zotio updates", "rerun papio doctor later")
		return
	}
	if !update.IsNewer(info.LatestVersion, preflight.Version) {
		add("updates (zotio)", Pass, fmt.Sprintf("zotio %s is current", preflight.Version), "")
		return
	}
	add(
		"updates (zotio)",
		Warn,
		fmt.Sprintf("zotio %s available (you have %s)", info.LatestVersion, preflight.Version),
		update.UpgradeHintFor(cfg.Zotio.Executable, "zotio", info.URL),
	)
}

func integrationDependenciesComplete(deps IntegrationDependencies) bool {
	return deps.CLIVersion != "" && deps.LoadConfig != nil && deps.DaemonStatus != nil && deps.ManifestDir != nil && deps.FirefoxDir != nil && deps.ReadFile != nil && deps.ZotioPreflight != nil
}

func skipRemainingAfterDaemon(add func(string, string, string, string), reason string) {
	add("extension", Skip, reason, "")
	add("native host (Chrome)", Skip, reason, "")
	add("native host (Firefox)", Skip, reason, "")
	add("zotio", Skip, reason, "")
}

func runManifestChecks(cfg config.Config, deps IntegrationDependencies, add func(string, string, string, string)) {
	runManifestCheck("native host (Chrome)", cfg.Browser.ChromiumExtensionIDs(), "chrome-extension://", cfg, deps.ManifestDir, deps.ReadFile, add)
	var firefoxIDs []string
	if cfg.Browser.FirefoxExtensionID != "" {
		firefoxIDs = []string{cfg.Browser.FirefoxExtensionID}
	}
	runManifestCheck("native host (Firefox)", firefoxIDs, "", cfg, deps.FirefoxDir, deps.ReadFile, add)
}

const nativeHostManifestName = "com.orgmentem.papio"

type nativeHostManifest struct {
	AllowedOrigins    []string `json:"allowed_origins"`
	AllowedExtensions []string `json:"allowed_extensions"`
}

func runManifestCheck(name string, extensionIDs []string, originPrefix string, cfg config.Config, manifestDir func(config.Config) (string, error), readFile func(string) ([]byte, error), add func(string, string, string, string)) {
	if len(extensionIDs) == 0 {
		add(name, Skip, "skipped: extension ID is not configured", "")
		return
	}
	dir, err := manifestDir(cfg)
	if err != nil {
		add(name, Fail, err.Error(), "register the native host manually for this browser")
		return
	}
	path := filepath.Join(dir, nativeHostManifestName+".json")
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			add(name, Fail, "manifest is missing at "+path, "papio native-host install")
			return
		}
		add(name, Fail, fmt.Sprintf("reading manifest %s: %v", path, err), "papio native-host install")
		return
	}
	var manifest nativeHostManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		add(name, Fail, fmt.Sprintf("parsing manifest %s: %v", path, err), "papio native-host install")
		return
	}
	for _, extensionID := range extensionIDs {
		if originPrefix != "" {
			allowedOrigin := originPrefix + extensionID + "/"
			if !containsString(manifest.AllowedOrigins, allowedOrigin) {
				add(name, Fail, "manifest does not allow "+allowedOrigin, "papio native-host install")
				return
			}
		} else if !containsString(manifest.AllowedExtensions, extensionID) {
			add(name, Fail, "manifest does not allow "+extensionID, "papio native-host install")
			return
		}
	}
	add(name, Pass, "manifest allows configured extension", "")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
