// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package api exposes the acquisition core through strict local IPC methods.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"papio/internal/agentjson"
	"papio/internal/app"
	"papio/internal/batch"
	"papio/internal/bootstrap"
	"papio/internal/browser"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/ownership"
	"papio/internal/protocol"
	"papio/internal/update"
	"papio/internal/watch"
	"papio/internal/zotio"
)

// Version is the papio version, overridable at build time via
// -ldflags "-X papio/internal/api.Version=<v>". Defaults to a dev marker.
var Version = "0.1.0-dev"

type SubmitResult struct {
	JobID string `json:"job_id"`
}

// SubmitV2Result preserves the legacy SubmitResult wire shape while exposing
// whether a live job already owned this acquisition.
type SubmitV2Result struct {
	JobID    string `json:"job_id"`
	Existing bool   `json:"existing"`
}

// ActionsOpenParams names the parked handoffs the CLI wants the extension to
// surface instead of duplicating their resolver tabs through the OS.
type ActionsOpenParams struct {
	JobIDs []string `json:"job_ids"`
}

// ActionsOpenResult distinguishes a usable compatible holder from the normal
// fallback path, while Queued reports how many focus requests reached it.
type ActionsOpenResult struct {
	Queued      int  `json:"queued"`
	SessionLive bool `json:"session_live"`
}

// AcquireReportParams names one persisted batch manifest, or "latest".
type AcquireReportParams struct {
	BatchID string `json:"batch_id"`
}

type JobDetail struct {
	Job     *job.Row          `json:"job"`
	Events  []map[string]any  `json:"events"`
	Actions []job.HumanAction `json:"actions"`
}

type ArtifactResult struct {
	Artifact *job.Artifact `json:"artifact"`
}

type BundleResult struct {
	Path   string                      `json:"path"`
	Bundle *protocol.AcquisitionBundle `json:"bundle"`
}

// JobsPage and ActionsPage decode the jobs.list_v2 / actions.list_v2 envelopes.
// `Truncated` is a proof rather than a hint: the daemon reached one row past the
// limit to answer it, so false means this is the complete list.
type JobsPage struct {
	Jobs      []job.Row `json:"jobs"`
	Truncated bool      `json:"truncated"`
}

type ActionsPage struct {
	Actions   []job.HumanAction `json:"actions"`
	Truncated bool              `json:"truncated"`
}

// RepairResult reports what jobs.repair_awaiting_human did. Outcome is a closed
// vocabulary — repaired, not_parked, has_open_actions, conflict — so a consumer
// never has to parse an error message to tell "nothing to repair" from "I could
// not repair it".
type RepairResult struct {
	JobID    string `json:"job_id"`
	Repaired bool   `json:"repaired"`
	Outcome  string `json:"outcome"`
	State    string `json:"state,omitempty"`
}

// Receipt answers "what happened to this acquisition, and what exactly did it
// obtain" for one job. It exists for the states `acquisition-bundle/1` cannot
// describe — failures, which have no bundle — and deliberately does not restate
// what a bundle already carries. For a successful job it reports the components
// and points at the bundle rather than duplicating its candidate/validation
// blocks, so the two can never disagree (ADR-0007).
//
// Every field is a fact papio observed. There is no completeness verdict, no
// citation match, and no rights permission: those are judgements and belong to
// the consumer.
type Receipt struct {
	JobID     string `json:"job_id"`
	RequestID string `json:"request_id"`
	State     string `json:"state"`
	Terminal  bool   `json:"terminal"`
	// TerminalReason is a member of the closed job.TerminalReason vocabulary;
	// a value written by an older binary normalises to "unknown" rather than
	// leaking free text into a typed field.
	TerminalReason string `json:"terminal_reason,omitempty"`
	// Principal classifies the request origin (cli, mcp, or unknown), not an
	// authenticated identity or proof of whose entitlement obtained the bytes.
	// Consumers must not use it as a rights or permission input.
	Principal string `json:"principal"`
	// AttemptedTiers lists the access bases actually reached, in rank order —
	// candidates that were only ranked and never tried are excluded.
	AttemptedTiers []string `json:"attempted_tiers"`
	// Components is what this job holds, main first. Empty for a failure.
	Components []job.Component `json:"components"`
	// BundleAvailable reports that bundle.export can produce the full provenance
	// record — version, licence, access basis, validation — for this job.
	BundleAvailable bool `json:"bundle_available"`
}

// Router returns the complete Phase 1 local RPC surface.
func Router(system *bootstrap.System) ipc.Router {
	return RouterWithShutdown(system, nil)
}

