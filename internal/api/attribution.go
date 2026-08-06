// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"papio/internal/agentjson"
	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/job"
)

// JobRow is a job listing row plus the consumer that submitted it.
//
// A wrapper rather than two more fields on job.Row: job.Row is the body of
// jobs.get and every jobs.list*, all decoded with DisallowUnknownFields, so
// widening it would make every already-installed papio reject a newer daemon's
// response. Embedding keeps the ratified keys byte-identical and adds exactly
// one — the shape an older reader would have seen, plus attribution.
//
// Consumer is a pointer so an unattributed job omits the key entirely instead of
// claiming a consumer named "". Absent means nobody claimed it; there is no
// backfill and no default.
type JobRow struct {
	job.Row
	Consumer *string `json:"consumer,omitempty"`
}

// ActionRow is a human-action listing row plus attribution and staleness.
//
// AgeSeconds and Stale are both present on purpose. Age is the fact; Stale is
// the daemon's verdict against the configured actions.stale_after_seconds, so
// every consumer of one daemon agrees on which rows are abandoned instead of
// each inventing a threshold. Nothing acts on Stale: it does not expire, cancel,
// or sweep anything, because giving up on an acquisition is a person's decision.
type ActionRow struct {
	job.HumanAction
	Consumer   *string `json:"consumer,omitempty"`
	AgeSeconds int64   `json:"age_seconds"`
	Stale      bool    `json:"stale"`
}

// JobsPageV3 and ActionsPageV3 decode the jobs.list_v3 / actions.list_v3
// envelopes: the same proven `truncated` contract as their v2 predecessors, with
// attributed rows.
type JobsPageV3 struct {
	Jobs      []JobRow `json:"jobs"`
	Truncated bool     `json:"truncated"`
}

type ActionsPageV3 struct {
	Actions   []ActionRow `json:"actions"`
	Truncated bool        `json:"truncated"`
}

// JobDetailV2 is jobs.get with attribution and per-action staleness. The nested
// rows are the wrapper types, so a reader that already parses JobDetail parses
// this too, and finds the added keys where it expects them.
type JobDetailV2 struct {
	Job     *JobRow          `json:"job"`
	Events  []map[string]any `json:"events"`
	Actions []ActionRow      `json:"actions"`
}

// ValidationResult is the artifacts.validation result: every validation verdict
// recorded for one job, newest first.
//
// Each report is JSON TEXT carrying its own schema_version, for the reason
// bundle.document's is: results decode with DisallowUnknownFields recursively,
// so an inline object would freeze the evidence shape into this method and force
// a new method name for every field the pipeline learns to report. SchemaVersion
// is lifted out so a consumer routes to a decoder without parsing first.
type ValidationResult struct {
	JobID   string             `json:"job_id"`
	Reports []ValidationReport `json:"reports"`
}

// ValidationReport is one candidate's verdict and its full evidence document.
type ValidationReport struct {
	CandidateID   int64  `json:"candidate_id"`
	SHA256        string `json:"sha256,omitempty"`
	Outcome       string `json:"outcome"`
	RecordedAt    string `json:"recorded_at"`
	Accepted      bool   `json:"accepted"`
	SchemaVersion string `json:"schema_version"`
	Document      string `json:"document"`
}

