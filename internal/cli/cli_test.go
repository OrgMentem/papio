// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/discovery"
	"papio/internal/ipc"
)

func TestNormalizeIdentifiersAcceptsCommonDOIAndArXivForms(t *testing.T) {
	ids, err := normalizeIdentifiers([]string{"https://doi.org/10.48550/arXiv.2601.12345"}, "", "", "", "", "")
	if err != nil || ids.DOI != "10.48550/arxiv.2601.12345" {
		t.Fatalf("DOI normalization = %+v, %v", ids, err)
	}
	ids, err = normalizeIdentifiers([]string{"arXiv:2601.12345v2"}, "", "", "", "", "")
	if err != nil || ids.ArXiv != "2601.12345v2" {
		t.Fatalf("arXiv normalization = %+v, %v", ids, err)
	}
}

func TestNormalizeIdentifiersRejectsAmbiguousOrMultipleInputs(t *testing.T) {
	if _, err := normalizeIdentifiers([]string{"not-an-id"}, "", "", "", "", ""); err == nil {
		t.Fatal("ambiguous identifier accepted")
	}
	if _, err := normalizeIdentifiers([]string{"10.1000/example"}, "10.1000/other", "", "", "", ""); err == nil {
		t.Fatal("positional plus explicit identifier accepted")
	}
}

func TestSearchCommandAllowsSnowballWithoutQuery(t *testing.T) {
	command := newSearchCommand(&options{})
	if err := command.Flags().Set("cites", "10.1000/seed"); err != nil {
		t.Fatal(err)
	}
	if err := command.Args(command, nil); err != nil {
		t.Fatalf("snowball search without query rejected: %v", err)
	}
	if err := command.Flags().Set("cites", ""); err != nil {
		t.Fatal(err)
	}
	if err := command.Args(command, nil); err == nil {
		t.Fatal("search without query or a snowball DOI succeeded")
	}
	for _, name := range []string{"cites", "cited-by", "related-to", "new-only"} {
		flag := command.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("missing --%s flag", name)
		}
	}
	if got := command.Flags().Lookup("cited-by").Usage; !strings.Contains(got, "backward references") || !strings.Contains(got, "cited_by:") {
		t.Fatalf("cited-by help = %q", got)
	}
}

func TestNewWorksOnlyFiltersOwnedResultsWithoutRefetching(t *testing.T) {
	works := []discovery.DiscoveredWork{
		{OpenAlexID: "one"},
		{OpenAlexID: "two", Owned: true, OwnedItemKey: "PDF00001"},
		{OpenAlexID: "three"},
	}
	filtered := newWorksOnly(works)
	if len(filtered) != 2 || filtered[0].OpenAlexID != "one" || filtered[1].OpenAlexID != "three" {
		t.Fatalf("filtered works = %+v", filtered)
	}
	if got := ownedSuffix(true); got != " [in library]" {
		t.Fatalf("owned suffix = %q", got)
	}
	if got := newSearchCommand(&options{}).Flags().Lookup("new-only").Usage; !strings.Contains(got, "after --limit") || !strings.Contains(got, "fewer") {
		t.Fatalf("new-only help = %q", got)
	}
}

func TestConfigInitWritesPrivateStructuredConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	var stdout, stderr bytes.Buffer
	root := NewRoot(&stdout, &stderr)
	root.SetArgs([]string{"--config", path, "--json", "config", "init", "--access-mode", "maximal", "--email", "reader@example.test"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config init: %v (%s)", err, stderr.String())
	}
	var output map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("JSON output: %v (%q)", err, stdout.String())
	}
	if output["access_mode"] != "maximal" || output["config_path"] != path {
		t.Fatalf("output = %v", output)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v", info.Mode().Perm())
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.AccessMode != config.ModeMaximal || cfg.Email != "reader@example.test" {
		t.Fatalf("loaded config = %+v, %v", cfg, err)
	}
}

func TestDaemonPingResultDecodesFullStatus(t *testing.T) {
	var result daemonPingResult
	if err := ipc.DecodeResult(json.RawMessage(`{"status":"ok","version":"1.2.3","extension_connected":true,"extension_version":"4.5.6"}`), &result); err != nil {
		t.Fatalf("decode ping result: %v", err)
	}
	if result.Status != "ok" || result.Version != "1.2.3" || !result.ExtensionConnected || result.ExtensionVersion != "4.5.6" {
		t.Fatalf("ping result = %+v", result)
	}
}

func TestCallWarnsOnceForVersionSkew(t *testing.T) {
	opt, _, stderr := versionWarningTestOptions(api.Version + "-old")
	for range 2 {
		if err := opt.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
			t.Fatalf("call: %v", err)
		}
	}
	want := "papio: daemon is running " + api.Version + "-old but this CLI is " + api.Version + " — run 'papio daemon stop'; the next command starts the matching daemon\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestCallDoesNotWarnWhenDaemonVersionMatches(t *testing.T) {
	opt, _, stderr := versionWarningTestOptions(api.Version)
	if err := opt.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestCallSkipsVersionWarningWhenItStartsDaemon(t *testing.T) {
	opt, _, stderr := versionWarningTestOptions(api.Version + "-old")
	started := false
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	opt.newAutostarter = func(socket string) *daemon.Autostarter {
		return &daemon.Autostarter{
			SocketPath: socket,
			LockPath:   filepath.Join(t.TempDir(), "daemon.lock"),
			LogPath:    logPath,
			Executable: func() (string, error) { return "/test/papio", nil },
			Command:    func(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) },
			Start: func(context.Context, *exec.Cmd) error {
				started = true
				return nil
			},
			Ready: func(context.Context, string) error {
				if started {
					return nil
				}
				return errors.New("not ready")
			},
		}
	}
	if err := opt.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if !started {
		t.Fatal("call did not start the daemon")
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestCallVersionWarningLeavesJSONOutputClean(t *testing.T) {
	opt, stdout, stderr := versionWarningTestOptions(api.Version + "-old")
	opt.jsonOutput = true
	if err := opt.call(context.Background(), "jobs.list", struct{}{}, &struct{}{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if err := opt.printResult(map[string]string{"status": "ok"}, "ignored"); err != nil {
		t.Fatalf("print JSON result: %v", err)
	}
	if got := stdout.String(); got != "{\"status\":\"ok\"}\n" {
		t.Fatalf("stdout = %q, want JSON only", got)
	}
	if got := stderr.String(); !strings.Contains(got, "papio: daemon is running ") {
		t.Fatalf("stderr = %q, want version warning", got)
	}
}

func versionWarningTestOptions(daemonVersion string) (*options, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	opt := &options{
		out:    &stdout,
		errOut: &stderr,
		configLoader: func(string) (config.Config, error) {
			return config.Config{DataDir: "/test/data"}, nil
		},
		newAutostarter: func(socket string) *daemon.Autostarter {
			return &daemon.Autostarter{
				SocketPath: socket,
				Ready:      func(context.Context, string) error { return nil },
			}
		},
		rpcCall: func(_ context.Context, _ string, method string, _ any, result any) error {
			if method == "ping" {
				result.(*daemonPingResult).Version = daemonVersion
			}
			return nil
		},
	}
	return opt, &stdout, &stderr
}
