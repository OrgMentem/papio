// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/watch"
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

func TestExportOutputRefusesToReplaceAnExistingFileWithoutForce(t *testing.T) {
	rows := []api.JobRow{exportTestRow("job-1", "10.1371/journal.pone.0262026", "The perils of plurality rule")}
	stub := func(_ context.Context, _ string, _ any, result any) error {
		*result.(*api.JobsPageV3) = api.JobsPageV3{Jobs: rows}
		return nil
	}
	path := filepath.Join(t.TempDir(), "refs.bib")
	const curated = "@article{hand-curated, title = {Keep me}}\n"
	if err := os.WriteFile(path, []byte(curated), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, stub)
	root.SetArgs([]string{"export", "ledger", "-o", path})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("export over an existing file = %v, want a refusal naming --force", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != curated {
		t.Fatalf("existing file after refusal = %q, %v; want it untouched", got, err)
	}

	out.Reset()
	root = NewInProcessRoot(&out, &errOut, config.Config{}, stub)
	root.SetArgs([]string{"--json", "export", "ledger", "-o", path, "--force"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("export --force: %v", err)
	}
	if got, _ := os.ReadFile(path); !strings.Contains(string(got), "doi = {10.1371/journal.pone.0262026}") || strings.Contains(string(got), "hand-curated") {
		t.Fatalf("file after --force = %q, want the export to replace it", got)
	}
	var receipt exportResult
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil || !receipt.Replaced {
		t.Fatalf("--force receipt = %+v, %v; want replaced reported", receipt, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("export dir holds %d entries, want only the export (no temp file left)", len(entries))
	}
}

func TestExportWatchRefusesAFullDigestPage(t *testing.T) {
	entries := make([]watch.DigestEntry, watch.DigestLimitMax)
	for i := range entries {
		entries[i] = watch.DigestEntry{WorkKey: fmt.Sprintf("doi:10.5555/%d", i), Title: "Pending work", DOI: fmt.Sprintf("10.5555/%d", i)}
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "watch.digest" {
			t.Fatalf("method = %q", method)
		}
		*result.(*api.WatchDigestResult) = api.WatchDigestResult{WatchID: 7, Entries: entries}
		return nil
	})
	path := filepath.Join(t.TempDir(), "watch.ris")
	root.SetArgs([]string{"export", "watch", "7", "-o", path})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("export watch with a full digest page = %v, want a fail-closed refusal", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("export file stat = %v, want no file written", err)
	}

	// One entry short of the page cap is provably complete and exports.
	entries = entries[:watch.DigestLimitMax-1]
	root = NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*api.WatchDigestResult) = api.WatchDigestResult{WatchID: 7, Entries: entries}
		return nil
	})
	root.SetArgs([]string{"--json", "export", "watch", "7", "-o", path})
	out.Reset()
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("export watch below the cap: %v", err)
	}
	var result exportResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Records != watch.DigestLimitMax-1 {
		t.Fatalf("receipt = %+v, %v; want every pending entry exported", result, err)
	}
}

func TestExportLedgerRefusesATruncatedPageUnlessSinceIsCovered(t *testing.T) {
	recent := exportTestRow("job-new", "10.5555/new", "Recent work")
	recent.CreatedAt = "2026-09-30T00:00:00Z"
	old := exportTestRow("job-old", "10.5555/old", "Old work")
	old.CreatedAt = "2026-01-01T00:00:00Z"
	run := func(rows []api.JobRow, args ...string) (string, error) {
		var out, errOut bytes.Buffer
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
			*result.(*api.JobsPageV3) = api.JobsPageV3{Jobs: rows, Truncated: true}
			return nil
		})
		path := filepath.Join(t.TempDir(), "ledger.ris")
		root.SetArgs(append([]string{"export", "ledger", "-o", path}, args...))
		err := root.ExecuteContext(context.Background())
		payload, _ := os.ReadFile(path)
		return string(payload), err
	}

	if payload, err := run([]api.JobRow{recent, old}); err == nil || payload != "" {
		t.Fatalf("truncated ledger without --since = %q, %v; want a refusal and no file", payload, err)
	}
	// The newest-first page reaches past the cutoff, so every job after it
	// is present even though the daemon truncated the page.
	if payload, err := run([]api.JobRow{recent, old}, "--since", "2026-06-01T00:00:00Z"); err != nil || !strings.Contains(payload, "10.5555/new") || strings.Contains(payload, "10.5555/old") {
		t.Fatalf("truncated ledger covering --since = %q, %v; want the recent work exported", payload, err)
	}
	if payload, err := run([]api.JobRow{recent}, "--since", "2026-06-01T00:00:00Z"); err == nil || payload != "" {
		t.Fatalf("truncated ledger not reaching --since = %q, %v; want a refusal and no file", payload, err)
	}
}
