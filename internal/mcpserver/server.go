// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package mcpserver exposes papio through the Model Context Protocol.
//
// The command surface derives from the CLI command tree (see
// internal/mcpserver/cobratree): papio_command_search + papio_command_run
// collapse the whole tree behind a facade so the CLI stays the single source of
// truth and no typed tool layer drifts from it. Two composite tools remain
// hand-written because they have no single-command equivalent:
// papio_acquire_batch (bulk work input — the stdin path of `acquire --batch`,
// which MCP cannot supply) and papio_batch_wait (a bounded polling loop).
// Read-only JSON resources round out the surface.
package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"papio/internal/api"
	"papio/internal/batch"
	"papio/internal/bootstrap"
	"papio/internal/job"
	"papio/internal/mcpserver/cobratree"
	"papio/internal/protocol"
)

const serverInstructions = "Acquire papers into durable papio jobs. Discover and run CLI commands with papio_command_search then papio_command_run (output is JSON). Submit many works at once with papio_acquire_batch, then await outcomes with papio_batch_wait. To import into Zotero, run `zotio plan <job-id>` via papio_command_run, then `zotio apply <plan-id>` with the plan's confirm-sha256 flag; apply is the only path that mutates Zotero."

const (
	defaultBatchWaitTimeoutSeconds = 300
	maxBatchWaitTimeoutSeconds     = 600
	defaultBatchWaitPollSeconds    = 5
)

// callerFunc adapts the in-process RPC caller to the batch.Caller interface and
// to direct method calls.
type callerFunc func(context.Context, string, any, any) error

func (f callerFunc) Call(ctx context.Context, method string, params, result any) error {
	return f(ctx, method, params, result)
}

type toolDependencies struct {
	caller callerFunc
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
}

// BatchWaitOutput returns the final batch report and whether it fully settled.
type BatchWaitOutput struct {
	Report  *batch.Report `json:"report"`
	Settled bool          `json:"settled"`
}

// New builds the papio MCP server. factory supplies a fresh in-process CLI
// command tree for the cobratree facade. The embedding cli package provides it:
// cli imports mcpserver, so the command-tree dependency must flow inward this
// way rather than mcpserver importing cli (which would cycle).
func New(system *bootstrap.System, factory cobratree.RootFactory) *server.MCPServer {
	return newServer(system, factory, toolDependencies{
		caller: callerFunc(api.InProcessCaller(system)),
		now:    time.Now,
		wait:   waitForPoll,
	})
}

// newServer builds the server with injectable tool dependencies so tests can
// supply a fake caller and clock.
func newServer(system *bootstrap.System, factory cobratree.RootFactory, deps toolDependencies) *server.MCPServer {
	s := server.NewMCPServer("papio", api.Version,
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, true),
		server.WithInstructions(serverInstructions),
	)
	registerCompositeTools(s, system, deps)
	registerResources(s, system)
	cobratree.Register(s, factory)
	return s
}

// Run serves the papio MCP server over stdio until the context is cancelled.
func Run(ctx context.Context, system *bootstrap.System, factory cobratree.RootFactory) error {
	s := New(system, factory)
	// Stdio uses stdout as the JSON-RPC transport. Route any stray process
	// stdout writes to stderr and hand the real stdout only to the transport.
	stdout := os.Stdout
	os.Stdout = os.Stderr
	return server.NewStdioServer(s).Listen(ctx, os.Stdin, stdout)
}