// RouterWithShutdown adds the process-lifecycle method used by `daemon stop`.
// The delayed callback lets the successful response flush before cancellation.
func RouterWithShutdown(system *bootstrap.System, shutdown context.CancelFunc) ipc.Router {
	var updateRefreshInFlight atomic.Bool
	methods := map[string]ipc.MethodHandler{
		"ping": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return ping(ctx, raw, system, &updateRefreshInFlight)
		},
		"adapter.captures.list": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return listCaptures(ctx, raw, system)
		},
		"adapter.captures.purge": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return purgeCaptures(ctx, raw, system)
		},
		"acquire.submit": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return submit(ctx, raw, system)
		},
		"acquire.submit_v2": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return submitV2(ctx, raw, system)
		},
		"acquire.report": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return acquireReport(ctx, raw, system)
		},
		"discovery.search": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return searchDiscovery(ctx, raw, system)
		},
		"watch.add": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return addWatch(ctx, raw, system)
		},
		"watch.digest": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return watchDigest(ctx, raw, system)
		},
		"watch.digest_acquire": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return acquireWatchDigest(ctx, raw, system)
		},
		"watch.digest_clear": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return clearWatchDigest(ctx, raw, system)
		},
		"watch.list": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return listWatches(ctx, raw, system)
		},
		"watch.remove": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return removeWatch(ctx, raw, system)
		},
		"watch.run": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return runWatch(ctx, raw, system)
		},
		"triage.snapshot": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return triageSnapshot(ctx, raw, system)
		},
		"triage.counts": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return triageCounts(ctx, raw, system)
		},
		"stats.get": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return triageStats(ctx, raw, system)
		},
		"triage.decide": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return triageDecide(ctx, raw, system)
		},
		"jobs.list": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return listJobs(ctx, raw, system)
		},
		"jobs.list_v2": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return listJobsV2(ctx, raw, system)
		},
		"jobs.receipt": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return jobReceipt(ctx, raw, system)
		},
		"jobs.add_component": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return addComponent(ctx, raw, system)
		},
		"jobs.repair_awaiting_human": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return repairAwaitingHuman(ctx, raw, system)
		},
		"jobs.failures": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return listFailures(ctx, raw, system)
		},
		"jobs.get": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return getJob(ctx, raw, system)
		},
		"jobs.cancel": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return cancelJob(ctx, raw, system)
		},
		"jobs.retry": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return retryJob(ctx, raw, system)
		},
		"actions.list": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return listActions(ctx, raw, system)
		},
		"actions.list_v2": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return listActionsV2(ctx, raw, system)
		},
		"actions.open": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return openActions(ctx, raw, system)
		},
		"actions.resolve": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return resolveAction(ctx, raw, system)
		},
		"artifacts.get": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return getArtifact(ctx, raw, system)
		},
		"bundle.export": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return exportBundle(ctx, raw, system)
		},
		"doctor.run": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return runDoctor(ctx, raw, system)
		},
		"zotio.preflight": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioPreflight(ctx, raw, system)
		},
		"zotio.queue": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioQueue(ctx, raw, system)
		},
		"zotio.missing_count": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioMissingCount(ctx, raw, system)
		},
		"zotio.lookup_works": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioLookupWorks(ctx, raw, system)
		},
		"library.lookup_works": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return libraryLookupWorks(ctx, raw, system)
		},
		"zotio.plan": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioPlan(ctx, raw, system)
		},
		"zotio.apply": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioApply(ctx, raw, system)
		},
		"zotio.tags.reconcile": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioTagsReconcile(ctx, raw, system)
		},
		"browser.sync": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return browserSync(ctx, raw, system)
		},
		"browser.sessions": func(_ context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return browserSessions(raw, system)
		},
		"browser.claim": func(_ context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return browserClaim(raw, system)
		},
	}
	if shutdown != nil {
		methods["daemon.shutdown"] = func(_ context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			var params struct{}
			if err := ipc.DecodeParams(raw, &params); err != nil {
				return badParams(err)
			}
			time.AfterFunc(25*time.Millisecond, shutdown)
			return marshal(map[string]bool{"stopping": true})
		}
	}
	return ipc.Router{Methods: methods}
}

