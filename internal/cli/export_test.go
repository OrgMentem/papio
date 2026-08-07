// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/work"
)

func exportTestRow(id, doi, title string) api.JobRow {
	return api.JobRow{Row: job.Row{
		ID:        id,
		State:     job.StateReady,
		CreatedAt: "2026-08-01T00:00:00Z",
		Work: work.Work{
			Title: title, Authors: []string{"Joshua Holzer"}, Year: 2022,
			Container: "PLOS ONE", DOI: doi,
		},
	}}
}

func TestExportJobWritesCitationBytesToStdout(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		if method != "jobs.get_v2" {
			t.Fatalf("method = %q", method)
		}
		id := params.(map[string]string)["job_id"]
		row := exportTestRow(id, "10.1371/journal.pone.0262026", "The perils of plurality rule")
		*result.(*api.JobDetailV2) = api.JobDetailV2{Job: &row}
		return nil
	})
	root.SetArgs([]string{"export", "job", "job-1", "--format", "bibtex"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("export job: %v (%s)", err, errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "@article{holzer-2022-perils-") || !strings.Contains(got, "doi = {10.1371/journal.pone.0262026}") {
		t.Fatalf("stdout = %q, want BibTeX bytes", got)
	}
}

func TestExportLedgerJSONRequiresOutputAndReportsCollapse(t *testing.T) {
	rows := []api.JobRow{
		exportTestRow("job-1", "10.1371/journal.pone.0262026", "The perils of plurality rule"),
		exportTestRow("job-2", "10.1371/journal.pone.0262026", "Same work, second job"),
		exportTestRow("job-3", "10.5555/other", "A different work"),
	}
	stub := func(_ context.Context, method string, params any, result any) error {
		if method != "jobs.list_v3" {
			t.Fatalf("method = %q", method)
		}
		if state := params.(map[string]any)["state"]; state != job.StateReady {
			t.Fatalf("state param = %v, want the ready default", state)
		}
		*result.(*api.JobsPageV3) = api.JobsPageV3{Jobs: rows}
		return nil
	}

	// --json without -o is refused: stdout carries the result object, never
	// citation bytes mixed with papio JSON.
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, stub)
	root.SetArgs([]string{"--json", "export", "ledger"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "requires -o") {
		t.Fatalf("export ledger --json without -o = %v, want the -o requirement", err)
	}

	path := filepath.Join(t.TempDir(), "refs.ris")
	out.Reset()
	errOut.Reset()
	root = NewInProcessRoot(&out, &errOut, config.Config{}, stub)
	root.SetArgs([]string{"--json", "export", "ledger", "-o", path})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("export ledger: %v (%s)", err, errOut.String())
	}
	var result exportResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("result JSON: %v (%s)", err, out.String())
	}
	if result.Format != "ris" || result.Records != 2 || result.DuplicatesCollapsed != 1 || result.SHA256 == "" || result.Output != path {
		t.Fatalf("result = %+v, want format inferred from .ris, one duplicate collapsed", result)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(payload); !strings.Contains(got, "TI  - The perils of plurality rule\r\n") || strings.Contains(got, "Same work, second job") {
		t.Fatalf("file = %q, want the first occurrence kept and the duplicate collapsed", got)
	}
}

func TestExportLedgerIncludeDuplicatesKeepsScopeRows(t *testing.T) {
	rows := []api.JobRow{
		exportTestRow("job-1", "10.1371/journal.pone.0262026", "The perils of plurality rule"),
		exportTestRow("job-2", "10.1371/journal.pone.0262026", "Same work, second job"),
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*api.JobsPageV3) = api.JobsPageV3{Jobs: rows}
		return nil
	})
	root.SetArgs([]string{"export", "ledger", "--include-duplicates", "--format", "csl-json"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("export ledger: %v (%s)", err, errOut.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(out.Bytes(), &items); err != nil {
		t.Fatalf("CSL JSON: %v (%s)", err, out.String())
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want both scope rows retained", len(items))
	}
}
