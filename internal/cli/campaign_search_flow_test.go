// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"papio/internal/config"
	"papio/internal/discovery"
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
		reply = map[string]any{"job_id": fmt.Sprintf("job_%d", seq)}
	case "jobs.get":
		reply = map[string]any{"job": map[string]any{"id": "job_x", "state": "queued"}}
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