// InProcessCaller routes RPC methods through the local router in-process,
// without a socket. It backs the embedded MCP server and the CLI command
// facade that server exposes, so both reach the same handlers the daemon
// serves over IPC.
//
// Calls through here are tagged as the MCP principal: this is the agent-facing
// entry point, and an acquisition's principal records whose entitlement obtained
// the bytes. Socket callers stay on the CLI default (see PrincipalFrom) — papio
// is single-user, so there is no durable per-user identity to source, and an
// honest coarse principal beats a fabricated one (ADR-0007).
func InProcessCaller(system *bootstrap.System) func(context.Context, string, any, any) error {
	router := Router(system)
	return func(ctx context.Context, method string, params, result any) error {
		ctx = WithPrincipal(ctx, job.PrincipalMCP)
		if router.Methods == nil {
			return errors.New("papio RPC is not configured")
		}
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		response, rpcErr := router.Handle(ctx, ipc.Request{Method: method, Params: raw})
		if rpcErr != nil {
			return rpcErr
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(response, result)
	}
}

type statusResult struct {
	Status                 string `json:"status"`
	Version                string `json:"version"`
	ExtensionConnected     bool   `json:"extension_connected"`
	ExtensionVersion       string `json:"extension_version,omitempty"`
	PendingBrowserSessions int    `json:"pending_browser_sessions,omitempty"`
	BrowserSessionDenied   int    `json:"browser_session_denied,omitempty"`
	UpdateAvailable        *bool  `json:"update_available,omitempty"`
	LatestVersion          string `json:"latest_version,omitempty"`
	ZotioUpdateAvailable   *bool  `json:"zotio_update_available,omitempty"`
	ZotioLatestVersion     string `json:"zotio_latest_version,omitempty"`
}

func ping(ctx context.Context, raw json.RawMessage, system *bootstrap.System, updateRefreshInFlight *atomic.Bool) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	result := statusResult{Status: "ok", Version: Version}
	if system != nil && system.Browser != nil {
		result.ExtensionVersion, _, result.ExtensionConnected = system.Browser.SessionInfo()
		sessions, denied, _ := system.Browser.Sessions()
		for _, session := range sessions {
			if !session.Holder {
				result.PendingBrowserSessions++
			}
		}
		result.BrowserSessionDenied = denied
	}
	if system != nil && system.Updates != nil {
		refresh := updateRefreshInFlight.CompareAndSwap(false, true)
		available := false
		if info := system.Updates.Cached(); info != nil {
			result.LatestVersion = info.LatestVersion
			available = update.IsNewer(info.LatestVersion, Version)
		}
		result.UpdateAvailable = &available
		if refresh {
			checker := system.Updates
			// The refresh must outlive this RPC: WithoutCancel keeps the request's
			// values but detaches its cancellation so a returning ping cannot abort
			// the once-daily check.
			refreshCtx := context.WithoutCancel(ctx)
			go func() {
				defer updateRefreshInFlight.Store(false)
				_ = checker.Check(refreshCtx)
			}()
		}
		zotioAvailable := false
		if info, installed := update.NewZotio(system.Config.DataDir).CachedState(); info != nil {
			result.ZotioLatestVersion = info.LatestVersion
			zotioAvailable = installed != "" && update.IsNewer(info.LatestVersion, installed)
		}
		result.ZotioUpdateAvailable = &zotioAvailable
	}
	return marshal(result)
}

type acquireSubmitParams struct {
	Request    protocol.WorkRequest `json:"request"`
	AutoImport *bool                `json:"auto_import,omitempty"`
}

type acquireSubmitV2Params struct {
	Request    protocol.WorkRequest `json:"request"`
	AutoImport *bool                `json:"auto_import,omitempty"`
	Force      bool                 `json:"force,omitempty"`
}

func submitV2(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params acquireSubmitV2Params
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	result, err := system.App.SubmitWithOptionsAs(ctx, PrincipalFrom(ctx), params.Request, app.SubmitOptions{
		AutoImport: params.AutoImport,
		Force:      params.Force,
	})
	if err != nil {
		var unset *config.ErrAccessModeUnset
		if errors.As(err, &unset) {
			return nil, &ipc.RPCError{Code: "configuration_required", Message: unset.Error()}
		}
		return nil, &ipc.RPCError{Code: "invalid_argument", Message: "invalid acquisition request"}
	}
	return marshal(SubmitV2Result{JobID: result.JobID, Existing: result.Existing})
}

func submit(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return badParams(err)
	}
	var request protocol.WorkRequest
	var autoImport *bool
	if _, ok := envelope["request"]; ok {
		var params acquireSubmitParams
		if err := ipc.DecodeParams(raw, &params); err != nil {
			return badParams(err)
		}
		request = params.Request
		autoImport = params.AutoImport
	} else if err := ipc.DecodeParams(raw, &request); err != nil {
		return badParams(err)
	}
	submitted, err := system.App.SubmitWithOptionsAs(ctx, PrincipalFrom(ctx), request, app.SubmitOptions{AutoImport: autoImport})
	if err != nil {
		var unset *config.ErrAccessModeUnset
		if errors.As(err, &unset) {
			return nil, &ipc.RPCError{Code: "configuration_required", Message: unset.Error()}
		}
		return nil, &ipc.RPCError{Code: "invalid_argument", Message: "invalid acquisition request"}
	}
	return marshal(SubmitResult{JobID: submitted.JobID})
}

// BatchReport joins a persisted CLI batch manifest to the daemon's durable
// job/event state. It is shared by IPC and the in-process MCP surface.
func BatchReport(ctx context.Context, system *bootstrap.System, batchID string) (*batch.Report, error) {
	if system == nil || system.Jobs == nil {
		return nil, errors.New("batch reports are not configured")
	}
	manifest, err := batch.Load(system.Config.DataDir, batchID)
	if err != nil {
		return nil, err
	}
	return batch.BuildReport(ctx, manifest, system.Jobs)
}

