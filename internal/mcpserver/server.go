// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package mcpserver exposes Papio through the official Model Context Protocol SDK.
package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"papio/internal/api"
	"papio/internal/batch"
	"papio/internal/bootstrap"
	"papio/internal/discovery"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/zotio"
)

const Version = "0.1.0"

type AcquireInput struct {
	RequestID          string                `json:"request_id,omitempty" jsonschema:"stable idempotency key; omit to generate one"`
	Identifiers        *protocol.Identifiers `json:"identifiers,omitempty" jsonschema:"DOI, PMID, arXiv, ISBN, or OpenAlex identity"`
	Title              string                `json:"title,omitempty" jsonschema:"work title; required with authors and year when no identifier is supplied"`
	Authors            []string              `json:"authors,omitempty" jsonschema:"authors; required with title and year when no identifier is supplied"`
	Year               int                   `json:"year,omitempty" jsonschema:"publication year; required with title and authors when no identifier is supplied"`
	ZotioItemKey       string                `json:"zotio_item_key,omitempty" jsonschema:"existing Zotero parent key when filling a known missing attachment"`
	Collection         string                `json:"collection,omitempty" jsonschema:"optional Zotero collection key"`
	DesiredVersion     string                `json:"desired_version,omitempty" jsonschema:"published, accepted, preprint, or any"`
	AccessModeOverride string                `json:"access_mode_override,omitempty" jsonschema:"conservative, assisted, or maximal"`
	MaxCostUSD         *float64              `json:"max_cost_usd,omitempty" jsonschema:"maximum permitted acquisition cost"`
	SourcesAllow       []string              `json:"sources_allow,omitempty" jsonschema:"optional source allowlist, at most 50"`
	SourcesDeny        []string              `json:"sources_deny,omitempty" jsonschema:"optional source denylist, at most 50"`
	AutoImport         *bool                 `json:"auto_import,omitempty" jsonschema:"automatically import a ready job into Zotero; omit to use the configured default"`
}

type AcquireOutput struct {
	JobID string `json:"job_id"`
}

type ExportInput struct {
	JobID     string `json:"job_id" jsonschema:"ready Papio job ID"`
	OutputDir string `json:"output_dir,omitempty" jsonschema:"optional private destination; Papio data directory is the default"`
}

type ExportOutput struct {
	Path   string                      `json:"path"`
	Bundle *protocol.AcquisitionBundle `json:"bundle"`
}

type PlanInput struct {
	JobIDs []string `json:"job_ids" jsonschema:"one to fifty ready Papio job IDs"`
}

type PlanOutput struct {
	Plans []*zotio.Plan `json:"plans"`
}

type ApplyInput struct {
	PlanID             string `json:"plan_id" jsonschema:"opaque plan ID returned by papio_zotio_plan"`
	ConfirmationSHA256 string `json:"confirmation_sha256" jsonschema:"exact confirmation SHA-256 returned by papio_zotio_plan"`
}

// SearchInput selects a bounded, read-only OpenAlex work search.
type SearchInput struct {
	Query     string `json:"query,omitempty" jsonschema:"optional scholarly search query; required unless a citation snowball DOI is supplied"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum results, clamped to 1 through 50"`
	YearFrom  int    `json:"year_from,omitempty" jsonschema:"minimum publication year"`
	YearTo    int    `json:"year_to,omitempty" jsonschema:"maximum publication year"`
	OAOnly    bool   `json:"oa_only,omitempty" jsonschema:"return only open-access works"`
	Cites     string `json:"cites,omitempty" jsonschema:"DOI to find papers citing it (forward citations; OpenAlex cites: filter)"`
	CitedBy   string `json:"cited_by,omitempty" jsonschema:"DOI to find papers it cites (backward references; OpenAlex cited_by: filter)"`
	RelatedTo string `json:"related_to,omitempty" jsonschema:"DOI to find OpenAlex-related papers (related_to: filter)"`
	NewOnly   bool   `json:"new_only,omitempty" jsonschema:"omit works already owned in the local Zotio library; filtering happens after the OpenAlex limit, so fewer results may be returned"`
}

