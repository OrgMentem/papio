// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package mcpserver exposes Papio through the official Model Context Protocol SDK.
package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"papio/internal/api"
	"papio/internal/batch"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/doctor"
	"papio/internal/errcat"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/zotio"
)

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
	Resolver           string                `json:"resolver,omitempty" jsonschema:"named institutional OpenURL resolver profile"`
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

// AcquireBatchInput is the MCP equivalent of acquire --batch. Each work may be
// a bare work object or a discovered-work envelope containing a work field.
type AcquireBatchInput struct {
	Works        []map[string]any `json:"works" jsonschema:"one to fifty bare work objects or discovered-work envelopes"`
	AutoImport   *bool            `json:"auto_import,omitempty" jsonschema:"automatically import ready jobs; defaults to true"`
	Collection   string           `json:"collection,omitempty" jsonschema:"target Zotero collection for every submitted work; defaults to the label (search query) so imported papers are filed under the search that produced them"`
	Resolver     string           `json:"resolver,omitempty" jsonschema:"named institutional OpenURL resolver profile for every submitted work"`
	Label        string           `json:"label,omitempty" jsonschema:"batch query context; also the default target collection when collection is unset"`
	IncludeOwned bool             `json:"include_owned,omitempty" jsonschema:"submit works already carrying a PDF in Zotio; defaults to false"`
}

// AcquireBatchOutput records submitted jobs and works routed without a new
// acquisition job by Papio's standard ownership policy.
type AcquireBatchOutput = batch.SubmitOutput

// StatusSnapshot is the current active and recently completed job view.
type StatusSnapshot struct {
	GeneratedAt string        `json:"generated_at"`
	Groups      []StatusGroup `json:"groups"`
}

// StatusGroup groups jobs by their user-visible acquisition phase.
type StatusGroup struct {
	Phase string      `json:"phase"`
	Jobs  []StatusJob `json:"jobs"`
}

// StatusJob is one concise job record in StatusSnapshot.
type StatusJob struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Provider     string `json:"provider"`
	State        string `json:"state"`
	Age          string `json:"age"`
	Reason       string `json:"reason,omitempty"`
	Category     string `json:"category,omitempty"`
	Guidance     string `json:"guidance,omitempty"`
	ImportStatus string `json:"import_status,omitempty"`
}

// ActionsListOutput exposes open human actions that need local inspection.
type ActionsListOutput struct {
	Actions []job.HumanAction `json:"actions"`
}

// ActionsResolveInput resolves one verify_identity action after inspecting its
// local quarantine artifact.
type ActionsResolveInput struct {
	ActionID int64  `json:"action_id" jsonschema:"open verify_identity action ID"`
	Verdict  string `json:"verdict" jsonschema:"accept or reject; accept asserts that a human or agent verified the quarantined artifact is the requested work"`
}

// ActionsResolveOutput is the new job state after resolving one human action.
type ActionsResolveOutput struct {
	JobID string `json:"job_id"`
	State string `json:"state"`
}

// BatchWaitInput bounds one wait for a persisted acquisition batch.
type BatchWaitInput struct {
	BatchID        string `json:"batch_id" jsonschema:"batch ID returned by papio acquire --batch, or latest"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"maximum wait in seconds, 1 through 600; 0 or omitted defaults to 300"`
	PollSeconds    int    `json:"poll_seconds,omitempty" jsonschema:"seconds between reports; defaults to 5"`
}

// BatchWaitOutput returns the final daemon report and whether it fully settled.
type BatchWaitOutput struct {
	Report  *batch.Report `json:"report"`
	Settled bool          `json:"settled"`
}

// WatchFiltersInput narrows discovery results for a scheduled watch.
type WatchFiltersInput struct {
	YearFrom int  `json:"year_from,omitempty"`
	YearTo   int  `json:"year_to,omitempty"`
	OAOnly   bool `json:"oa_only,omitempty"`
}

// WatchAddInput creates one scheduled discovery watch.
type WatchAddInput struct {
	Label        string             `json:"label,omitempty" jsonschema:"optional display label; defaults to query"`
	Query        string             `json:"query" jsonschema:"scholarly discovery query"`
	Filters      *WatchFiltersInput `json:"filters,omitempty" jsonschema:"optional discovery filters"`
	Collection   string             `json:"collection,omitempty" jsonschema:"optional Zotero collection key for discovered work"`
	CadenceHours int                `json:"cadence_hours" jsonschema:"positive interval between runs in hours"`
	PerRunCap    int                `json:"per_run_cap,omitempty" jsonschema:"maximum newly discovered works per run; defaults to 10"`
}