func acquireReport(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params AcquireReportParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if strings.TrimSpace(params.BatchID) == "" {
		return badParams(errors.New("batch_id is required"))
	}
	report, err := BatchReport(ctx, system, params.BatchID)
	if err != nil {
		switch {
		case errors.Is(err, batch.ErrManifestNotFound):
			return nil, &ipc.RPCError{Code: "not_found", Message: safeMessage(err, "batch report not found")}
		case errors.Is(err, batch.ErrInvalidBatchID):
			return nil, &ipc.RPCError{Code: "invalid_argument", Message: safeMessage(err, "invalid batch id")}
		default:
			return nil, &ipc.RPCError{Code: "internal", Message: safeMessage(err, "batch report failed")}
		}
	}
	return marshal(report)
}

func zotioPreflight(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system.Zotio == nil || system.Zotio.CLI == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "Zotio integration is not configured"}
	}
	result, err := system.Zotio.CLI.Preflight(ctx)
	if err != nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: safeMessage(err, "Zotio preflight failed")}
	}
	update.NewZotio(system.Config.DataDir).RememberInstalledVersion(result.Version)
	return marshal(result)
}

func zotioQueue(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var options zotio.QueueOptions
	if err := ipc.DecodeParams(raw, &options); err != nil {
		return badParams(err)
	}
	if system.Zotio == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "Zotio integration is not configured"}
	}
	result, err := system.Zotio.QueueMissingPDF(ctx, options)
	if err != nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: safeMessage(err, "Zotio queue failed")}
	}
	return marshal(result)
}

func zotioMissingCount(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		Collection string `json:"collection"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system.Zotio == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "Zotio integration is not configured"}
	}
	count, err := system.Zotio.MissingPDFCount(ctx, params.Collection)
	if err != nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: safeMessage(err, "Zotio missing-PDF count failed")}
	}
	return marshal(map[string]int{"missing": count})
}

func zotioLookupWorks(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var request zotio.LookupWorksRequest
	if err := ipc.DecodeParams(raw, &request); err != nil {
		return badParams(err)
	}
	if system.Zotio == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "Zotio integration is not configured"}
	}
	result, err := system.Zotio.LookupWorks(ctx, request)
	if err != nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: safeMessage(err, "Zotio ownership lookup failed")}
	}
	return marshal(result)
}

// LibraryLookupWorksRequest asks the generic holdings providers about a bounded
// batch of works.
//
// This is a new method rather than a widened zotio.lookup_works because nothing
// on the wire is additive (internal/ipc rejects unknown fields on the whole
// envelope) and, more importantly, because the two answer different questions:
// zotio.lookup_works returns a Zotero routing decision whose owned_missing_pdf
// status carries an item key callers act on, while this returns destination-
// neutral holdings claims plus per-source completeness. zotio.lookup_works keeps
// its old shape *and* its old semantics (ADR-0008 invariant 8).
type LibraryLookupWorksRequest struct {
	Works []ownership.Query `json:"works"`
	// ExpectedFingerprint binds the lookup to the configuration that selected
	// generic holdings. A daemon is reused per data_dir/socket, and --config may
	// name a different config with the same data directory, so without this a
	// stale or shared daemon would answer authoritatively against another
	// client's library.sources — reporting an empty registry as a complete
	// all-negative result, or suppressing a paper the caller's library does not
	// hold. Safe to require because this method is new: no released caller omits
	// it.
	ExpectedFingerprint string `json:"expected_fingerprint"`
}

func libraryLookupWorks(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var request LibraryLookupWorksRequest
	if err := ipc.DecodeParams(raw, &request); err != nil {
		return badParams(err)
	}
	if request.ExpectedFingerprint == "" || system == nil || request.ExpectedFingerprint != system.Config.LibraryFingerprint() {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "library configuration does not match caller"}
	}
	// The fingerprint proves the SOURCES match; it cannot prove this daemon chose
	// to consult them. A daemon with the same library.sources but zotio enabled
	// leaves the generic registry empty, and answering from it would be a
	// complete-looking negative.
	if system.Holdings == nil || !system.Holdings.Enabled() {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "generic library authority is not active"}
	}
	if len(request.Works) == 0 || len(request.Works) > 50 {
		return nil, &ipc.RPCError{Code: "invalid_params", Message: "library lookup requires 1..50 works"}
	}
	return marshal(system.Holdings.Lookup(ctx, request.Works))
}

// WatchRemoveResult confirms removal of one scheduled watch.
type WatchRemoveResult struct {
	ID      int64 `json:"id"`
	Removed bool  `json:"removed"`
}

// WatchDigestResult contains recent alert-watch discoveries.
type WatchDigestResult struct {
	WatchID int64               `json:"watch_id"`
	Entries []watch.DigestEntry `json:"entries"`
}

func addWatch(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var input watch.CreateInput
	if err := ipc.DecodeParams(raw, &input); err != nil {
		return badParams(err)
	}
	if system == nil || system.Watches == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	created, err := system.Watches.Create(ctx, input)
	if err != nil {
		return badParams(err)
	}
	return marshal(created)
}

func listWatches(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Watches == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	watches, err := system.Watches.List(ctx)
	if err != nil {
		return failure(err)
	}
	return marshal(watches)
}

type CapturePurgeResult struct {
	Removed int `json:"removed"`
}

func listCaptures(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Captures == nil {
		return marshal([]any{})
	}
	rows, err := system.Captures.List(ctx)
	if err != nil {
		return failure(err)
	}
	if rows == nil {
		return marshal([]any{})
	}
	return marshal(rows)
}

func purgeCaptures(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		Host string `json:"host"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Captures == nil {
		return marshal(CapturePurgeResult{})
	}
	removed, err := system.Captures.Purge(ctx, params.Host)
	if err != nil {
		return failure(err)
	}
	return marshal(CapturePurgeResult{Removed: removed})
}

