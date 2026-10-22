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
	"time"

	"papio/internal/api"
	"papio/internal/batch"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/job"
	"papio/internal/protocol"
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
		state := params.(map[string]any)["state"]
		switch state {
		case job.StateReady:
			*result.(*api.JobsPageV3) = api.JobsPageV3{Jobs: rows}
		case job.StateImported:
			// The imported job reached Zotero; the default must export it too.
			imported := exportTestRow("job-4", "10.5555/imported", "Imported work")
			imported.State = job.StateImported
			*result.(*api.JobsPageV3) = api.JobsPageV3{Jobs: []api.JobRow{imported}}
		default:
			t.Fatalf("state param = %v, want ready then imported", state)
		}
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
	if result.Format != "ris" || result.Records != 3 || result.DuplicatesCollapsed != 1 || result.SHA256 == "" || result.Output != path {
		t.Fatalf("result = %+v, want format inferred from .ris, one duplicate collapsed", result)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(payload); !strings.Contains(got, "TI  - The perils of plurality rule\r\n") || strings.Contains(got, "Same work, second job") || !strings.Contains(got, "Imported work") {
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

func TestRecordFromWorkRequestKeepsISBNAndOpenAlexIdentity(t *testing.T) {
	isbnA := recordFromWorkRequest(protocol.WorkRequest{Identifiers: &protocol.Identifiers{ISBN: "9780306406157"}})
	isbnB := recordFromWorkRequest(protocol.WorkRequest{Identifiers: &protocol.Identifiers{ISBN: "9780131103627"}})
	if isbnA.Identity() == isbnB.Identity() {
		t.Fatalf("distinct ISBN-only requests share identity %q", isbnA.Identity())
	}
	oaA := recordFromWorkRequest(protocol.WorkRequest{Identifiers: &protocol.Identifiers{OpenAlex: "W1"}})
	oaB := recordFromWorkRequest(protocol.WorkRequest{Identifiers: &protocol.Identifiers{OpenAlex: "W2"}})
	if oaA.Identity() == oaB.Identity() {
		t.Fatalf("distinct OpenAlex-only requests share identity %q", oaA.Identity())
	}
}

func exportSnapshotFixture(t *testing.T, dois ...string) string {
	t.Helper()
	hits := make([]discovery.DiscoveredWork, 0, len(dois))
	for i, doi := range dois {
		hits = append(hits, discovery.DiscoveredWork{Work: work.Work{
			Title: fmt.Sprintf("Snapshot work %d", i+1), Authors: []string{"Joshua Holzer"}, Year: 2020 + i, DOI: doi,
		}})
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := writeSearchSnapshot(path, discovery.SearchParams{Query: "plurality", Limit: 10}, hits); err != nil {
		t.Fatal(err)
	}
	return path
}

func runExport(t *testing.T, cfg config.Config, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, cfg, func(_ context.Context, method string, _ any, _ any) error {
		t.Fatalf("export must not call the daemon, called %q", method)
		return nil
	})
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), errOut.String(), err
}

func TestExportSearchEmitsOneRISRecordPerRow(t *testing.T) {
	path := exportSnapshotFixture(t, "10.1000/aaa", "10.1000/bbb", "10.1000/ccc")
	out, _, err := runExport(t, config.Config{}, "export", "search", path, "--format", "ris")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out, "TY  - "); got != 3 {
		t.Fatalf("RIS records = %d, want 3\n%s", got, out)
	}
	for _, doi := range []string{"10.1000/aaa", "10.1000/bbb", "10.1000/ccc"} {
		if !strings.Contains(out, "DO  - "+doi) {
			t.Fatalf("RIS lacks DOI %s\n%s", doi, out)
		}
	}
}

func TestExportSearchKeysSelectRowsAndRejectUnknown(t *testing.T) {
	path := exportSnapshotFixture(t, "10.1000/aaa", "10.1000/bbb")
	snapshot, err := readSearchSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runExport(t, config.Config{}, "export", "search", path, "--format", "ris", "--keys", snapshot.Rows[1].Key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "TY  - ") != 1 || !strings.Contains(out, "10.1000/bbb") || strings.Contains(out, "10.1000/aaa") {
		t.Fatalf("--keys output = %q, want only the second row", out)
	}
	if _, _, err := runExport(t, config.Config{}, "export", "search", path, "--keys", snapshot.Rows[0].Key+",nosuchkey"); err == nil || !strings.Contains(err.Error(), "nosuchkey") {
		t.Fatalf("unknown key error = %v, want it to name the key", err)
	}
}