// WatchOutput is the durable state of one scheduled discovery watch.
type WatchOutput struct {
	ID                  int64             `json:"id"`
	Label               string            `json:"label"`
	Query               string            `json:"query"`
	Filters             WatchFiltersInput `json:"filters"`
	Collection          string            `json:"collection,omitempty"`
	CadenceHours        int               `json:"cadence_hours"`
	PerRunCap           int               `json:"per_run_cap"`
	Enabled             bool              `json:"enabled"`
	LastRunAt           string            `json:"last_run_at,omitempty"`
	CreatedAt           string            `json:"created_at"`
	ConsecutiveFailures int               `json:"consecutive_failures"`
	LastError           string            `json:"last_error,omitempty"`
}

// WatchListOutput groups the watch rows under one structured MCP output.
type WatchListOutput struct {
	Watches []WatchOutput `json:"watches"`
}

// WatchRemoveInput identifies one scheduled watch to remove.
type WatchRemoveInput struct {
	ID int64 `json:"id" jsonschema:"scheduled watch ID"`
}

// WatchRemoveOutput confirms removal of one scheduled watch.
type WatchRemoveOutput struct {
	ID      int64 `json:"id"`
	Removed bool  `json:"removed"`
}

type rpcCaller interface {
	Call(context.Context, string, any, any) error
}

// localRPCCaller preserves the local RPC contracts for MCP handlers while the
// MCP process owns the same configured services as the CLI.
type localRPCCaller struct {
	router ipc.Router
}

func newLocalRPCCaller(system *bootstrap.System) rpcCaller {
	if system == nil {
		return localRPCCaller{}
	}
	return localRPCCaller{router: api.Router(system)}
}