func watchDigest(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		ID    int64 `json:"id"`
		Limit int   `json:"limit"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || params.ID <= 0 {
		if err == nil {
			err = errors.New("watch id is required")
		}
		return badParams(err)
	}
	if system == nil || system.Watches == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	entries, err := system.Watches.Digest(ctx, params.ID, params.Limit)
	if err != nil {
		return failure(err)
	}
	return marshal(WatchDigestResult{WatchID: params.ID, Entries: entries})
}

func removeWatch(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params watch.IDInput
	if err := ipc.DecodeParams(raw, &params); err != nil || params.ID <= 0 {
		if err == nil {
			err = errors.New("watch id is required")
		}
		return badParams(err)
	}
	if system == nil || system.Watches == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	if err := system.Watches.Remove(ctx, params.ID); err != nil {
		return failure(err)
	}
	return marshal(WatchRemoveResult{ID: params.ID, Removed: true})
}

func runWatch(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params watch.IDInput
	if err := ipc.DecodeParams(raw, &params); err != nil || params.ID <= 0 {
		if err == nil {
			err = errors.New("watch id is required")
		}
		return badParams(err)
	}
	if system == nil || system.WatchRunner == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "watchlists are not configured"}
	}
	result, err := system.WatchRunner.Run(ctx, params.ID)
	if err != nil {
		return watchFailure(err)
	}
	return marshal(result)
}
func listJobs(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		State string `json:"state,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	rows, err := system.Jobs.List(ctx, params.State, params.Limit)
	if err != nil {
		return failure(err)
	}
	return marshal(rows)
}

// listJobsV2 is jobs.list with the one thing a cohort-scale consumer cannot
// derive for itself: whether the page it just read was the whole list. It is a
// new method rather than a widened jobs.list result because the IPC envelope is
// decoded with DisallowUnknownFields, so an added field would make every older
// CLI reject a newer daemon's response.
func listJobsV2(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		State string `json:"state,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	rows, truncated, err := system.Jobs.ListPage(ctx, params.State, params.Limit)
	if err != nil {
		return failure(err)
	}
	return marshal(agentjson.Envelope("jobs", rows, truncated))
}

// repairAwaitingHuman returns an ORPHANED parked job to resolving: one that is
// awaiting_human with no open action left to act on, which no other verb can
// reach (jobs.retry refuses parked jobs by design, and actions.open needs an
// open handoff action). It is deliberately orphan-only and never accepts action
// ids, so a consumer cannot close actions it never read; a job that still has
// open actions is reported as such rather than repaired. This does not weaken
// the "handoff offers do not hard-expire" decision: nothing expires here, and
// the transactional lease predicate inside the store keeps an in-flight adoption
// from losing its download.
func repairAwaitingHuman(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID string `json:"job_id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || strings.TrimSpace(params.JobID) == "" {
		if err == nil {
			err = errors.New("job_id is required")
		}
		return badParams(err)
	}
	row, err := system.Jobs.Get(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	if row.State != job.StateAwaitingHuman {
		return marshal(RepairResult{JobID: params.JobID, Outcome: "not_parked", State: row.State})
	}
	open, err := system.Jobs.ListOpenHumanActionsForJobs(ctx, []string{params.JobID})
	if err != nil {
		return failure(err)
	}
	if len(open) != 0 {
		return marshal(RepairResult{JobID: params.JobID, Outcome: "has_open_actions", State: row.State})
	}
	err = system.Jobs.RepairAwaitingHuman(ctx, params.JobID, nil, map[string]any{"reason": "orphan_repair"})
	switch {
	case errors.Is(err, job.ErrConflict):
		// The job was leased or left awaiting_human between the read above and
		// the transaction. The store is authoritative; report the race.
		return marshal(RepairResult{JobID: params.JobID, Outcome: "conflict", State: row.State})
	case err != nil:
		return failure(err)
	}
	return marshal(RepairResult{JobID: params.JobID, Repaired: true, Outcome: "repaired", State: job.StateResolving})
}

