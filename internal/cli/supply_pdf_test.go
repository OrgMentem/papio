// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/config"
)

func supplyPDFConfig(t *testing.T, limit int64) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Fetch.MaxBytes = limit
	return cfg
}

// The daemon never receives a path: the CLI stages the file under the data
// directory and names only the staged file.
func TestSupplyPDFStagesTheFileAndSendsOnlyItsName(t *testing.T) {
	body := []byte("%PDF-1.7\nsupplied by the operator\n%%EOF")
	cfg := supplyPDFConfig(t, 1<<20)
	source := filepath.Join(t.TempDir(), "emailed.pdf")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	stageDir, err := app.SuppliedPDFStagingDir(cfg.DataDir, "job_01")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	var staged string
	root := NewInProcessRoot(&out, &errOut, cfg, func(_ context.Context, method string, params, result any) error {
		if method != "jobs.supply_pdf" {
			t.Fatalf("method = %q, want jobs.supply_pdf", method)
		}
		got := params.(map[string]string)
		if len(got) != 2 || got["job_id"] != "job_01" || got["name"] == "" || strings.ContainsAny(got["name"], `/\`) {
			t.Fatalf("params = %+v; want job_id and a bare staged name", got)
		}
		staged = filepath.Join(stageDir, got["name"])
		copied, err := os.ReadFile(staged)
		if err != nil || !bytes.Equal(copied, body) {
			t.Fatalf("staged copy = %q, %v; want the supplied bytes", copied, err)
		}
		*result.(*api.SupplyPDFResult) = api.SupplyPDFResult{JobID: "job_01", Outcome: app.AdoptionAccepted, State: "ready", SHA256: "abc"}
		return nil
	})
	root.SetArgs([]string{"jobs", "supply-pdf", "job_01", source})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "job_01\taccepted\tready\tabc\n" {
		t.Fatalf("output = %q", got)
	}
	if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged copy after the call: %v; want it removed", err)
	}
	if original, err := os.ReadFile(source); err != nil || !bytes.Equal(original, body) {
		t.Fatalf("original file = %q, %v; want it unchanged", original, err)
	}
}

func TestSupplyPDFRefusesInputBeforeCallingTheDaemon(t *testing.T) {
	const limit = 64
	dir := t.TempDir()
	pdf := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.7\nsmall\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notPDF := filepath.Join(dir, "page.html")
	if err := os.WriteFile(notPDF, []byte("<html>sign in</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(dir, "big.pdf")
	if err := os.WriteFile(oversized, append([]byte("%PDF-1.7\n"), make([]byte, limit)...), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.pdf")
	if err := os.Symlink(pdf, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		jobID string
		path  string
		want  string
	}{
		{"not a PDF", "job_01", notPDF, "not a PDF file"},
		{"directory", "job_01", dir, "is a directory"},
		{"symlink", "job_01", link, "symbolic link"},
		{"oversized", "job_01", oversized, "fetch.max_bytes"},
		{"missing", "job_01", filepath.Join(dir, "missing.pdf"), "no such file"},
		{"job id escapes", "../escape", pdf, "invalid job id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := supplyPDFConfig(t, limit)
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, cfg, func(context.Context, string, any, any) error {
				t.Fatal("a refused file must not reach the daemon")
				return nil
			})
			root.SetArgs([]string{"jobs", "supply-pdf", tc.jobID, tc.path})
			err := root.ExecuteContext(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("supply-pdf %s = %v; want an error naming %q", tc.path, err, tc.want)
			}
			// Every refusal comes before the copy, so nothing is staged.
			if entries, err := os.ReadDir(cfg.DataDir); err != nil || len(entries) != 0 {
				t.Fatalf("data directory after a refusal = %v, %v; want it empty", entries, err)
			}
		})
	}
}