func registerCompositeTools(s *server.MCPServer, system *bootstrap.System, deps toolDependencies) {
	s.AddTool(mcplib.NewTool("papio_acquire_batch",
		mcplib.WithDescription("Create a durable acquisition batch (1-50 works) with papio's ownership routing, deterministic request IDs, auto-import policy, and manifest. Bulk-input equivalent of `acquire --batch`, whose stdin path is unavailable over MCP."),
		mcplib.WithArray("works", mcplib.Required(), mcplib.Description("1 to 50 bare work objects or discovered-work envelopes containing a work field.")),
		mcplib.WithBoolean("auto_import", mcplib.Description("Automatically import ready jobs into Zotero; defaults to true.")),
		mcplib.WithString("collection", mcplib.Description("Target Zotero collection for every work; defaults to the label.")),
		mcplib.WithString("resolver", mcplib.Description("Named institutional OpenURL resolver profile for every work.")),
		mcplib.WithString("label", mcplib.Description("Batch query context; also the default collection when collection is unset.")),
		mcplib.WithBoolean("include_owned", mcplib.Description("Submit works already carrying a PDF in Zotio; defaults to false.")),
		mcplib.WithDestructiveHintAnnotation(false),
	), handleAcquireBatch(system, deps))

	s.AddTool(mcplib.NewTool("papio_batch_wait",
		mcplib.WithDescription("Poll a batch report until every work has a settled outcome (including an explicit human-review outcome) or the bounded timeout expires. Does not submit, import, or resolve work."),
		mcplib.WithString("batch_id", mcplib.Required(), mcplib.Description("Batch ID returned by papio_acquire_batch, or \"latest\".")),
		mcplib.WithNumber("timeout_seconds", mcplib.Description("Maximum wait in seconds, 1 to 600; 0 or omitted defaults to 300.")),
		mcplib.WithNumber("poll_seconds", mcplib.Description("Seconds between reports; defaults to 5.")),
		mcplib.WithReadOnlyHintAnnotation(true),
	), handleBatchWait(deps))
}

func handleAcquireBatch(system *bootstrap.System, deps toolDependencies) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if system == nil || strings.TrimSpace(system.Config.DataDir) == "" {
			return mcplib.NewToolResultError("batch acquisition is not configured"), nil
		}
		args := req.GetArguments()
		rawWorks, _ := args["works"].([]any)
		if len(rawWorks) == 0 {
			return mcplib.NewToolResultError("works must contain 1 to 50 entries"), nil
		}
		if len(rawWorks) > 50 {
			return mcplib.NewToolResultError("batch exceeds maximum of 50 works"), nil
		}
		requests := make([]protocol.WorkRequest, len(rawWorks))
		for i, item := range rawWorks {
			raw, err := json.Marshal(item)
			if err != nil {
				return mcplib.NewToolResultError(fmt.Sprintf("encoding batch work %d: %v", i+1, err)), nil
			}
			request, err := batch.ParseWork(raw)
			if err != nil {
				return mcplib.NewToolResultError(fmt.Sprintf("batch work %d: %v", i+1, err)), nil
			}
			requests[i] = request
		}
		autoImport := true
		if value, ok := args["auto_import"].(bool); ok {
			autoImport = value
		}
		collection, _ := args["collection"].(string)
		resolver, _ := args["resolver"].(string)
		label, _ := args["label"].(string)
		includeOwned, _ := args["include_owned"].(bool)
		output, err := batch.Submit(ctx, deps.caller, system.Config.DataDir, requests, batch.SubmitOptions{
			AutoImport: &autoImport, Collection: collection, Resolver: resolver, Label: label, IncludeOwned: includeOwned, Now: deps.now(),
		})
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		return jsonToolResult(output)
	}
}

func handleBatchWait(deps toolDependencies) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		args := req.GetArguments()
		batchID, _ := args["batch_id"].(string)
		batchID = strings.TrimSpace(batchID)
		if batchID == "" {
			return mcplib.NewToolResultError("batch_id is required"), nil
		}
		output, err := waitForBatch(ctx, deps, batchID, intArg(args, "timeout_seconds"), intArg(args, "poll_seconds"))
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		return jsonToolResult(output)
	}
}

func waitForBatch(ctx context.Context, deps toolDependencies, batchID string, timeoutSeconds, pollSeconds int) (BatchWaitOutput, error) {
	if timeoutSeconds == 0 {
		timeoutSeconds = defaultBatchWaitTimeoutSeconds
	}
	if timeoutSeconds < 0 || timeoutSeconds > maxBatchWaitTimeoutSeconds {
		return BatchWaitOutput{}, fmt.Errorf("timeout_seconds must be between 1 and %d", maxBatchWaitTimeoutSeconds)
	}
	if pollSeconds == 0 {
		pollSeconds = defaultBatchWaitPollSeconds
	}
	if pollSeconds < 0 {
		return BatchWaitOutput{}, fmt.Errorf("poll_seconds must be positive")
	}
	if pollSeconds > timeoutSeconds {
		pollSeconds = timeoutSeconds
	}

	deadline := deps.now().Add(time.Duration(timeoutSeconds) * time.Second)
	var output BatchWaitOutput
	for {
		var report batch.Report
		if err := deps.caller(ctx, "acquire.report", api.AcquireReportParams{BatchID: batchID}, &report); err != nil {
			return BatchWaitOutput{}, err
		}
		output.Report = &report
		output.Settled = report.Settled()
		if output.Settled {
			return output, nil
		}

		remaining := deadline.Sub(deps.now())
		if remaining <= 0 {
			return output, nil
		}
		delay := time.Duration(pollSeconds) * time.Second
		if delay > remaining {
			delay = remaining
		}
		if err := deps.wait(ctx, delay); err != nil {
			return BatchWaitOutput{}, err
		}
	}
}