func jobReceipt(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID string `json:"job_id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || strings.TrimSpace(params.JobID) == "" {
		if err == nil {
			err = errors.New("job_id is required")
		}
		return badParams(err)
	}
	row, err := system.Jobs.Get(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	principal, err := system.Jobs.Principal(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	tiers, err := system.Jobs.AttemptedTiers(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	components, err := system.Jobs.Components(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	if components == nil {
		components = []job.Component{}
	}
	if tiers == nil {
		tiers = []string{}
	}
	// An empty reason stays empty: a job that has not ended has no reason, which
	// is not the same fact as "ended for an unrecognised reason".
	var reason job.TerminalReason
	if row.TerminalReason != "" {
		reason = job.NormalizeTerminalReason(row.TerminalReason)
	}
	receipt := Receipt{
		JobID:           row.ID,
		RequestID:       row.WorkRequestID,
		State:           row.State,
		Terminal:        job.Terminal(row.State),
		Principal:       string(principal),
		TerminalReason:  string(reason),
		AttemptedTiers:  tiers,
		Components:      components,
		BundleAvailable: row.State == job.StateReady || row.State == job.StateImported,
	}
	return marshal(receipt)
}

// addComponent files a supplement or appendix beside a job's main artifact. A
// quotation absent from a main PDF may sit in a supplement, so a consumer that
// reports "not found" without them would be making an accusation papio's own
// evidence does not support (ADR-0007).
func addComponent(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID string `json:"job_id"`
		Path  string `json:"path"`
		Role  string `json:"role"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if strings.TrimSpace(params.JobID) == "" || strings.TrimSpace(params.Path) == "" || strings.TrimSpace(params.Role) == "" {
		return badParams(errors.New("job_id, path, and role are required"))
	}
	if system == nil || system.App == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "acquisition service is not configured"}
	}
	if err := system.App.AdoptComponent(ctx, params.JobID, params.Path, params.Role); err != nil {
		// Every case below is an ordinary operator mistake, not a daemon fault, so
		// it must say what to change. The path sentinel deliberately does NOT echo
		// the wrapped error: confinement failures carry the caller's filesystem
		// path, and the daemon log is the place for that, not an RPC message.
		switch {
		case errors.Is(err, app.ErrComponentRole):
			return badParams(err)
		case errors.Is(err, app.ErrComponentPrecondition):
			log.Printf("rpc add_component precondition: %v", err)
			return nil, &ipc.RPCError{Code: "precondition_failed", Message: "the job holds no main artifact to attach a component to"}
		case errors.Is(err, app.ErrComponentPath):
			log.Printf("rpc add_component path rejected: %v", err)
			return nil, &ipc.RPCError{Code: "invalid_argument", Message: "the file must be a regular file inside the job's adoption root"}
		case errors.Is(err, app.ErrComponentRejected):
			return nil, &ipc.RPCError{Code: "invalid_argument", Message: safeMessage(err, "component rejected")}
		}
		return failure(err)
	}
	components, err := system.Jobs.Components(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	return marshal(agentjson.Envelope("components", components, false))
}

func getJob(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID string `json:"job_id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || strings.TrimSpace(params.JobID) == "" {
		if err == nil {
			err = errors.New("job_id is required")
		}
		return badParams(err)
	}
	row, err := system.Jobs.Get(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	events, err := system.Jobs.Events(ctx, params.JobID)
	if err != nil {
		return failure(err)
	}
	actions, err := system.Jobs.ListHumanActions(ctx, false)
	if err != nil {
		return failure(err)
	}
	jobActions := actions[:0]
	for _, action := range actions {
		if action.JobID == params.JobID {
			jobActions = append(jobActions, action)
		}
	}
	return marshal(JobDetail{Job: row, Events: events, Actions: jobActions})
}

func cancelJob(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID string `json:"job_id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || strings.TrimSpace(params.JobID) == "" {
		if err == nil {
			err = errors.New("job_id is required")
		}
		return badParams(err)
	}
	if err := system.Jobs.Cancel(ctx, params.JobID, "cancelled by user"); err != nil {
		return failure(err)
	}
	return marshal(map[string]any{"job_id": params.JobID, "cancelled": true})
}

func retryJob(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID string `json:"job_id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || strings.TrimSpace(params.JobID) == "" {
		if err == nil {
			err = errors.New("job_id is required")
		}
		return badParams(err)
	}
	if err := system.Jobs.Retry(ctx, params.JobID); err != nil {
		return failure(err)
	}
	return marshal(map[string]any{"job_id": params.JobID, "state": job.StateResolving})
}

func resolveAction(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	return resolveActionCAS(ctx, raw, system)
}

func listActions(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		OpenOnly *bool `json:"open_only,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	openOnly := true
	if params.OpenOnly != nil {
		openOnly = *params.OpenOnly
	}
	actions, err := system.Jobs.ListHumanActions(ctx, openOnly)
	if err != nil {
		return failure(err)
	}
	return marshal(actions)
}

