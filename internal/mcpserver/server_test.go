// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerExposesBoundedPlanApplySurface(t *testing.T) {
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(nil).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "papio-test", Version: "1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var applyTool, exportTool, searchTool *mcp.Tool
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		switch tool.Name {
		case "papio_zotio_apply":
			applyTool = tool
		case "papio_export_bundle":
			exportTool = tool
		case "papio_search":
			searchTool = tool
		}
	}
	sort.Strings(names)
	want := []string{"papio_acquire", "papio_export_bundle", "papio_search", "papio_zotio_apply", "papio_zotio_plan"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	if applyTool == nil || applyTool.Annotations == nil || !applyTool.Annotations.IdempotentHint || applyTool.Annotations.ReadOnlyHint {
		t.Fatalf("apply annotations = %+v", applyTool)
	}
	schema, err := json.Marshal(applyTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), `"plan_id"`) || !strings.Contains(string(schema), `"confirmation_sha256"`) || strings.Contains(string(schema), `"job_id"`) {
		t.Fatalf("apply schema bypasses plan confirmation: %s", schema)
	}
	if exportTool == nil || exportTool.Annotations == nil || !exportTool.Annotations.ReadOnlyHint {
		t.Fatalf("export annotations = %+v", exportTool)
	}
	if searchTool == nil || searchTool.Annotations == nil || !searchTool.Annotations.ReadOnlyHint {
		t.Fatalf("search annotations = %+v", searchTool)
	}
	searchSchema, err := json.Marshal(searchTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(searchSchema), `"query"`) || !strings.Contains(string(searchSchema), `"oa_only"`) {
		t.Fatalf("search schema = %s", searchSchema)
	}

	resources, err := clientSession.ListResources(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, resource := range resources.Resources {
		uris = append(uris, resource.URI)
	}
	sort.Strings(uris)
	wantURIs := []string{"papio://artifacts", "papio://bundles", "papio://exports", "papio://jobs", "papio://zotio/plans"}
	if strings.Join(uris, ",") != strings.Join(wantURIs, ",") {
		t.Fatalf("resources = %v, want %v", uris, wantURIs)
	}
}
