// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/pdf"
	"papio/internal/store"
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
		if c.Name == "database" && c.Status == Pass && strings.Contains(c.Detail, "schema version 5") {
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
