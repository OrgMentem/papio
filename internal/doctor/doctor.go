// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package doctor reports actionable, secret-free readiness checks for the
// Phase 1 acquisition core.
package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"papio/internal/config"
	"papio/internal/pdf"
	"papio/internal/store"
)

// Status values are stable CLI/agent output.
const (
	Pass = "pass"
	Warn = "warn"
	Fail = "fail"
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
