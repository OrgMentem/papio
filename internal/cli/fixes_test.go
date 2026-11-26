package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/work"
)

func TestSearchMissingQueryIsUsageError(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	_, err := runFlowText(t, cfg, &flowDaemon{}, "search")
	if err == nil || exitCode(err) != exitUsage {
		t.Fatalf("err = %v, exit %d; want usage exit %d", err, exitCode(err), exitUsage)
	}
	if code := classifyError(err).Code; code != errorCodeInvalidArgument {
		t.Fatalf("error code = %q, want %q", code, errorCodeInvalidArgument)
	}
	_, err = runFlowText(t, cfg, &flowDaemon{}, "search", "save", filepath.Join(t.TempDir(), "s.json"), " ")
	if err == nil || exitCode(err) != exitUsage || classifyError(err).Code != errorCodeInvalidArgument {
		t.Fatalf("save err = %v, exit %d", err, exitCode(err))
	}
}

func TestInvalidWatchIDAndIncompatibleAcquireFlagsAreUsageErrors(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	for _, args := range [][]string{
		{"export", "watch", "not-a-number", "-o", filepath.Join(t.TempDir(), "x.json")},
		{"acquire", "--keys", "a"},
		{"acquire", "--batch", "-", "--from-zotio"},
		{"acquire", "--batch", "-", "--wait"},
		{"acquire", "--from-digest", "3", "--from-zotio"},
		{"acquire", "--from-zotio", "--wait"},
	} {
		_, err := runFlowText(t, cfg, &flowDaemon{}, args...)
		if err == nil || exitCode(err) != exitUsage || classifyError(err).Code != errorCodeInvalidArgument {
			t.Errorf("%v: err = %v, exit %d, code %q", args, err, exitCode(err), classifyError(err).Code)
		}
	}
}