// listActionsV2 bounds actions.list, which is unbounded today, and reports
// truncation as a proof. New method for the same wire reason as jobs.list_v2.
func listActionsV2(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		OpenOnly *bool `json:"open_only,omitempty"`
		Limit    int   `json:"limit,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	openOnly := true
	if params.OpenOnly != nil {
		openOnly = *params.OpenOnly
	}
	actions, truncated, err := system.Jobs.ListHumanActionsPage(ctx, openOnly, params.Limit)
	if err != nil {
		return failure(err)
	}
	return marshal(agentjson.Envelope("actions", actions, truncated))
}

func openActions(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params ActionsOpenParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if system == nil || system.Browser == nil {
		return marshal(ActionsOpenResult{})
	}
	queued, sessionLive, err := system.Browser.FocusHandoffs(ctx, params.JobIDs)
	if err != nil {
		return failure(err)
	}
	return marshal(ActionsOpenResult{Queued: queued, SessionLive: sessionLive})
}

func getArtifact(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID  string `json:"job_id,omitempty"`
		SHA256 string `json:"sha256,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if (params.JobID == "") == (params.SHA256 == "") {
		return badParams(errors.New("exactly one of job_id or sha256 is required"))
	}
	sha := params.SHA256
	if params.JobID != "" {
		row, err := system.Jobs.Get(ctx, params.JobID)
		if err != nil {
			return failure(err)
		}
		sha = row.ArtifactSHA256
		if sha == "" {
			return nil, &ipc.RPCError{Code: "not_found", Message: "job has no validated artifact"}
		}
	}
	artifact, err := system.Jobs.GetArtifact(ctx, sha)
	if err != nil {
		return failure(err)
	}
	if artifact == nil {
		return nil, &ipc.RPCError{Code: "not_found", Message: "artifact not found"}
	}
	return marshal(ArtifactResult{Artifact: artifact})
}

func exportBundle(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobID     string `json:"job_id"`
		OutputDir string `json:"output_dir"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || params.JobID == "" || params.OutputDir == "" {
		if err == nil {
			err = errors.New("job_id and output_dir are required")
		}
		return badParams(err)
	}
	path, result, err := system.Bundle.Export(ctx, params.JobID, params.OutputDir)
	if err != nil {
		return failure(err)
	}
	return marshal(BundleResult{Path: path, Bundle: result})
}

func runDoctor(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	return marshal(system.DoctorReport(ctx))
}

func zotioPlan(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		JobIDs []string `json:"job_ids"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	plans, err := system.Zotio.PlanJobs(ctx, params.JobIDs)
	if err != nil {
		return zotioFailure(err)
	}
	return marshal(map[string]any{"plans": plans})
}

func zotioTagsReconcile(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	result, err := system.Zotio.ReconcileTags(ctx)
	if err != nil {
		return zotioFailure(err)
	}
	return marshal(result)
}

func zotioApply(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		PlanID             string `json:"plan_id"`
		ConfirmationSHA256 string `json:"confirmation_sha256"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if params.PlanID == "" || params.ConfirmationSHA256 == "" {
		return badParams(errors.New("plan_id and confirmation_sha256 are required"))
	}
	result, err := system.Zotio.Apply(ctx, params.PlanID, params.ConfirmationSHA256)
	if err != nil {
		return zotioFailure(err)
	}
	return marshal(result)
}

func browserSync(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		SessionID string            `json:"session_id,omitempty"`
		Goodbye   bool              `json:"goodbye,omitempty"`
		Messages  []json.RawMessage `json:"messages,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	outbound, err := system.Browser.Sync(ctx, params.SessionID, params.Goodbye, params.Messages)
	if err != nil {
		if errors.Is(err, browser.ErrInvalidFrame) {
			// A fail-closed protocol violation is a client error.
			return nil, &ipc.RPCError{Code: "invalid_argument", Message: safeMessage(err, "invalid browser frame")}
		}
		return failure(err)
	}
	if outbound == nil {
		outbound = []json.RawMessage{}
	}
	return marshal(map[string]any{"outbound": outbound})
}

// browserSessions lists connected browser sessions and arbitration counters.
func browserSessions(raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	sessions, denied, takeovers := system.Browser.Sessions()
	if sessions == nil {
		sessions = []browser.SessionSummary{}
	}
	return marshal(map[string]any{"sessions": sessions, "denied_hellos": denied, "takeovers": takeovers})
}