// BatchReportInput selects a persisted batch and one agent-facing format.
type BatchReportInput struct {
	BatchID string `json:"batch_id" jsonschema:"batch ID returned by papio acquire --batch, or latest"`
	Format  string `json:"format,omitempty" jsonschema:"json or markdown; defaults to json"`
}

// New builds one MCP server. Zotero writes are deliberately unreachable from
// every tool except papio_zotio_apply, which requires an immutable plan ID and
// its exact confirmation digest.
func New(system *bootstrap.System) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "papio", Title: "Papio paper acquisition", Version: Version,
	}, &mcp.ServerOptions{
		Instructions: "Acquire papers into durable Papio jobs. Export ready acquisition bundles. Always call papio_zotio_plan and inspect its preview before papio_zotio_apply; apply accepts only the returned plan ID and exact confirmation SHA-256.",
		Capabilities: &mcp.ServerCapabilities{},
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_acquire", Title: "Acquire a scholarly work",
		Description: "Queue one bounded, policy-controlled paper acquisition job. This does not write to Zotero.",
		Annotations: additiveAnnotations(false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AcquireInput) (*mcp.CallToolResult, AcquireOutput, error) {
		requestID := input.RequestID
		if requestID == "" {
			requestID = job.NewID("request")
		}
		jobID, err := system.App.SubmitWithAutoImport(ctx, protocol.WorkRequest{
			SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: requestID,
			Identifiers: input.Identifiers, Title: input.Title, Authors: input.Authors, Year: input.Year,
			ZotioItemKey: input.ZotioItemKey, Collection: input.Collection, DesiredVersion: input.DesiredVersion,
			AccessModeOverride: input.AccessModeOverride, MaxCostUSD: input.MaxCostUSD,
			SourcesAllow: input.SourcesAllow, SourcesDeny: input.SourcesDeny,
		}, input.AutoImport)
		return nil, AcquireOutput{JobID: jobID}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_search", Title: "Search scholarly works",
		Description: "Run bounded, read-only OpenAlex searches, including citation snowballs. Results include owned and owned_item_key from the local Zotio library when available; new_only filters owned results after the OpenAlex limit, so it may return fewer results. This never creates an acquisition job.",
		Annotations: readAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, any, error) {
		if system == nil || system.Discovery == nil {
			return nil, nil, fmt.Errorf("discovery is not configured")
		}
		works, err := system.Discovery.Search(ctx, discovery.SearchParams{
			Query: input.Query, Limit: input.Limit, YearFrom: input.YearFrom, YearTo: input.YearTo, OAOnly: input.OAOnly,
			Cites: input.Cites, CitedBy: input.CitedBy, RelatedTo: input.RelatedTo,
		})
		if err != nil {
			return nil, nil, err
		}
		var lookup discovery.OwnershipLookup
		if system.Zotio != nil {
			lookup = system.Zotio
		}
		if warning := discovery.ClassifyOwnership(ctx, works, lookup); warning != "" {
			log.Printf("warning: %s", warning)
		}
		if input.NewOnly {
			works = newWorksOnly(works)
		}
		data, err := json.Marshal(works)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_batch_report", Title: "Report batch acquisition outcomes",
		Description: "Join a persisted batch manifest with live job, event, and human-action state. Returns an agent-ready JSON or Markdown digest.",
		Annotations: readAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input BatchReportInput) (*mcp.CallToolResult, any, error) {
		if input.BatchID == "" {
			return nil, nil, fmt.Errorf("batch_id is required")
		}
		format := input.Format
		if format == "" {
			format = "json"
		}
		report, err := api.BatchReport(ctx, system, input.BatchID)
		if err != nil {
			return nil, nil, err
		}
		switch format {
		case "json":
			data, err := json.Marshal(report)
			if err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
		case "markdown":
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: batch.Markdown(report)}}}, nil, nil
		default:
			return nil, nil, fmt.Errorf("format must be json or markdown")
		}
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_export_bundle", Title: "Export an acquisition bundle",
		Description: "Export or idempotently reuse the validated PDF and provenance bundle for one ready job.",
		Annotations: readAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input ExportInput) (*mcp.CallToolResult, ExportOutput, error) {
		if input.JobID == "" {
			return nil, ExportOutput{}, fmt.Errorf("job_id is required")
		}
		path, bundle, err := system.Bundle.Export(ctx, input.JobID, input.OutputDir)
		return nil, ExportOutput{Path: path, Bundle: bundle}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_zotio_plan", Title: "Preview Zotero mutations",
		Description: "Export ready jobs, route each through Zotio, and persist immutable previews. This never applies a Zotero mutation.",
		Annotations: additiveAnnotations(true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input PlanInput) (*mcp.CallToolResult, PlanOutput, error) {
		plans, err := system.Zotio.PlanJobs(ctx, input.JobIDs)
		return nil, PlanOutput{Plans: plans}, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_zotio_apply", Title: "Apply a confirmed Zotero plan",
		Description: "Apply exactly one immutable Zotio preview. Requires the plan ID and exact confirmation SHA-256 returned by papio_zotio_plan; replay is idempotent.",
		Annotations: additiveAnnotations(true),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input ApplyInput) (*mcp.CallToolResult, zotio.ApplyResult, error) {
		if input.PlanID == "" || input.ConfirmationSHA256 == "" {
			return nil, zotio.ApplyResult{}, fmt.Errorf("plan_id and confirmation_sha256 are required")
		}
		result, err := system.Zotio.Apply(ctx, input.PlanID, input.ConfirmationSHA256)
		if result == nil {
			return nil, zotio.ApplyResult{}, err
		}
		return nil, *result, err
	})

	addJSONResource(server, system, "papio://jobs", "jobs", "Recent durable acquisition jobs", jobsResource)
	addJSONResource(server, system, "papio://artifacts", "artifacts", "Recent validated content-addressed PDF artifacts", artifactsResource)
	addJSONResource(server, system, "papio://bundles", "bundles", "Recent acquisition bundle export records", func(ctx context.Context, system *bootstrap.System) (any, error) {
		return exportsResource(ctx, system, "bundle")
	})
	addJSONResource(server, system, "papio://zotio/plans", "zotio-plans", "Recent immutable Zotio preview records", func(ctx context.Context, system *bootstrap.System) (any, error) {
		return exportsResource(ctx, system, "zotio_plan")
	})
	addJSONResource(server, system, "papio://exports", "exports", "Recent bundle, plan, and Zotio apply ledger records", func(ctx context.Context, system *bootstrap.System) (any, error) {
		return exportsResource(ctx, system, "")
	})
	return server
}