func TestStatusFollowJSONIsOneCompleteDocumentPerLineWithoutAggregate(t *testing.T) {
	old := statusFollowInterval
	statusFollowInterval = time.Millisecond
	t.Cleanup(func() { statusFollowInterval = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lists int
	call := func(_ context.Context, method string, _, result any) error {
		switch method {
		case "jobs.list_v2":
			lists++
			if lists >= 3*(1+len(statusActiveStates)) {
				cancel() // stop after the third refresh
			}
			return json.Unmarshal([]byte(`{"jobs":[],"truncated":false}`), result)
		}
		return errors.New("unavailable")
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Default(), call)
	root.SetArgs([]string{"--json", "status", "--follow"})
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("got %d snapshot(s), want at least 2: %q", len(lines), out.String())
	}
	for _, line := range lines {
		var snapshot map[string]any
		if err := json.Unmarshal([]byte(line), &snapshot); err != nil || snapshot["generated_at"] == nil {
			t.Fatalf("line is not one complete snapshot: %q (%v)", line, err)
		}
	}
}

func TestCampaignListProseAndJSON(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = "zotio"
	input := filepath.Join(t.TempDir(), "ids.txt")
	if err := os.WriteFile(input, []byte("10.1000/alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runFlowText(t, cfg, &flowDaemon{}, "campaign", "list"); err != nil || strings.TrimSpace(out) != "No campaigns." {
		t.Fatalf("empty prose = %q, %v", out, err)
	}
	if _, err := runFlow(t, cfg, &flowDaemon{}, "campaign", "import", input); err != nil {
		t.Fatal(err)
	}
	out, err := runFlowText(t, cfg, &flowDaemon{}, "campaign", "list")
	if err != nil || json.Valid([]byte(out)) || !strings.Contains(out, "campaign_") || !strings.Contains(out, "1 works") {
		t.Fatalf("prose = %q, %v", out, err)
	}
	out, err = runFlow(t, cfg, &flowDaemon{}, "campaign", "list")
	var page struct {
		Campaigns []campaignSummary `json:"campaigns"`
	}
	if err != nil || json.Unmarshal([]byte(out), &page) != nil || len(page.Campaigns) != 1 {
		t.Fatalf("json = %q, %v", out, err)
	}
}

func TestWriteSearchSnapshotInterruptedBeforePublishLeavesNoFinalFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.json")
	hits := []discovery.DiscoveredWork{{Work: work.Work{DOI: "10.1000/a", Title: "A"}}}
	snapshotBeforePublish = func() { panic("simulated kill") }
	func() {
		defer func() { recover() }()
		_ = writeSearchSnapshot(path, discovery.SearchParams{Query: "q"}, hits)
	}()
	snapshotBeforePublish = nil
	if _, err := os.Lstat(path); err == nil {
		t.Fatal("interrupted save left a final snapshot file")
	}
	if err := writeSearchSnapshot(path, discovery.SearchParams{Query: "q"}, hits); err != nil {
		t.Fatalf("retry after interruption: %v", err)
	}
	if _, err := readSearchSnapshot(path); err != nil {
		t.Fatal(err)
	}
}

func TestSearchSaveAndAcquireHonourJSONFlag(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = "zotio"
	d := &flowDaemon{works: []discovery.DiscoveredWork{{Work: work.Work{DOI: "10.1000/alpha", Title: "Alpha", Year: 2020}}}}
	path := filepath.Join(t.TempDir(), "snap.json")
	out, err := runFlowText(t, cfg, d, "search", "save", path, "alpha")
	if err != nil || json.Valid([]byte(out)) || !strings.Contains(out, "Saved 1 candidate") {
		t.Fatalf("plain save output = %q, %v", out, err)
	}
	snap, err := readSearchSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err = runFlowText(t, cfg, d, "search", "acquire", path, "--keys", snap.Rows[0].Key)
	if err != nil || json.Valid([]byte(out)) || !strings.Contains(out, "nothing submitted") {
		t.Fatalf("plain acquire output = %q, %v", out, err)
	}
	for _, rowText := range []string{"Alpha", "ownership: not_owned", "Cost policy:", "Auto-import: false"} {
		if !strings.Contains(out, rowText) {
			t.Fatalf("plain acquire preview lacks %q: %q", rowText, out)
		}
	}
	out, err = runFlow(t, cfg, d, "--json=false", "search", "acquire", path, "--keys", snap.Rows[0].Key)
	if err != nil || json.Valid([]byte(out)) {
		t.Fatalf("--json=false acquire output = %q, %v", out, err)
	}
	out, err = runFlow(t, cfg, d, "--json", "search", "acquire", path, "--keys", snap.Rows[0].Key)
	if err != nil || !json.Valid([]byte(out)) {
		t.Fatalf("json acquire output = %q, %v", out, err)
	}
}

func TestWriteSearchSnapshotIgnoresCrashResidueAndKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	// Residue of a killed save: a partial temp sibling must not block a save.
	if err := os.WriteFile(filepath.Join(dir, ".papio-snapshot-dead"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "snap.json")
	hits := []discovery.DiscoveredWork{{Work: work.Work{DOI: "10.1000/a", Title: "A"}}}
	if err := writeSearchSnapshot(path, discovery.SearchParams{Query: "q"}, hits); err != nil {
		t.Fatal(err)
	}
	if _, err := readSearchSnapshot(path); err != nil {
		t.Fatalf("published snapshot unreadable: %v", err)
	}
	before, _ := os.ReadFile(path)
	if err := writeSearchSnapshot(path, discovery.SearchParams{Query: "other"}, hits); err == nil {
		t.Fatal("existing snapshot was overwritten")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("existing snapshot changed")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "snap.json" && e.Name() != ".papio-snapshot-dead" {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestConfigInitForceJSONReplacedIsBoolean(t *testing.T) {
	home := cliPrivateTestDir(t)
	isolateCLIProfile(t, home)
	path := filepath.Join(home, "config", "config.toml")
	run := func(args ...string) string {
		var stdout bytes.Buffer
		root := NewRoot(&stdout, &bytes.Buffer{})
		root.SetArgs(append([]string{"--config", path, "--json"}, args...))
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return stdout.String()
	}
	for i, want := range []bool{false, true} {
		args := []string{"config", "init", "--access-mode", "conservative"}
		if i == 1 {
			args = append(args, "--force")
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(run(args...)), &got); err != nil {
			t.Fatal(err)
		}
		if v, ok := got["replaced"].(bool); !ok || v != want {
			t.Fatalf("receipt %d replaced = %#v, want boolean %v", i, got["replaced"], want)
		}
	}
}

func TestStatusFollowHelpDocumentsJSONFraming(t *testing.T) {
	cmd := newStatusCommand(&options{})
	if usage := cmd.Flags().Lookup("follow").Usage; !strings.Contains(usage, "one compact JSON snapshot per line") {
		t.Fatalf("--follow usage = %q", usage)
	}
}

func TestCampaignRunReportsReceiptWhenManifestPersistenceFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX directory permissions and a non-root user")
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = "zotio"
	d := &flowDaemon{}
	input := filepath.Join(t.TempDir(), "ids.txt")
	if err := os.WriteFile(input, []byte("10.1000/alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runFlow(t, cfg, d, "--json", "campaign", "import", input); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.DataDir, "campaigns")
	entries, _ := os.ReadDir(dir)
	var id string
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".json") {
			id = strings.TrimSuffix(n, ".json")
		}
	}
	if id == "" {
		t.Fatal("no campaign manifest written")
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	call := func(ctx context.Context, method string, params, result any) error {
		err := d.call(ctx, method, params, result)
		if strings.HasPrefix(method, "acquire.submit") {
			os.Chmod(dir, 0o500) // the post-submit manifest write now fails
		}
		return err
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, cfg, call)
	root.SetArgs([]string{"--json", "campaign", "run", id})
	err := root.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("expected persistence error")
	}
	if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), `"campaign_id"`) || !strings.Contains(out.String(), `"job_id"`) {
		t.Fatalf("no submission receipt with an accepted job id on stdout: %q", out.String())
	}
	if msg := err.Error(); !strings.Contains(msg, "inflight") || !strings.Contains(msg, "campaign report "+id) {
		t.Fatalf("error lacks reconcile guidance: %v", err)
	}
	os.Chmod(dir, 0o700)
	var persisted struct {
		Chunks []campaignChunk `json:"chunks"`
	}
	raw, _ := os.ReadFile(filepath.Join(dir, id+".json"))
	if json.Unmarshal(raw, &persisted) != nil || len(persisted.Chunks) != 1 || persisted.Chunks[0].State != "inflight" {
		t.Fatalf("persisted chunks = %+v, want one inflight chunk", persisted.Chunks)
	}
}