func waitForPoll(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func intArg(args map[string]any, key string) int {
	switch value := args[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

func jsonToolResult(value any) (*mcplib.CallToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	return mcplib.NewToolResultText(string(data)), nil
}

// --- Resources ---

type resourceLoader func(context.Context, *bootstrap.System) (any, error)

func registerResources(s *server.MCPServer, system *bootstrap.System) {
	addJSONResource(s, system, "papio://jobs", "jobs", "Recent durable acquisition jobs", jobsResource)
	addJSONResource(s, system, "papio://artifacts", "artifacts", "Recent validated content-addressed PDF artifacts", artifactsResource)
	addJSONResource(s, system, "papio://bundles", "bundles", "Recent acquisition bundle export records", func(ctx context.Context, system *bootstrap.System) (any, error) {
		return exportsResource(ctx, system, "bundle")
	})
	addJSONResource(s, system, "papio://zotio/plans", "zotio-plans", "Recent immutable Zotio preview records", func(ctx context.Context, system *bootstrap.System) (any, error) {
		return exportsResource(ctx, system, "zotio_plan")
	})
	addJSONResource(s, system, "papio://exports", "exports", "Recent bundle, plan, and Zotio apply ledger records", func(ctx context.Context, system *bootstrap.System) (any, error) {
		return exportsResource(ctx, system, "")
	})
}

func addJSONResource(s *server.MCPServer, system *bootstrap.System, uri, name, description string, loader resourceLoader) {
	s.AddResource(
		mcplib.NewResource(uri, name, mcplib.WithResourceDescription(description), mcplib.WithMIMEType("application/json")),
		func(ctx context.Context, _ mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
			value, err := loader(ctx, system)
			if err != nil {
				return nil, err
			}
			data, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				return nil, err
			}
			return []mcplib.ResourceContents{mcplib.TextResourceContents{URI: uri, MIMEType: "application/json", Text: string(data)}}, nil
		},
	)
}

func jobsResource(ctx context.Context, system *bootstrap.System) (any, error) {
	return system.Jobs.List(ctx, "", 100)
}

func artifactsResource(ctx context.Context, system *bootstrap.System) (any, error) {
	rows, err := system.Store.DB().QueryContext(ctx, `SELECT sha256 FROM artifacts ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hashes []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			_ = rows.Close()
			return nil, err
		}
		hashes = append(hashes, sha)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	artifacts := make([]job.Artifact, 0, len(hashes))
	for _, sha := range hashes {
		artifact, err := system.Jobs.GetArtifact(ctx, sha)
		if err != nil {
			return nil, err
		}
		if artifact != nil {
			artifacts = append(artifacts, *artifact)
		}
	}
	return artifacts, nil
}

type exportRecord struct {
	ID             int64           `json:"id"`
	JobID          string          `json:"job_id"`
	Kind           string          `json:"kind"`
	IdempotencyKey string          `json:"idempotency_key"`
	Path           string          `json:"path,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	CreatedAt      string          `json:"created_at"`
}

func exportsResource(ctx context.Context, system *bootstrap.System, kind string) (any, error) {
	query := `SELECT id, job_id, kind, COALESCE(idempotency_key,''), COALESCE(path,''), COALESCE(result_json,''), created_at FROM exports`
	args := []any{}
	if kind != "" {
		query += ` WHERE kind = ?`
		args = append(args, kind)
	}
	query += ` ORDER BY id DESC LIMIT 100`
	rows, err := system.Store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []exportRecord
	for rows.Next() {
		var record exportRecord
		var raw sql.NullString
		if err := rows.Scan(&record.ID, &record.JobID, &record.Kind, &record.IdempotencyKey, &record.Path, &raw, &record.CreatedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if raw.Valid && json.Valid([]byte(raw.String)) {
			record.Result = json.RawMessage(raw.String)
		}
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return records, rows.Err()
}