// listJobsV3 is jobs.list_v2 with consumer attribution and a consumer filter.
// New method, not a widened result, for the reason listJobsV2 was: the envelope
// is decoded fail-closed in both directions.
func listJobsV3(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		State    string `json:"state,omitempty"`
		Limit    int    `json:"limit,omitempty"`
		Consumer string `json:"consumer,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	rows, truncated, err := system.Jobs.ListPageFor(ctx, params.State, strings.TrimSpace(params.Consumer), params.Limit)
	if err != nil {
		return failure(err)
	}
	consumers, err := system.Jobs.ConsumersFor(ctx, jobIDsOf(rows))
	if err != nil {
		return failure(err)
	}
	out := make([]JobRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, JobRow{Row: row, Consumer: consumerOf(consumers, row.ID)})
	}
	return marshal(agentjson.Envelope("jobs", out, truncated))
}

// listActionsV3 is actions.list_v2 with attribution, staleness, and a consumer
// filter.
func listActionsV3(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params struct {
		OpenOnly *bool  `json:"open_only,omitempty"`
		Limit    int    `json:"limit,omitempty"`
		Consumer string `json:"consumer,omitempty"`
	}
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	openOnly := true
	if params.OpenOnly != nil {
		openOnly = *params.OpenOnly
	}
	rows, truncated, err := system.Jobs.ListHumanActionsPageFor(ctx, openOnly, strings.TrimSpace(params.Consumer), params.Limit)
	if err != nil {
		return failure(err)
	}
	staleAfter := system.Config.Actions.EffectiveActionStaleAfter()
	now := time.Now().UTC()
	out := make([]ActionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, actionRow(row, staleAfter, now))
	}
	return marshal(agentjson.Envelope("actions", out, truncated))
}

// getJobV2 is jobs.get with attribution and per-action staleness.
func getJobV2(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	jobID, rpcErr := requireJobID(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	row, err := system.Jobs.Get(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	events, err := system.Jobs.Events(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	consumer, recorded, err := system.Jobs.Consumer(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	attributed, err := system.Jobs.ListHumanActionsForJob(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	staleAfter := system.Config.Actions.EffectiveActionStaleAfter()
	now := time.Now().UTC()
	actions := make([]ActionRow, 0, len(attributed))
	for _, action := range attributed {
		actions = append(actions, actionRow(action, staleAfter, now))
	}
	detail := JobDetailV2{Job: &JobRow{Row: *row}, Events: events, Actions: actions}
	if recorded {
		detail.Job.Consumer = &consumer
	}
	return marshal(detail)
}

// validationReports is artifacts.validation: the complete stage-by-stage
// evidence for every candidate this job validated, which artifacts.get cannot
// carry (it returns the shared, content-addressed artifact row, and ADR-0007
// forbids projecting one job's identity decision through it).
//
// A job validated before this evidence was persisted returns an empty list
// rather than a reconstruction, because there is nothing to reconstruct from.
func validationReports(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	jobID, rpcErr := requireJobID(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	row, err := system.Jobs.Get(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	if row == nil {
		return nil, &ipc.RPCError{Code: "not_found", Message: "job not found"}
	}
	records, err := system.Jobs.ValidationReports(ctx, jobID)
	if err != nil {
		return failure(err)
	}
	result := ValidationResult{JobID: jobID, Reports: make([]ValidationReport, 0, len(records))}
	for _, record := range records {
		result.Reports = append(result.Reports, ValidationReport{
			CandidateID: record.CandidateID, SHA256: record.SHA256, Outcome: record.Outcome,
			RecordedAt: record.RecordedAt,
			// Accepted names the candidate whose bytes this job actually kept,
			// so a consumer never has to infer it from a rejected report that
			// happens to share the artifact hash.
			Accepted:      record.CandidateID != 0 && record.CandidateID == row.SelectedCandidateID,
			SchemaVersion: documentSchemaVersion(record.Document),
			Document:      record.Document,
		})
	}
	return marshal(result)
}

func actionRow(row job.AttributedAction, staleAfter time.Duration, now time.Time) ActionRow {
	out := ActionRow{HumanAction: row.Action}
	if row.Consumer != "" {
		consumer := row.Consumer
		out.Consumer = &consumer
	}
	created, err := time.Parse(time.RFC3339Nano, row.Action.CreatedAt)
	if err != nil {
		// An unparseable timestamp is not evidence of staleness. Reporting age
		// 0 and stale false says "I cannot tell", which is true; guessing stale
		// would mark rows abandoned on a formatting bug.
		return out
	}
	age := now.Sub(created)
	if age < 0 {
		age = 0
	}
	out.AgeSeconds = int64(age / time.Second)
	// Resolved actions are history, not a queue: only an action still open can
	// be waiting for someone.
	out.Stale = row.Action.Status == "open" && age >= staleAfter
	return out
}

func jobIDsOf(rows []job.Row) []string {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func consumerOf(consumers map[string]string, jobID string) *string {
	name, ok := consumers[jobID]
	if !ok || name == "" {
		return nil
	}
	return &name
}

// documentSchemaVersion reads the version the stored document declares for
// itself, rather than stamping the reader's own constant onto a row written by
// an older binary.
func documentSchemaVersion(document string) string {
	var envelope struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal([]byte(document), &envelope); err != nil {
		return ""
	}
	return envelope.SchemaVersion
}

// handoffOpenedEvent records that a human handoff was driven onto the operator's
// screen, with the owning consumer and the batch size. It is the audit trail
// ADR-0014 Decision 6 relies on instead of a gate: "consumer X opened N human
// actions in M minutes" is answerable from the event stream, and a rate limit
// would have obstructed the operator it was meant to protect.
const handoffOpenedEvent = "handoff.opened"