// Run serves MCP over stdin/stdout until the client disconnects.
func Run(ctx context.Context, system *bootstrap.System) error {
	return New(system).Run(ctx, &mcp.StdioTransport{})
}

type resourceLoader func(context.Context, *bootstrap.System) (any, error)

func addJSONResource(server *mcp.Server, system *bootstrap.System, uri, name, description string, loader resourceLoader) {
	server.AddResource(&mcp.Resource{
		URI: uri, Name: name, Title: name, Description: description, MIMEType: "application/json",
	}, func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		value, err := loader(ctx, system)
		if err != nil {
			return nil, err
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(data)}}}, nil
	})
}

func jobsResource(ctx context.Context, system *bootstrap.System) (any, error) {
	return system.Jobs.List(ctx, "", 100)
}

func artifactsResource(ctx context.Context, system *bootstrap.System) (any, error) {
	rows, err := system.Store.DB().QueryContext(ctx, `SELECT sha256 FROM artifacts ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
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

func newWorksOnly(works []discovery.DiscoveredWork) []discovery.DiscoveredWork {
	filtered := make([]discovery.DiscoveredWork, 0, len(works))
	for _, discovered := range works {
		if !discovered.Owned {
			filtered = append(filtered, discovered)
		}
	}
	return filtered
}

func readAnnotations() *mcp.ToolAnnotations {
	closed := false
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closed}
}

func additiveAnnotations(openWorld bool) *mcp.ToolAnnotations {
	destructive := false
	return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &destructive, IdempotentHint: true, OpenWorldHint: &openWorld}
}
