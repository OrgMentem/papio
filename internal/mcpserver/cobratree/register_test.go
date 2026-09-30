// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// newRegisteredClient registers the command surface the way mcpserver.New
// does and returns a client that speaks to it over MCP, so these tests see
// the tools an agent host sees rather than the registration internals.
func newRegisteredClient(t *testing.T, surface string) *client.Client {
	t.Helper()
	t.Setenv("PAPIO_MCP_SURFACE", surface)
	s := server.NewMCPServer("papio-test", "0", server.WithToolCapabilities(false))
	Register(s, testFactory())
	c, err := client.NewInProcessClient(s)
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

func listTools(t *testing.T, c *client.Client) map[string]mcplib.Tool {
	t.Helper()
	res, err := c.ListTools(context.Background(), mcplib.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]mcplib.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	return tools
}

func callTool(t *testing.T, c *client.Client, name string, args map[string]any) *mcplib.CallToolResult {
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

func propertyType(t *testing.T, tool mcplib.Tool, name string) string {
	t.Helper()
	prop, ok := tool.InputSchema.Properties[name].(map[string]any)
	if !ok {
		t.Fatalf("%s has no %q parameter: %v", tool.Name, name, tool.InputSchema.Properties)
	}
	typ, _ := prop["type"].(string)
	return typ
}

func TestMirrorSurfaceRegistersOneTypedToolPerVisibleCommand(t *testing.T) {
	c := newRegisteredClient(t, " Mirror ")
	tools := listTools(t, c)
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	// Hidden subtrees, non-runnable parents and help-only groups get no tool,
	// and the facade tools are not registered beside the mirror.
	want := []string{"papio_fail", "papio_helponly_leaf", "papio_parent_child", "papio_readstdin", "papio_visible"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("mirror tools = %v, want %v", names, want)
	}

	visible, fail := tools["papio_visible"], tools["papio_fail"]
	if hint := visible.Annotations; hint.ReadOnlyHint == nil || !*hint.ReadOnlyHint || hint.DestructiveHint == nil || *hint.DestructiveHint {
		t.Errorf("read-only command annotations = %+v, want read-only and non-destructive", hint)
	}
	if hint := fail.Annotations; hint.ReadOnlyHint == nil || *hint.ReadOnlyHint || hint.DestructiveHint == nil || !*hint.DestructiveHint {
		t.Errorf("mutating command annotations = %+v, want destructive", hint)
	}
	if got := propertyType(t, visible, "limit"); got != "number" {
		t.Errorf("int flag type = %q, want number", got)
	}
	if got := propertyType(t, tools["papio_readstdin"], "batch"); got != "boolean" {
		t.Errorf("bool flag type = %q, want boolean", got)
	}
	child := tools["papio_parent_child"]
	if got := propertyType(t, child, "tag"); got != "string" {
		t.Errorf("string flag type = %q, want string", got)
	}
	if got := propertyType(t, child, "args"); got != "string" {
		t.Errorf("positional args type = %q, want string", got)
	}
	if _, ok := visible.InputSchema.Properties["args"]; ok {
		t.Error("command without positional args exposes an args parameter")
	}
	if _, ok := visible.InputSchema.Properties["json"]; ok {
		t.Error("inherited --json exposed as a tool parameter")
	}
}

func TestMirrorToolRunsTheCommandWithTypedFlagsAndArgs(t *testing.T) {
	c := newRegisteredClient(t, "mirror")
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		wantErr    bool
		want       string
	}{
		{name: "number flag", tool: "papio_visible", args: map[string]any{"limit": 5}, want: "json=true limit=5 args=[]"},
		{name: "string flag and positional", tool: "papio_parent_child", args: map[string]any{"tag": "x", "args": "alpha"}, want: "child args=[alpha] tag=x"},
		{name: "unknown flag", tool: "papio_visible", args: map[string]any{"bogus": true}, wantErr: true, want: "command does not expose --bogus"},
		{name: "inherited flag", tool: "papio_visible", args: map[string]any{"json": false}, wantErr: true, want: "command does not expose --json"},
		{name: "raw flag in args", tool: "papio_parent_child", args: map[string]any{"args": "alpha --tag y"}, wantErr: true, want: "raw flag"},
		{name: "command error", tool: "papio_fail", args: map[string]any{}, wantErr: true, want: "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, c, tc.tool, tc.args)
			if got := resultText(res); res.IsError != tc.wantErr || !strings.Contains(got, tc.want) {
				t.Fatalf("%s(%v) = error=%v %q, want error=%v containing %q", tc.tool, tc.args, res.IsError, got, tc.wantErr, tc.want)
			}
		})
	}
}

