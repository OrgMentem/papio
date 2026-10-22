// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"papio/internal/bootstrap"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/store"
)

type failuresParams struct {
	Since string `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
type failuresResult struct {
	Failures []job.FailureGroup `json:"failures"`
	Since    string             `json:"since,omitempty"`
}

func listFailures(ctx context.Context, raw json.RawMessage, system *bootstrap.System) ([]byte, *ipc.RPCError) {
	var params failuresParams
	if err := ipc.DecodeParams(raw, &params); err != nil {
		return badParams(err)
	}
	if params.Limit < 0 {
		return badParams(errors.New("limit must not be negative"))
	}
	since, err := ParseSince(params.Since, time.Now().UTC())
	if err != nil {
		return badParams(err)
	}
	failures, err := system.Jobs.Failures(ctx, since, params.Limit)
	if err != nil {
		return failure(err)
	}
	if failures == nil {
		failures = []job.FailureGroup{}
	}
	result := failuresResult{Failures: failures}
	if !since.IsZero() {
		result.Since = store.FormatTime(since)
	}
	return marshal(result)
}

// ParseSince is the one parser for every --since lookback: a Go duration
// ("24h"), a whole number of days ("7d", which time.ParseDuration lacks), or an
// RFC3339 instant. The daemon's jobs.failures/jobs.incidents and the CLI's
// export ledger and stats producers all use it, so a form that works on one
// command works on all of them. An empty value is the zero time (no cutoff).
func ParseSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
		if err != nil || days < 0 || days > int64(time.Duration(1<<63-1)/(24*time.Hour)) {
			return time.Time{}, errSinceForm
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration < 0 {
			return time.Time{}, errSinceForm
		}
		return now.Add(-duration), nil
	}
	timestamp, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errSinceForm
	}
	return timestamp.UTC(), nil
}

var errSinceForm = errors.New("since must be a non-negative duration (such as 24h or 7d) or RFC3339 timestamp")
