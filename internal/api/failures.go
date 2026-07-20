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
	since, err := parseFailuresSince(params.Since, time.Now().UTC())
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
		result.Since = since.UTC().Format(time.RFC3339Nano)
	}
	return marshal(result)
}

func parseFailuresSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
		if err != nil || days > int64(time.Duration(1<<63-1)/(24*time.Hour)) || days < -int64(time.Duration(1<<63-1)/(24*time.Hour)) {
			return time.Time{}, errors.New("since must be a duration or RFC3339 timestamp")
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return now.Add(-duration), nil
	}
	timestamp, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("since must be a duration or RFC3339 timestamp")
	}
	return timestamp.UTC(), nil
}