func (c localRPCCaller) Call(ctx context.Context, method string, params, result any) error {
	if c.router.Methods == nil {
		return fmt.Errorf("Papio RPC is not configured")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	response, rpcErr := c.router.Handle(ctx, ipc.Request{Method: method, Params: raw})
	if rpcErr != nil {
		return rpcErr
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response, result)
}

type toolDependencies struct {
	caller    rpcCaller
	cfg       config.Config
	now       func() time.Time
	wait      func(context.Context, time.Duration) error
	doctorRun func(context.Context) doctor.Report
}

func defaultToolDependencies(caller rpcCaller) toolDependencies {
	return toolDependencies{caller: caller, now: time.Now, wait: waitForPoll}
}

// New builds one MCP server. Zotero writes are deliberately unreachable from
// every tool except papio_zotio_apply, which requires an immutable plan ID and
// its exact confirmation digest.
func New(system *bootstrap.System) *mcp.Server {
	deps := defaultToolDependencies(newLocalRPCCaller(system))
	if system != nil {
		deps.cfg = system.Config
		deps.doctorRun = doctorReportForSystem(system)
	}
	return newServer(system, deps)
}

func doctorReportForSystem(system *bootstrap.System) func(context.Context) doctor.Report {
	return func(ctx context.Context) doctor.Report {
		readiness := doctor.Run(ctx, system.Config, system.Store, system.PDFCapability, system.WorkerBinary)
		integration := doctor.RunIntegration(ctx, doctor.IntegrationDependencies{
			CLIVersion: api.Version,
			LoadConfig: func() (config.Config, error) {
				return system.Config, nil
			},
			DaemonStatus: func(context.Context, config.Config) (doctor.DaemonStatus, error) {
				status := doctor.DaemonStatus{Status: "ok", Version: api.Version}
				if system.Browser != nil {
					status.ExtensionVersion, _, status.ExtensionConnected = system.Browser.SessionInfo()
				}
				return status, nil
			},
			ManifestDir: func(config.Config) (string, error) {
				return doctor.DefaultChromeNativeMessagingHostsDir()
			},
			FirefoxDir: func(config.Config) (string, error) {
				return doctor.DefaultFirefoxNativeMessagingHostsDir()
			},
			ReadFile: os.ReadFile,
			ZotioPreflight: func(ctx context.Context, cfg config.Config) (*zotio.PreflightResult, error) {
				return zotio.New(cfg.Zotio).Preflight(ctx)
			},
		})
		return doctor.Report{
			OK:     readiness.OK && integration.OK,
			Checks: append(readiness.Checks, integration.Checks...),
		}
	}
}

func newServer(system *bootstrap.System, dependencies toolDependencies) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "papio", Title: "Papio paper acquisition", Version: api.Version,
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
			Resolver:     input.Resolver,
			SourcesAllow: input.SourcesAllow, SourcesDeny: input.SourcesDeny,
		}, input.AutoImport)
		return nil, AcquireOutput{JobID: jobID}, err
	})
	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_acquire_batch", Title: "Acquire a batch of scholarly works",
		Description: "Create a durable acquisition batch using the same ownership routing, deterministic request IDs, auto-import policy, and manifest as papio acquire --batch.",
		Annotations: additiveAnnotations(false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input AcquireBatchInput) (*mcp.CallToolResult, AcquireBatchOutput, error) {
		if system == nil || strings.TrimSpace(system.Config.DataDir) == "" {
			return nil, AcquireBatchOutput{}, fmt.Errorf("batch acquisition is not configured")
		}
		if len(input.Works) == 0 {
			return nil, AcquireBatchOutput{}, fmt.Errorf("batch contains no works")
		}
		if len(input.Works) > 50 {
			return nil, AcquireBatchOutput{}, fmt.Errorf("batch exceeds maximum of 50 works")
		}
		requests := make([]protocol.WorkRequest, len(input.Works))
		for i, inputWork := range input.Works {
			raw, err := json.Marshal(inputWork)
			if err != nil {
				return nil, AcquireBatchOutput{}, fmt.Errorf("encoding batch work %d: %w", i+1, err)
			}
			request, err := batch.ParseWork(raw)
			if err != nil {
				return nil, AcquireBatchOutput{}, fmt.Errorf("batch work %d: %w", i+1, err)
			}
			requests[i] = request
		}
		autoImport := true
		if input.AutoImport != nil {
			autoImport = *input.AutoImport
		}
		output, err := batch.Submit(ctx, dependencies.caller, system.Config.DataDir, requests, batch.SubmitOptions{
			AutoImport: &autoImport, Collection: input.Collection, Resolver: input.Resolver, Label: input.Label, IncludeOwned: input.IncludeOwned, Now: dependencies.now(),
		})
		if output == nil {
			return nil, AcquireBatchOutput{}, err
		}
		return nil, *output, err
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

	addLoopClosureTools(server, dependencies)

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

type StatusInput struct{}

type DoctorInput struct{}

type ActionsListInput struct{}

func addLoopClosureTools(server *mcp.Server, dependencies toolDependencies) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_doctor", Title: "Diagnose Papio integrations",
		Description: "Run papio's integration diagnostics: config, daemon, browser extension connectivity, native-messaging host manifests, and Zotio preflight. Returns pass/warn/fail/skip checks with fix guidance. Read-only.",
		Annotations: readAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ DoctorInput) (*mcp.CallToolResult, doctor.Report, error) {
		if dependencies.doctorRun == nil {
			return nil, doctor.Report{
				OK: false,
				Checks: []doctor.Check{{
					Name: "doctor", Status: doctor.Fail, Detail: "in-process diagnostics are not configured",
					Remediation: "run papio mcp through the Papio daemon",
				}},
			}, nil
		}
		return nil, dependencies.doctorRun(ctx), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_status", Title: "Summarize Papio jobs",
		Description: "Return active and recently completed acquisition jobs grouped into the same working, human-review, ready, and failed phases as papio status. Parked or no-file jobs carry an actionable category and a one-line next step (e.g. institution_not_configured -> run papio init). Read-only.",
		Annotations: readAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, StatusSnapshot, error) {
		snapshot, err := loadStatusSnapshot(ctx, dependencies.caller, dependencies.cfg, dependencies.now())
		return nil, snapshot, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_actions_list", Title: "List open human actions",
		Description: "Return open human actions with their action ID, job ID, kind, and detail. verify_identity detail intentionally includes the local quarantine path so the calling agent can inspect that file.",
		Annotations: readAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ ActionsListInput) (*mcp.CallToolResult, ActionsListOutput, error) {
		var actions []job.HumanAction
		if err := dependencies.call(ctx, "actions.list", map[string]bool{"open_only": true}, &actions); err != nil {
			return nil, ActionsListOutput{}, err
		}
		return nil, ActionsListOutput{Actions: actions}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_actions_resolve", Title: "Resolve a verified identity action",
		Description: "Resolve only an open verify_identity action. accept asserts that a human or calling agent inspected the local quarantine artifact and verified it is the requested work; reject records that it is not.",
		Annotations: additiveAnnotations(false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input ActionsResolveInput) (*mcp.CallToolResult, ActionsResolveOutput, error) {
		var output ActionsResolveOutput
		if err := dependencies.call(ctx, "actions.resolve", map[string]any{"action_id": input.ActionID, "verdict": input.Verdict}, &output); err != nil {
			return nil, ActionsResolveOutput{}, err
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_batch_wait", Title: "Wait for batch outcomes",
		Description: "Poll acquire.report for one batch until every work has a settled outcome, including an explicit human-review outcome, or the bounded timeout expires. Does not submit, import, or resolve work.",
		Annotations: readAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input BatchWaitInput) (*mcp.CallToolResult, BatchWaitOutput, error) {
		output, err := waitForBatch(ctx, dependencies, input)
		return nil, output, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_watch_add", Title: "Add a scheduled discovery watch",
		Description: "Create a bounded scheduled discovery watch. Each run searches for matching work and applies Papio's existing batch, ownership, auto-import, collection, and notification policy.",
		Annotations: additiveAnnotations(false),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input WatchAddInput) (*mcp.CallToolResult, WatchOutput, error) {
		var output WatchOutput
		if err := dependencies.call(ctx, "watch.add", input, &output); err != nil {
			return nil, WatchOutput{}, err
		}
		return nil, output, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_watch_list", Title: "List scheduled discovery watches",
		Description: "List scheduled discovery watches and their last run state. Read-only.",
		Annotations: readAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, WatchListOutput, error) {
		var watches []WatchOutput
		if err := dependencies.call(ctx, "watch.list", map[string]any{}, &watches); err != nil {
			return nil, WatchListOutput{}, err
		}
		return nil, WatchListOutput{Watches: watches}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "papio_watch_remove", Title: "Remove a scheduled discovery watch",
		Description: "Permanently remove one scheduled discovery watch. This does not delete any jobs or Zotero items created by prior watch runs.",
		Annotations: destructiveAnnotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input WatchRemoveInput) (*mcp.CallToolResult, WatchRemoveOutput, error) {
		var output WatchRemoveOutput
		if err := dependencies.call(ctx, "watch.remove", input, &output); err != nil {
			return nil, WatchRemoveOutput{}, err
		}
		return nil, output, nil
	})
}

