// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"papio/internal/batch"
	"papio/internal/bibparse"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/ipc"
	"papio/internal/work"
)

// flowDaemon answers the RPCs a batch preview or submit makes. Replies go
// through JSON so the mock needs no knowledge of the unexported batch types.
type flowDaemon struct {
	mu      sync.Mutex
	methods []string
	// searched counts discovery calls, to prove a refused save never pays for one.
	searched int
	works    []discovery.DiscoveredWork
	// refuseDOI names one work the daemon reports as already in flight under
	// another filing intent: submit_once_v1 answers it with the typed
	// once-only conflict instead of a job.
	refuseDOI string
}

func (d *flowDaemon) call(_ context.Context, method string, params, result any) error {
	d.mu.Lock()
	d.methods = append(d.methods, method)
	seq := len(d.methods)
	if method == "discovery.search" {
		d.searched++
	}
	d.mu.Unlock()
	var reply any
	switch method {
	case "zotio.lookup_works":
		raw, _ := json.Marshal(params)
		var req struct {
			Works []json.RawMessage `json:"works"`
		}
		_ = json.Unmarshal(raw, &req)
		works := make([]map[string]any, len(req.Works))
		for i := range works {
			works[i] = map[string]any{"status": "not_owned"}
		}
		reply = map[string]any{"works": works}
	case "acquire.submit_once_v1", "acquire.submit_v2":
		if method == "acquire.submit_once_v1" && d.isRefused(params) {
			return &ipc.RemoteError{Code: "conflict", Message: "once-only request conflict: once-only filing intent differs (collection) from live job job_live_1 for the same work"}
		}
		reply = map[string]any{"job_id": fmt.Sprintf("job_%d", seq)}
	case "jobs.get":
		reply = map[string]any{"job": map[string]any{"id": "job_x", "state": "queued"}}
	case "jobs.disposition":
		reply = map[string]any{"job_id": "job_x", "state": "queued", "disposition": "active", "changed": false}
	case "discovery.search":
		reply = d.works
	default:
		return fmt.Errorf("unexpected method %q", method)
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

// isRefused reports whether the daemon must refuse this submit_once work as
// already in flight under another filing intent.
func (d *flowDaemon) isRefused(params any) bool {
	if d.refuseDOI == "" {
		return false
	}
	raw, _ := json.Marshal(params)
	var req struct {
		Request struct {
			Identifiers *struct {
				DOI string `json:"doi"`
			} `json:"identifiers"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.Request.Identifiers == nil {
		return false
	}
	return req.Request.Identifiers.DOI == d.refuseDOI
}

func (d *flowDaemon) count(method string) int {
	n := 0
	for _, m := range d.methods {
		if m == method {
			n++
		}
	}
	return n
}

func runFlow(t *testing.T, cfg config.Config, d *flowDaemon, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, cfg, d.call)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func runFlowLookup(t *testing.T, cfg config.Config, d *flowDaemonFailingLookup, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, cfg, d.call)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestCampaignImportPreviewRunPauseResumeExecute(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = "zotio"
	export := filepath.Join(t.TempDir(), "refs.bib")
	bib := "@article{a,\n  doi = {10.1000/alpha},\n  title = {Alpha},\n  year = {2020}\n}\n@article{b,\n  doi = {10.1000/beta},\n  title = {Beta},\n  year = {2021}\n}\n"
	if err := os.WriteFile(export, []byte(bib), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &flowDaemon{}

	out, err := runFlow(t, cfg, d, "campaign", "import", export)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var imported struct {
		Manifest  campaignManifest `json:"manifest"`
		Ownership []string         `json:"ownership"`
	}
	if err := json.Unmarshal([]byte(out), &imported); err != nil {
		t.Fatalf("import output %q: %v", out, err)
	}
	id := imported.Manifest.ID
	if len(imported.Manifest.Works) != 2 || len(imported.Ownership) != 2 || d.count("acquire.submit_once_v1") != 0 {
		t.Fatalf("import = %d works, %d ownership rows, %d submits; want a 2-work preview with no submission", len(imported.Manifest.Works), len(imported.Ownership), d.count("acquire.submit_once_v1"))
	}

	if _, err := runFlow(t, cfg, d, "campaign", "preview", id); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if d.count("acquire.submit_once_v1") != 0 {
		t.Fatal("preview submitted jobs")
	}

	if _, err := runFlow(t, cfg, d, "campaign", "pause", id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := runFlow(t, cfg, d, "campaign", "run", id); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("run while paused: err = %v, want a paused refusal", err)
	}
	if d.count("acquire.submit_once_v1") != 0 {
		t.Fatal("a paused campaign submitted jobs")
	}

	if _, err := runFlow(t, cfg, d, "campaign", "resume", id); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, err := runFlow(t, cfg, d, "campaign", "run", id); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := d.count("acquire.submit_once_v1"); got != 2 {
		t.Fatalf("run submitted %d works, want 2", got)
	}
	out, err = runFlow(t, cfg, d, "campaign", "run", id)
	if err != nil || !strings.Contains(out, `"complete":true`) {
		t.Fatalf("second run = %q, %v; want the complete marker", out, err)
	}
	if got := d.count("acquire.submit_once_v1"); got != 2 {
		t.Fatalf("a finished campaign resubmitted: %d submits, want 2", got)
	}
}

func TestSearchSaveThenAcquireExecutes(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = "zotio"
	d := &flowDaemon{works: []discovery.DiscoveredWork{{Work: work.Work{DOI: "10.1000/alpha", Title: "Alpha", Year: 2020}}}}
	snapshotPath := filepath.Join(t.TempDir(), "snap.json")

	out, err := runFlow(t, cfg, d, "search", "save", snapshotPath, "alpha")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	var saved struct {
		Snapshot searchSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal([]byte(out), &saved); err != nil || len(saved.Snapshot.Rows) != 1 {
		t.Fatalf("save output %q: %v", out, err)
	}
	key := saved.Snapshot.Rows[0].Key

	if _, err := runFlow(t, cfg, d, "search", "acquire", snapshotPath, "--keys", key); err != nil {
		t.Fatalf("acquire preview: %v", err)
	}
	if d.count("acquire.submit_v2") != 0 {
		t.Fatal("acquire without --confirm submitted")
	}
	if _, err := runFlow(t, cfg, d, "search", "acquire", snapshotPath, "--keys", key, "--confirm"); err != nil {
		t.Fatalf("acquire --confirm: %v", err)
	}
	if d.count("acquire.submit_v2") != 1 {
		t.Fatalf("acquire --confirm submitted %d works, want 1", d.count("acquire.submit_v2"))
	}
	if _, err := runFlow(t, cfg, d, "search", "acquire", snapshotPath, "--keys", "nope"); err == nil {
		t.Fatal("an unknown key must be refused")
	}
}

// An existing snapshot path is refused before the discovery call, which can
// spend provider quota.
func TestSearchSaveRefusesExistingSnapshotBeforeSearching(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	d := &flowDaemon{}
	snapshotPath := filepath.Join(t.TempDir(), "snap.json")
	if err := os.WriteFile(snapshotPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runFlow(t, cfg, d, "search", "save", snapshotPath, "alpha"); err == nil {
		t.Fatal("save over an existing file must fail")
	}
	if d.searched != 0 {
		t.Fatalf("discovery ran %d time(s) before the refusal", d.searched)
	}
	if got, _ := os.ReadFile(snapshotPath); string(got) != "keep" {
		t.Fatalf("existing file = %q, want it untouched", got)
	}
}

func TestCampaignImportIdentifierList(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = "zotio"
	write := func(body string) string {
		path := filepath.Join(t.TempDir(), "ids.txt")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	out, err := runFlow(t, cfg, &flowDaemon{}, "campaign", "import", write("# list\n10.1000/alpha\narXiv:2601.12345\n\nPMID:12345678\n"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var imported struct {
		Manifest campaignManifest `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(out), &imported); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if len(imported.Manifest.Works) != 3 {
		t.Fatalf("works = %d, want 3", len(imported.Manifest.Works))
	}
	for _, body := range []string{"10.1000/alpha\nisbn:9780306406157\n", "10.1000/alpha\nW123456789\n"} {
		_, err := runFlow(t, cfg, &flowDaemon{}, "campaign", "import", write(body))
		if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "DOI, arXiv and PMID") {
			t.Fatalf("import %q: err = %v, want line 2 refusal", body, err)
		}
	}
}

// An interrupted run leaves the batch intent manifest without job receipts.
// Report must not echo the intent status for those rows: a job-less work in
// an inflight chunk reports failed/missing_job, matching batch report.
func TestCampaignReportInflightChunkWithoutJobsShowsFailedMissingJob(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = "zotio"
	export := filepath.Join(t.TempDir(), "refs.bib")
	bib := "@article{a,\n  doi = {10.1000/alpha},\n  title = {Alpha},\n  year = {2020}\n}\n@article{b,\n  doi = {10.1000/beta},\n  title = {Beta},\n  year = {2021}\n}\n@article{c,\n  doi = {10.1000/gamma},\n  title = {Gamma},\n  year = {2022}\n}\n"
	if err := os.WriteFile(export, []byte(bib), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &flowDaemon{}
	out, err := runFlow(t, cfg, d, "campaign", "import", export)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var imported struct {
		Manifest campaignManifest `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(out), &imported); err != nil {
		t.Fatalf("import output %q: %v", out, err)
	}
	id := imported.Manifest.ID
	if len(imported.Manifest.Chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(imported.Manifest.Chunks))
	}
	// Fail the ownership lookup so batch.Submit returns before any job exists,
	// after marking the chunk inflight and writing the campaign manifest.
	dFail := &flowDaemonFailingLookup{}
	out, err = runFlowLookup(t, cfg, dFail, "campaign", "run", id)
	if err == nil {
		t.Fatalf("run = %q, want the injected lookup failure", out)
	}
	stored, err := batch.Load(cfg.DataDir, imported.Manifest.Chunks[0].BatchID)
	if err != nil {
		t.Fatal(err)
	}
	// A partial run can also persist conclusive outcomes alongside intent.
	stored.Works[1].Status = "skipped_owned"
	stored.Works[2].Status = "submission_failed"
	stored.Works[2].Error = "submit"
	if err := batch.Write(cfg.DataDir, stored); err != nil {
		t.Fatal(err)
	}
	out, err = runFlow(t, cfg, d, "campaign", "report", id)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	var report struct {
		Records []struct {
			Outcome string `json:"outcome"`
			JobID   string `json:"job_id"`
			Reason  string `json:"reason"`
		} `json:"records"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("report output %q: %v", out, err)
	}
	if len(report.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(report.Records))
	}
	if report.Records[0].Outcome != "failed" || report.Records[0].JobID != "" || report.Records[0].Reason != "missing_job" {
		t.Fatalf("record = %+v, want failed/missing_job with no job id", report.Records[0])
	}
	if got := report.Records[1]; got.Outcome != "skipped_owned" || got.Reason != "" || got.JobID != "" {
		t.Fatalf("owned record = %+v, want skipped_owned with no reason or job id", got)
	}
	if got := report.Records[2]; got.Outcome != "submission_failed" || got.Reason != "submit" || got.JobID != "" {
		t.Fatalf("failed record = %+v, want submission_failed with its original submit reason", got)
	}
	if report.Summary["submitted"] != 0 || report.Summary["failed"] != 1 || report.Summary["skipped_owned"] != 1 || report.Summary["submission_failed"] != 1 {
		t.Fatalf("summary = %v, want one missing job, one owned work, one submission failure, and no submitted works", report.Summary)
	}
}

type flowDaemonFailingLookup struct{ flowDaemon }

func (d *flowDaemonFailingLookup) call(ctx context.Context, method string, params, result any) error {
	if method == "zotio.lookup_works" {
		return errors.New("injected ownership lookup failure")
	}
	return d.flowDaemon.call(ctx, method, params, result)
}

// TestCampaignRunAcknowledgesFilingIntentConflict pins the chunk-advance rule
// for the once-only refusal: a campaign chunk holding one work that is
// already in flight under another filing intent and 49 normal works is
// acknowledged anyway — the conflict is a recorded per-work outcome, not a
// chunk error — so the next run advances to the following chunk instead of
// retrying the refused chunk forever, and the report keeps the conflict
// visible against the live job.
func TestCampaignRunAcknowledgesFilingIntentConflict(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = "zotio"
	var bib strings.Builder
	for i := 1; i <= 51; i++ {
		fmt.Fprintf(&bib, "@article{w%03d,\n  doi = {10.1000/w%03d},\n  title = {Work %03d},\n  year = {2020}\n}\n", i, i, i)
	}
	export := filepath.Join(t.TempDir(), "refs.bib")
	if err := os.WriteFile(export, []byte(bib.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &flowDaemon{refuseDOI: "10.1000/w001"}
	out, err := runFlow(t, cfg, d, "campaign", "import", export)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var imported struct {
		Manifest campaignManifest `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(out), &imported); err != nil {
		t.Fatalf("import output %q: %v", out, err)
	}
	id := imported.Manifest.ID
	if len(imported.Manifest.Works) != 51 || len(imported.Manifest.Chunks) != 2 {
		t.Fatalf("import = %d works in %d chunks; want 51 works in 2 chunks", len(imported.Manifest.Works), len(imported.Manifest.Chunks))
	}
	out, err = runFlow(t, cfg, d, "campaign", "run", id)
	if err != nil {
		t.Fatalf("run chunk 1 with one conflicting work: %v", err)
	}
	var first struct {
		Chunk  int                 `json:"chunk"`
		Result *batch.SubmitOutput `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatalf("run output %q: %v", out, err)
	}
	if first.Chunk != 0 || first.Result == nil || first.Result.Failed != 1 || len(first.Result.Submitted) != 49 {
		t.Fatalf("chunk 1 result = %+v; want chunk 0 with 1 recorded refusal and 49 submissions", first.Result)
	}
	if got := d.count("acquire.submit_once_v1"); got != 50 {
		t.Fatalf("chunk 1 submitted %d works, want 50", got)
	}
	if _, err := runFlow(t, cfg, d, "campaign", "run", id); err != nil {
		t.Fatalf("run chunk 2: %v", err)
	}
	if got := d.count("acquire.submit_once_v1"); got != 51 {
		t.Fatalf("after chunk 2, submits = %d; want 51 (no retry of the refused chunk)", got)
	}
	out, err = runFlow(t, cfg, d, "campaign", "run", id)
	if err != nil || !strings.Contains(out, `"complete":true`) {
		t.Fatalf("third run = %q, %v; want the complete marker", out, err)
	}
	if got := d.count("acquire.submit_once_v1"); got != 51 {
		t.Fatalf("a finished campaign resubmitted: %d submits, want 51", got)
	}
	out, err = runFlow(t, cfg, d, "campaign", "report", id)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	var report struct {
		Records []struct {
			Citation bibparse.Record `json:"citation"`
			Outcome  string          `json:"outcome"`
			Reason   string          `json:"reason"`
		} `json:"records"`
		Summary map[string]int `json:"summary"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("report output %q: %v", out, err)
	}
	found := false
	for _, record := range report.Records {
		if record.Citation.DOI == "10.1000/w001" {
			found = true
			if record.Outcome != "submission_failed" || !strings.Contains(record.Reason, "job_live_1") {
				t.Fatalf("conflicting work = outcome %q reason %q; want submission_failed naming job_live_1", record.Outcome, record.Reason)
			}
		}
	}
	if !found {
		t.Fatal("conflicting work missing from the campaign report")
	}
	if report.Summary["submission_failed"] != 1 {
		t.Fatalf("report summary = %v; want exactly one submission_failed", report.Summary)
	}
}