func TestFacadeSurfaceSearchesAndRunsCommands(t *testing.T) {
	c := newRegisteredClient(t, "")
	tools := listTools(t, c)
	if len(tools) != 2 {
		t.Fatalf("facade tools = %v, want only papio_command_search and papio_command_run", tools)
	}
	if hint := tools["papio_command_search"].Annotations; hint.ReadOnlyHint == nil || !*hint.ReadOnlyHint {
		t.Errorf("command search annotations = %+v, want read-only", hint)
	}
	if hint := tools["papio_command_run"].Annotations; hint.DestructiveHint == nil || !*hint.DestructiveHint {
		t.Errorf("command run annotations = %+v, want destructive", hint)
	}

	t.Run("query filters by name and summary", func(t *testing.T) {
		res := callTool(t, c, "papio_command_search", map[string]any{"query": "LEAF"})
		var got []commandSummary
		if err := json.Unmarshal([]byte(resultText(res)), &got); res.IsError || err != nil {
			t.Fatalf("search = error=%v %q (%v)", res.IsError, resultText(res), err)
		}
		names := make([]string, 0, len(got))
		for _, cmd := range got {
			names = append(names, cmd.Name)
		}
		if want := []string{"helponly leaf", "parent child"}; !reflect.DeepEqual(names, want) {
			t.Fatalf("query matched %v, want %v", names, want)
		}
	})

	t.Run("name returns local flags and positional intent", func(t *testing.T) {
		res := callTool(t, c, "papio_command_search", map[string]any{"name": "parent child"})
		var got commandDetail
		if err := json.Unmarshal([]byte(resultText(res)), &got); res.IsError || err != nil {
			t.Fatalf("search detail = error=%v %q (%v)", res.IsError, resultText(res), err)
		}
		if !got.TakesArgs || got.ReadOnly || len(got.Flags) != 1 || got.Flags[0].Name != "tag" || got.Flags[0].Type != "string" {
			t.Fatalf("detail = %+v, want takes_args, mutating, one string --tag flag", got)
		}
	})

	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		wantErr    bool
		want       string
	}{
		{name: "search unknown name", tool: "papio_command_search", args: map[string]any{"name": "hidden secret"}, wantErr: true, want: "command not found: hidden secret"},
		{name: "run with flags and args", tool: "papio_command_run", args: map[string]any{"name": "parent child", "flags": map[string]any{"tag": "x"}, "args": "alpha"}, want: "child args=[alpha] tag=x"},
		{name: "run number flag", tool: "papio_command_run", args: map[string]any{"name": "visible", "flags": map[string]any{"limit": 3}}, want: "json=true limit=3 args=[]"},
		{name: "run missing name", tool: "papio_command_run", args: map[string]any{}, wantErr: true, want: "requires name"},
		{name: "run unknown command", tool: "papio_command_run", args: map[string]any{"name": "nope"}, wantErr: true, want: "command not found: nope"},
		{name: "run non-object flags", tool: "papio_command_run", args: map[string]any{"name": "visible", "flags": "--limit 3"}, wantErr: true, want: "flags must be an object"},
		{name: "run unknown flag", tool: "papio_command_run", args: map[string]any{"name": "visible", "flags": map[string]any{"bogus": true}}, wantErr: true, want: "command does not expose --bogus"},
		{name: "run raw flag in args", tool: "papio_command_run", args: map[string]any{"name": "parent child", "args": "--tag y"}, wantErr: true, want: "raw flag"},
		{name: "run command error", tool: "papio_command_run", args: map[string]any{"name": "fail"}, wantErr: true, want: "partial output\nboom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, c, tc.tool, tc.args)
			if got := resultText(res); res.IsError != tc.wantErr || !strings.Contains(got, tc.want) {
				t.Fatalf("%s(%v) = error=%v %q, want error=%v containing %q", tc.tool, tc.args, res.IsError, got, tc.wantErr, tc.want)
			}
		})
	}
}