func (d toolDependencies) call(ctx context.Context, method string, params, result any) error {
	if d.caller == nil {
		return fmt.Errorf("Papio RPC is not configured")
	}
	return d.caller.Call(ctx, method, params, result)
}

const (
	statusRecentWindow             = 24 * time.Hour
	defaultBatchWaitTimeoutSeconds = 300
	maxBatchWaitTimeoutSeconds     = 600
	defaultBatchWaitPollSeconds    = 5
)

func loadStatusSnapshot(ctx context.Context, caller rpcCaller, cfg config.Config, now time.Time) (StatusSnapshot, error) {
	if caller == nil {
		return StatusSnapshot{}, fmt.Errorf("Papio RPC is not configured")
	}
	var rows []job.Row
	if err := caller.Call(ctx, "jobs.list", map[string]int{"limit": 500}, &rows); err != nil {
		return StatusSnapshot{}, err
	}

	details := make(map[string]api.JobDetail, len(rows))
	for _, row := range rows {
		if !showStatusRow(row, now) {
			continue
		}
		var detail api.JobDetail
		if err := caller.Call(ctx, "jobs.get", map[string]string{"job_id": row.ID}, &detail); err != nil {
			return StatusSnapshot{}, err
		}
		details[row.ID] = detail
	}
	return buildStatusSnapshot(rows, details, cfg, now), nil
}

func buildStatusSnapshot(rows []job.Row, details map[string]api.JobDetail, cfg config.Config, now time.Time) StatusSnapshot {
	groups := map[string][]StatusJob{
		"working":            nil,
		"awaiting_human":     nil,
		"needs_review":       nil,
		"ready":              nil,
		"failed_unavailable": nil,
	}
	for _, row := range rows {
		if !showStatusRow(row, now) {
			continue
		}

		group := statusPhase(row.State)
		if group == "" {
			continue
		}
		detail := details[row.ID]
		item := StatusJob{
			ID:       row.ID,
			Title:    shortTitle(row.Work.Describe()),
			Provider: eventProvider(detail.Events),
			State:    row.State,
			Age:      formatStatusAge(row.UpdatedAt, now),
		}
		if item.Title == "" {
			item.Title = "(untitled)"
		}
		if group == "awaiting_human" || group == "needs_review" || group == "failed_unavailable" {
			item.Reason = transitionReason(detail.Events, row.State)
			exp := errcat.Explain(row.State, item.Reason, row.Policy.Resolver, row.Policy.AccessMode, cfg)
			item.Category = exp.Category
			item.Guidance = exp.Guidance
		}
		if group == "ready" {
			item.ImportStatus = autoImportStatus(detail.Events)
		}
		groups[group] = append(groups[group], item)
	}

	ordered := []struct {
		phase string
		label string
	}{
		{"working", "working"},
		{"awaiting_human", "awaiting_human"},
		{"needs_review", "needs_review"},
		{"ready", "ready (last 24h)"},
		{"failed_unavailable", "failed / unavailable"},
	}
	snapshot := StatusSnapshot{GeneratedAt: now.UTC().Format(time.RFC3339Nano)}
	for _, group := range ordered {
		if len(groups[group.phase]) != 0 {
			snapshot.Groups = append(snapshot.Groups, StatusGroup{Phase: group.label, Jobs: groups[group.phase]})
		}
	}
	return snapshot
}

