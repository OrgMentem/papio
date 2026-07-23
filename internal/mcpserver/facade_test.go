// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// External test: exercises the command facade against a real papio CLI command
// tree wired to the in-process RPC router. Lives in mcpserver_test (not the
// internal package) because it imports cli, which imports mcpserver.
package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/bootstrap"
	"papio/internal/cli"
	"papio/internal/config"
	"papio/internal/mcpserver"
)

func newFacadeClient(t *testing.T) *client.Client {
	t.Helper()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	system, err := bootstrap.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = system.Close() })
	call := api.InProcessCaller(system)
	factory := func(out, errOut io.Writer) *cobra.Command {
		return cli.NewInProcessRoot(out, errOut, cfg, call)
	}
	c, err := client.NewInProcessClient(mcpserver.New(system, factory))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Initialize(ctx, mcplib.InitializeRequest{}); err != nil {
		t.Fatal(err)
	}
	return c
}

func runTool(t *testing.T, c *client.Client, name string, args map[string]any) *mcplib.CallToolResult {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func toolText(res *mcplib.CallToolResult) string {
	for _, content := range res.Content {
		if text, ok := content.(mcplib.TextContent); ok {
			return text.Text
		}
	}
	return ""
}

func TestFacadeSearchHidesLifecycleAndMarksReadOnly(t *testing.T) {
	c := newFacadeClient(t)
	res := runTool(t, c, "papio_command_search", map[string]any{})
	if res.IsError {
		t.Fatalf("command_search error: %s", toolText(res))
	}
	var commands []struct {
		Name     string `json:"name"`
		ReadOnly bool   `json:"read_only"`
	}
	if err := json.Unmarshal([]byte(toolText(res)), &commands); err != nil {
		t.Fatalf("decode search: %v (%s)", err, toolText(res))
	}
	readOnly := map[string]bool{}
	present := map[string]bool{}
	for _, cmd := range commands {
		present[cmd.Name] = true
		readOnly[cmd.Name] = cmd.ReadOnly
	}

	for _, want := range []string{"status", "search", "version", "acquire", "batch report", "watch list", "zotio apply"} {
		if !present[want] {
			t.Errorf("facade should expose %q; got %v", want, present)
		}
	}
	// Lifecycle/setup/system commands are hidden, along with their subtrees.
	for _, hidden := range []string{"daemon", "daemon stop", "mcp", "config", "config init", "init", "native-host", "native-host install"} {
		if present[hidden] {
			t.Errorf("facade should hide %q", hidden)
		}
	}
	// Help-only command groups carry an injected help RunE for the terminal
	// (cli.configureCommandGroups) but must not surface as tools, while their
	// real verbs stay exposed.
	for _, group := range []string{"jobs", "actions", "watch", "zotio"} {
		if present[group] {
			t.Errorf("facade should hide help-only group %q", group)
		}
	}
	for _, verb := range []string{"jobs get", "jobs list", "actions list", "watch digest"} {
		if !present[verb] {
			t.Errorf("facade should expose %q despite its help-only parent; got %v", verb, present)
		}
	}
	if !readOnly["status"] || !readOnly["search"] || !readOnly["version"] {
		t.Errorf("read-only commands mismarked: %v", readOnly)
	}
	if readOnly["acquire"] || readOnly["zotio apply"] {
		t.Errorf("mutating commands mismarked read-only: %v", readOnly)
	}
}

func TestFacadeSearchDetailListsLocalFlags(t *testing.T) {
	c := newFacadeClient(t)
	res := runTool(t, c, "papio_command_search", map[string]any{"name": "status"})
	if res.IsError {
		t.Fatalf("command_search detail error: %s", toolText(res))
	}
	var detail struct {
		Name     string `json:"name"`
		ReadOnly bool   `json:"read_only"`
		Flags    []struct {
			Name string `json:"name"`
		} `json:"flags"`
	}
	if err := json.Unmarshal([]byte(toolText(res)), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if !detail.ReadOnly {
		t.Errorf("status detail should be read-only")
	}
	hasFollow := false
	for _, flag := range detail.Flags {
		if flag.Name == "follow" {
			hasFollow = true
		}
		if flag.Name == "json" || flag.Name == "config" {
			t.Errorf("detail leaked inherited flag %q", flag.Name)
		}
	}
	if !hasFollow {
		t.Errorf("status detail missing local --follow flag: %+v", detail.Flags)
	}
}

func TestFacadeRunVersionEmitsJSON(t *testing.T) {
	c := newFacadeClient(t)
	res := runTool(t, c, "papio_command_run", map[string]any{"name": "version"})
	if res.IsError {
		t.Fatalf("command_run version error: %s", toolText(res))
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(toolText(res)), &out); err != nil {
		t.Fatalf("decode version: %v (%s)", err, toolText(res))
	}
	if out.Version == "" {
		t.Errorf("version output missing version: %s", toolText(res))
	}
}

func TestFacadeRunStatusRoutesThroughInProcessRPC(t *testing.T) {
	c := newFacadeClient(t)
	res := runTool(t, c, "papio_command_run", map[string]any{"name": "status"})
	if res.IsError {
		t.Fatalf("command_run status error: %s", toolText(res))
	}
	var snapshot struct {
		GeneratedAt string `json:"generated_at"`
	}
	if err := json.Unmarshal([]byte(toolText(res)), &snapshot); err != nil {
		t.Fatalf("decode status: %v (%s)", err, toolText(res))
	}
	if snapshot.GeneratedAt == "" {
		t.Errorf("status snapshot missing generated_at: %s", toolText(res))
	}
}

func TestFacadeRunRejectsUnknownFlagAndHiddenCommand(t *testing.T) {
	c := newFacadeClient(t)
	if res := runTool(t, c, "papio_command_run", map[string]any{"name": "status", "flags": map[string]any{"bogus": true}}); !res.IsError {
		t.Errorf("unknown flag should be rejected: %s", toolText(res))
	}
	if res := runTool(t, c, "papio_command_run", map[string]any{"name": "daemon"}); !res.IsError {
		t.Errorf("hidden command should not be runnable: %s", toolText(res))
	}
}