func TestExportSearchCollapsesDuplicatesUnlessIncluded(t *testing.T) {
	// Two distinct snapshot rows (different titles) share one DOI.
	hits := []discovery.DiscoveredWork{
		{Work: work.Work{Title: "First copy", Authors: []string{"Joshua Holzer"}, Year: 2022, DOI: "10.1000/dup"}},
		{Work: work.Work{Title: "Second copy", Authors: []string{"Joshua Holzer"}, Year: 2022, DOI: "10.1000/dup"}},
	}
	path := filepath.Join(t.TempDir(), "snapshot.json")
	if err := writeSearchSnapshot(path, discovery.SearchParams{Query: "q", Limit: 10}, hits); err != nil {
		t.Fatal(err)
	}
	out, _, err := runExport(t, config.Config{}, "export", "search", path, "--format", "ris")
	if err != nil || strings.Count(out, "TY  - ") != 1 {
		t.Fatalf("collapsed export = %q, %v; want one record", out, err)
	}
	out, _, err = runExport(t, config.Config{}, "export", "search", path, "--format", "ris", "--include-duplicates")
	if err != nil || strings.Count(out, "TY  - ") != 2 {
		t.Fatalf("--include-duplicates export = %q, %v; want two records", out, err)
	}
}

func TestExportSearchOutputForceAndJSON(t *testing.T) {
	snap := exportSnapshotFixture(t, "10.1000/aaa")
	target := filepath.Join(t.TempDir(), "refs.ris")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runExport(t, config.Config{}, "export", "search", snap, "-o", target); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("overwrite without --force = %v", err)
	}
	if _, _, err := runExport(t, config.Config{}, "export", "search", snap); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runExport(t, config.Config{}, "--json", "export", "search", snap); err == nil || !strings.Contains(err.Error(), "-o") {
		t.Fatalf("--json without -o = %v", err)
	}
	out, _, err := runExport(t, config.Config{}, "--json", "export", "search", snap, "-o", target, "--force")
	if err != nil {
		t.Fatal(err)
	}
	var receipt exportResult
	if err := json.Unmarshal([]byte(out), &receipt); err != nil || !receipt.Replaced || receipt.Records != 1 || receipt.Format != "ris" {
		t.Fatalf("receipt = %+v, %v", receipt, err)
	}
	if got, _ := os.ReadFile(target); !strings.Contains(string(got), "DO  - 10.1000/aaa") {
		t.Fatalf("file = %q", got)
	}
}

func TestExportSearchUnknownSnapshotErrors(t *testing.T) {
	if _, _, err := runExport(t, config.Config{}, "export", "search", filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("want an error for a missing snapshot")
	}
}

func exportCampaignFixture(t *testing.T) (config.Config, string) {
	t.Helper()
	cfg := config.Config{DataDir: t.TempDir()}
	var works []protocol.WorkRequest
	for _, doi := range []string{"10.1000/aaa", "10.1000/bbb"} {
		request, err := batch.ParseWork(json.RawMessage(fmt.Sprintf(`{"doi":%q,"title":"Work %s","authors":["Joshua Holzer"],"year":2021}`, doi, doi)))
		if err != nil {
			t.Fatal(err)
		}
		works = append(works, request)
	}
	created := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	id := "campaign_export01"
	manifest := &campaignManifest{
		Version: 1, ID: id, CreatedAt: created.Format(time.RFC3339Nano),
		Records: []campaignRecord{
			{Ordinal: 1, WorkIndex: 0},
			{Ordinal: 2, WorkIndex: -1, Error: "no usable identifier or title"},
			{Ordinal: 3, WorkIndex: 1},
			{Ordinal: 4, WorkIndex: -1, Error: "unparseable"},
		},
		Works:  works,
		Chunks: []campaignChunk{{Start: 0, End: 2, BatchID: batch.ScopedID(works, created, id), State: "pending"}},
	}
	if err := writeCampaign(cfg.DataDir, manifest); err != nil {
		t.Fatal(err)
	}
	return cfg, id
}

func TestExportCampaignEmitsDistinctWorksAndReportsSkipped(t *testing.T) {
	cfg, id := exportCampaignFixture(t)
	out, errOut, err := runExport(t, cfg, "export", "campaign", id, "--format", "bibtex")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "@article{")+strings.Count(out, "@misc{") != 2 || !strings.Contains(out, "doi = {10.1000/aaa}") || !strings.Contains(out, "doi = {10.1000/bbb}") {
		t.Fatalf("bibtex = %q, want both works", out)
	}
	if !strings.Contains(errOut, "skipped 2") {
		t.Fatalf("stderr = %q, want a skipped-2 note", errOut)
	}

	target := filepath.Join(t.TempDir(), "c.bib")
	jsonOut, _, err := runExport(t, cfg, "--json", "export", "campaign", id, "-o", target)
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Records int `json:"records"`
		Skipped int `json:"skipped_source_records"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &receipt); err != nil || receipt.Records != 2 || receipt.Skipped != 2 {
		t.Fatalf("receipt = %+v, %v (%s)", receipt, err, jsonOut)
	}
}

func TestExportCampaignUnknownOrInvalidIDErrors(t *testing.T) {
	cfg, _ := exportCampaignFixture(t)
	if _, _, err := runExport(t, cfg, "export", "campaign", "campaign_nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown id = %v, want a not-found error", err)
	}
	if _, _, err := runExport(t, cfg, "export", "campaign", "../etc/passwd"); err == nil || !strings.Contains(err.Error(), "invalid campaign id") {
		t.Fatalf("bad id = %v, want an invalid-id error", err)
	}
}
