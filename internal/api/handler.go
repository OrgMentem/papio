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
	"time"

	"papio/internal/batch"
	"papio/internal/bootstrap"
	"papio/internal/browser"
	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/zotio"
)

const Version = "0.1.0-dev"

type SubmitResult struct {
	JobID string `json:"job_id"`
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

// Router returns the complete Phase 1 local RPC surface.
func Router(system *bootstrap.System) ipc.Router {
	return RouterWithShutdown(system, nil)
}

// RouterWithShutdown adds the process-lifecycle method used by `daemon stop`.
// The delayed callback lets the successful response flush before cancellation.
func RouterWithShutdown(system *bootstrap.System, shutdown context.CancelFunc) ipc.Router {
	methods := map[string]ipc.MethodHandler{
		"ping": ping,
		"acquire.submit": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return submit(ctx, raw, system)
		},
		"acquire.report": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return acquireReport(ctx, raw, system)
		},
		"discovery.search": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return searchDiscovery(ctx, raw, system)
		},
		"jobs.list": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return listJobs(ctx, raw, system)
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
		"zotio.lookup_works": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioLookupWorks(ctx, raw, system)
		},
		"zotio.plan": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioPlan(ctx, raw, system)
		},
		"zotio.apply": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return zotioApply(ctx, raw, system)
		},
		"browser.sync": func(ctx context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
			return browserSync(ctx, raw, system)
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

func ping(_ context.Context, raw json.RawMessage) ([]byte, *ipc.RPCError) {
	var params struct{}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	return marshal(map[string]string{"status": "ok", "version": Version})
}

type acquireSubmitParams struct {
	Request    protocol.WorkRequest `json:"request"`
	AutoImport *bool                `json:"auto_import,omitempty"`
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
	id, err := system.App.SubmitWithAutoImport(ctx, request, autoImport)
	if err != nil {
		var unset *config.ErrAccessModeUnset
		if errors.As(err, &unset) {
			return nil, &ipc.RPCError{Code: "configuration_required", Message: unset.Error()}
		}
		return nil, &ipc.RPCError{Code: "invalid_argument", Message: "invalid acquisition request"}
	}
	return marshal(SubmitResult{JobID: id})
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
		return nil, &ipc.RPCError{Code: "not_found", Message: safeMessage(err, "batch report not found")}
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
	var params struct {
		ActionID int64  `json:"action_id"`
		Verdict  string `json:"verdict"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil || params.ActionID <= 0 || (params.Verdict != "accept" && params.Verdict != "reject") {
		if err == nil {
			err = errors.New("action_id and verdict (accept or reject) are required")
		}
		return badParams(err)
	}
	jobID, state, err := system.Jobs.ResolveReview(ctx, params.ActionID, params.Verdict)
	if err != nil {
		return failure(err)
	}
	return marshal(map[string]any{"job_id": jobID, "state": state})
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
		Messages []json.RawMessage `json:"messages,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	outbound, err := system.Browser.Sync(ctx, params.Messages)
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
	works, err := system.Discovery.Search(ctx, params)
	if err != nil {
		return nil, &ipc.RPCError{Code: "precondition_failed", Message: safeMessage(err, "discovery search failed")}
	}
	var lookup discovery.OwnershipLookup
	if system.Zotio != nil {
		lookup = system.Zotio
	}
	if warning := discovery.ClassifyOwnership(ctx, works, lookup); warning != "" {
		log.Printf("warning: %s", warning)
	}
	return marshal(works)
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
		return nil, &ipc.RPCError{Code: "internal", Message: "operation failed"}
	}
}

func zotioFailure(err error) ([]byte, *ipc.RPCError) {
	info := zotio.ErrorInfoFrom(err)
	detail := &ipc.ErrorDetail{
		ErrorClass:      info.Class,
		ErrorHint:       info.Hint,
		ErrorHTTPStatus: info.HTTPStatus,
	}
	return nil, &ipc.RPCError{Code: "internal", Message: "operation failed", Detail: detail}
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