// browserClaim promotes a pending browser session to holder.
func browserClaim(raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		SessionID string `json:"session_id"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if strings.TrimSpace(params.SessionID) == "" {
		return badParams(errors.New("session_id is required"))
	}
	resolved, err := system.Browser.Claim(params.SessionID)
	if err != nil {
		return nil, &ipc.RPCError{Code: "invalid_argument", Message: safeMessage(err, "unknown browser session")}
	}
	return marshal(map[string]any{"claimed": true, "session_id": resolved})
}

// searchDiscovery maps strict RPC input to the bounded OpenAlex client.
func searchDiscovery(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params discovery.SearchParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if strings.TrimSpace(params.Query) == "" && !params.HasCitationSnowball() {
		return badParams(errors.New("query is required unless a citation snowball DOI is supplied"))
	}
	if system == nil || system.Discovery == nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: "discovery is not configured"}
	}
	works, partial, err := searchWithPartialFailures(ctx, system.Discovery, params)
	// A backend that broke while another answered used to vanish here, leaving
	// a user unable to tell a dead backend from an unindexed work; a backend
	// that broke on every request used to log nothing at all, which is
	// backwards from what an operator needs. Both cases get the same
	// per-source log line here, unconditionally, before the error branch below
	// even runs. The messages are already sanitized by discovery; papio doctor
	// reports the same state.
	for _, failure := range partial {
		log.Printf("warning: discovery backend %s failed: %s", failure.Source, failure.Message)
	}
	if err != nil {
		// safeMessage bounds length but does not redact; the message built
		// below is already sanitized. SummarizeFailures names every backend
		// that broke — errors.As on a joined error only surfaces whichever
		// cause it finds first, which named one backend even when every
		// backend failed. partial is empty only when the configured source
		// does not implement PartialSearcher (a single backend, or a test
		// double), so SanitizeError on the raw error is still the right
		// fallback there.
		message := discovery.SummarizeFailures(partial)
		if message == "" {
			message = discovery.SanitizeError(err)
		}
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: safeMessage(errors.New(message), "discovery search failed")}
	}
	// Generic holdings sources answer only when zotio does not: mixed precedence
	// is out of scope (ADR-0008), and when zotio is configured its classification
	// stays exactly as it was.
	if system.Holdings.Enabled() {
		if warning := discovery.ClassifyHoldings(ctx, works, system.Holdings); warning != "" {
			log.Printf("warning: %s", warning)
		}
	} else {
		var lookup discovery.OwnershipLookup
		if system.Zotio != nil {
			lookup = system.Zotio
		}
		if warning := discovery.ClassifyOwnership(ctx, works, lookup); warning != "" {
			log.Printf("warning: %s", warning)
		}
	}
	return marshal(works)
}

// searchWithPartialFailures prefers the partial-aware search when the configured
// discovery source supports it, so a backend that failed alongside a successful
// one can still be reported. A source that does not (a single backend, or a test
// double) keeps the plain path.
func searchWithPartialFailures(ctx context.Context, source discovery.Source, params discovery.SearchParams) ([]discovery.DiscoveredWork, []discovery.BackendFailure, error) {
	if partial, ok := source.(discovery.PartialSearcher); ok {
		return partial.SearchPartial(ctx, params)
	}
	works, err := source.Search(ctx, params)
	return works, nil, err
}

func marshal(value any) ([]byte, *ipc.RPCError) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, &ipc.RPCError{Code: "internal", Message: "unable to encode daemon response"}
	}
	return data, nil
}

func badParams(err error) ([]byte, *ipc.RPCError) {
	return nil, &ipc.RPCError{Code: "invalid_argument", Message: safeMessage(err, "invalid parameters")}
}

func failure(err error) ([]byte, *ipc.RPCError) {
	var actionKind *job.ErrHumanActionKind
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, &ipc.RPCError{Code: "not_found", Message: "record not found"}
	case errors.Is(err, job.ErrConflict):
		return nil, &ipc.RPCError{Code: "conflict", Message: safeMessage(err, "state conflict")}
	case errors.As(err, &actionKind):
		return nil, &ipc.RPCError{Code: "invalid_argument", Message: safeMessage(err, "unsupported human action")}
	default:
		log.Printf("rpc internal error: %v", err)
		return nil, &ipc.RPCError{Code: "internal", Message: "operation failed"}
	}
}

func zotioFailure(err error) ([]byte, *ipc.RPCError) {
	info := zotio.ErrorInfoFrom(err)
	log.Printf("rpc zotio error [%s]: %v", info.Class, err)
	detail := &ipc.ErrorDetail{
		ErrorClass:      info.Class,
		ErrorHint:       info.Hint,
		ErrorHTTPStatus: info.HTTPStatus,
	}
	return nil, &ipc.RPCError{Code: "internal", Message: "operation failed", Detail: detail}
}

func watchFailure(err error) ([]byte, *ipc.RPCError) {
	info := zotio.ErrorInfoFrom(err)
	log.Printf("rpc watch error [%s]: %v", info.Class, err)
	if info.Class == zotio.ErrorClassUnknown {
		info.Class = "watch_execution_failed"
		info.Hint = watchErrorHint(err)
	}
	return nil, &ipc.RPCError{
		Code: "internal", Message: "watch execution failed",
		Detail: &ipc.ErrorDetail{
			ErrorClass:      info.Class,
			ErrorHint:       info.Hint,
			ErrorHTTPStatus: info.HTTPStatus,
		},
	}
}

func watchErrorHint(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "response exceeds configured limit"):
		return "OpenAlex response exceeds configured limit"
	case strings.Contains(message, "invalid OpenAlex response"):
		return "OpenAlex response was invalid"
	case strings.Contains(message, "OpenAlex returned HTTP"):
		return "OpenAlex returned an error response"
	case strings.Contains(message, "contact email is required"):
		return "OpenAlex contact email is required"
	default:
		return "watch execution failed"
	}
}

func safeMessage(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	message := strings.TrimSpace(err.Error())
	if message == "" || len(message) > 500 || strings.ContainsAny(message, "\r\n") {
		return fallback
	}
	return message
}