func showStatusRow(row job.Row, now time.Time) bool {
	if !job.Terminal(row.State) {
		return true
	}
	updated, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
	return err == nil && !updated.Before(now.Add(-statusRecentWindow))
}

func statusPhase(state string) string {
	switch state {
	case job.StateQueued, job.StateResolving, job.StateFetching, job.StateValidating, job.StateRetryWait:
		return "working"
	case job.StateAwaitingHuman:
		return "awaiting_human"
	case job.StateNeedsReview:
		return "needs_review"
	case job.StateReady:
		return "ready"
	case job.StateFailed, job.StateUnavailable, job.StateCancelled:
		return "failed_unavailable"
	default:
		return ""
	}
}

func eventProvider(events []map[string]any) string {
	for i := len(events) - 1; i >= 0; i-- {
		if value := eventDetailString(events[i], "source"); value != "" {
			return value
		}
	}
	return "—"
}

func transitionReason(events []map[string]any, state string) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "job.transition" || eventDetailString(events[i], "to") != state {
			continue
		}
		if reason := eventDetailString(events[i], "reason"); reason != "" {
			return shortText(reason, 72)
		}
	}
	return "—"
}

func autoImportStatus(events []map[string]any) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] == "zotio.auto_import" {
			if status := eventDetailString(events[i], "status"); status != "" {
				return status
			}
		}
	}
	return "—"
}

func eventDetailString(event map[string]any, key string) string {
	detail, _ := event["detail"].(map[string]any)
	value, _ := detail[key].(string)
	return value
}

func shortTitle(value string) string { return shortText(value, 50) }

func shortText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func formatStatusAge(timestamp string, now time.Time) string {
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "—"
	}
	age := now.Sub(at)
	if age <= 0 || age < time.Minute {
		return "now"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
}

func waitForBatch(ctx context.Context, dependencies toolDependencies, input BatchWaitInput) (BatchWaitOutput, error) {
	batchID := strings.TrimSpace(input.BatchID)
	if batchID == "" {
		return BatchWaitOutput{}, fmt.Errorf("batch_id is required")
	}
	timeoutSeconds := input.TimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = defaultBatchWaitTimeoutSeconds
	}
	if timeoutSeconds < 0 || timeoutSeconds > maxBatchWaitTimeoutSeconds {
		return BatchWaitOutput{}, fmt.Errorf("timeout_seconds must be between 1 and %d", maxBatchWaitTimeoutSeconds)
	}
	pollSeconds := input.PollSeconds
	if pollSeconds == 0 {
		pollSeconds = defaultBatchWaitPollSeconds
	}
	if pollSeconds < 0 {
		return BatchWaitOutput{}, fmt.Errorf("poll_seconds must be positive")
	}
	if pollSeconds > timeoutSeconds {
		pollSeconds = timeoutSeconds
	}

	deadline := dependencies.now().Add(time.Duration(timeoutSeconds) * time.Second)
	var output BatchWaitOutput
	for {
		var report batch.Report
		if err := dependencies.call(ctx, "acquire.report", api.AcquireReportParams{BatchID: batchID}, &report); err != nil {
			return BatchWaitOutput{}, err
		}
		output.Report = &report
		output.Settled = batchReportSettled(&report)
		if output.Settled {
			return output, nil
		}

		remaining := deadline.Sub(dependencies.now())
		if remaining <= 0 {
			return output, nil
		}
		delay := time.Duration(pollSeconds) * time.Second
		if delay > remaining {
			delay = remaining
		}
		if err := dependencies.wait(ctx, delay); err != nil {
			return BatchWaitOutput{}, err
		}
	}
}

func batchReportSettled(report *batch.Report) bool {
	return report != nil && report.Settled()
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

func destructiveAnnotations() *mcp.ToolAnnotations {
	destructive := true
	closed := false
	return &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &destructive, IdempotentHint: true, OpenWorldHint: &closed}
}
