// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"papio/internal/batch"
	"papio/internal/bootstrap"
)

const (
	defaultChangesLimit = 100
	maxChangesLimit     = 500
	// maxSafeJSONInteger is the largest integer a JSON number carries exactly
	// as float64; a cursor beyond it could not round-trip through a client.
	maxSafeJSONInteger = 1<<53 - 1
)

// Change is one durable event row. Seq is the events table's AUTOINCREMENT
// key: SQLite admits one writer at a time, so a reader never sees a higher seq
// commit before a lower one, and seq order is commit order.
type Change struct {
	Seq    int64           `json:"seq"`
	JobID  string          `json:"job_id,omitempty"`
	At     string          `json:"at"`
	Kind   string          `json:"kind"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

// ChangesOutput is one page of the change feed. NextSeq is the cursor for the
// following call: the seq of the last returned change, or the caller's
// after_seq when the page is empty.
type ChangesOutput struct {
	Changes   []Change `json:"changes"`
	Truncated bool     `json:"truncated"`
	NextSeq   int64    `json:"next_seq"`
}

func registerChangesTool(s *server.MCPServer, system *bootstrap.System) {
	s.AddTool(mcplib.NewTool("papio_changes",
		mcplib.WithDescription("Read durable job events after a cursor, oldest first. Pass the previous result's next_seq as after_seq to receive each change exactly once across reconnects and daemon restarts; truncated is true when more changes are already waiting. Optionally scope to one batch or one job. Does not wait: call again later, or use papio_batch_wait to block."),
		mcplib.WithNumber("after_seq", mcplib.Description("Return events with seq greater than this; 0 or omitted starts at the beginning.")),
		mcplib.WithNumber("limit", mcplib.Description(fmt.Sprintf("Maximum changes to return, 1 to %d; 0 or omitted defaults to %d.", maxChangesLimit, defaultChangesLimit))),
		mcplib.WithString("batch_id", mcplib.Description("Only events for jobs in this batch (an ID from papio_acquire_batch, or \"latest\").")),
		mcplib.WithString("job_id", mcplib.Description("Only events for this job.")),
		mcplib.WithReadOnlyHintAnnotation(true),
	), handleChanges(system))
}

func handleChanges(system *bootstrap.System) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if system == nil || system.Store == nil {
			return mcplib.NewToolResultError("the change feed is not configured"), nil
		}
		args := req.GetArguments()
		afterSeq, err := nonNegativeIntegerArg(args, "after_seq")
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		limit, err := nonNegativeIntegerArg(args, "limit")
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		if limit == 0 {
			limit = defaultChangesLimit
		}
		if limit > maxChangesLimit {
			return mcplib.NewToolResultError(fmt.Sprintf("limit must be 1 to %d", maxChangesLimit)), nil
		}
		// A scope that fails to parse must not fall back to the unscoped
		// feed, so a wrong type or a blank value is an error, not absent.
		batchID, err := optionalNonBlankStringArg(args, "batch_id")
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		jobID, err := optionalNonBlankStringArg(args, "job_id")
		if err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		if batchID != "" && jobID != "" {
			return mcplib.NewToolResultError("pass batch_id or job_id, not both"), nil
		}
		var jobIDs []string
		scoped := false
		switch {
		case jobID != "":
			jobIDs, scoped = []string{jobID}, true
		case batchID != "":
			manifest, err := batch.Load(system.Config.DataDir, batchID)
			if err != nil {
				if errors.Is(err, batch.ErrManifestNotFound) || errors.Is(err, batch.ErrInvalidBatchID) {
					return mcplib.NewToolResultError(fmt.Sprintf("batch %q: %v", batchID, err)), nil
				}
				return nil, err
			}
			scoped = true
			for i, w := range manifest.Works {
				if w.JobID != "" {
					jobIDs = append(jobIDs, w.JobID)
					continue
				}
				// A work still in submission has no job id yet, but its
				// job may already be writing events. Paging past them now
				// and adding the job later would skip those events for good.
				if w.Status != "skipped_owned" && w.Status != "submission_failed" {
					return mcplib.NewToolResultError(fmt.Sprintf("batch %q work %d has no job yet (submission still running or interrupted); call again once papio_acquire_batch returns, or scope by job_id", manifest.ID, i+1)), nil
				}
			}
		}
		output, err := readChanges(ctx, system, afterSeq, int(limit), scoped, jobIDs)
		if err != nil {
			return nil, err
		}
		return jsonToolResult(output)
	}
}

// readChanges pages the events table by seq. When scoped is true only events
// for jobIDs match; a scope with no jobs (a batch whose works were all skipped)
// matches nothing rather than everything.
func readChanges(ctx context.Context, system *bootstrap.System, afterSeq int64, limit int, scoped bool, jobIDs []string) (ChangesOutput, error) {
	output := ChangesOutput{Changes: []Change{}, NextSeq: afterSeq}
	if scoped && len(jobIDs) == 0 {
		return output, nil
	}
	query := `SELECT seq, COALESCE(job_id, ''), at, kind, detail_json FROM events WHERE seq > ?`
	params := []any{afterSeq}
	if scoped {
		query += ` AND job_id IN (?` + strings.Repeat(`,?`, len(jobIDs)-1) + `)`
		for _, id := range jobIDs {
			params = append(params, id)
		}
	}
	query += ` ORDER BY seq LIMIT ?`
	params = append(params, limit+1)
	rows, err := system.Store.DB().QueryContext(ctx, query, params...)
	if err != nil {
		return ChangesOutput{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var change Change
		var detail string
		if err := rows.Scan(&change.Seq, &change.JobID, &change.At, &change.Kind, &detail); err != nil {
			return ChangesOutput{}, err
		}
		if detail != "" && detail != "{}" && json.Valid([]byte(detail)) {
			change.Detail = json.RawMessage(detail)
		}
		output.Changes = append(output.Changes, change)
	}
	if err := rows.Err(); err != nil {
		return ChangesOutput{}, err
	}
	if len(output.Changes) > limit {
		output.Changes = output.Changes[:limit]
		output.Truncated = true
	}
	if n := len(output.Changes); n > 0 {
		output.NextSeq = output.Changes[n-1].Seq
	}
	return output, nil
}

// nonNegativeIntegerArg reads an optional whole, non-negative JSON number.
// Absent means 0.
func nonNegativeIntegerArg(args map[string]any, key string) (int64, error) {
	raw, present := args[key]
	if !present || raw == nil {
		return 0, nil
	}
	value, ok := raw.(float64)
	if !ok || math.IsNaN(value) || value != math.Trunc(value) || value < 0 || value > maxSafeJSONInteger {
		return 0, fmt.Errorf("%s must be a non-negative whole number", key)
	}
	return int64(value), nil
}

// optionalNonBlankStringArg reads an optional string argument. Absent means
// "", but a present value must be a non-blank string.
func optionalNonBlankStringArg(args map[string]any, key string) (string, error) {
	raw, present := args[key]
	if !present || raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}
	return strings.TrimSpace(value), nil
}
