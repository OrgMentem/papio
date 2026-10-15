// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"papio/internal/batch"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

// TestChangesCursorReplaysEachEventOnceAcrossReconnects walks the feed in small
// pages, reconnects a fresh client between pages, and appends events between
// calls: every durable event must arrive exactly once, in seq order, and a
// batch scope must see only its own jobs.
func TestChangesCursorReplaysEachEventOnceAcrossReconnects(t *testing.T) {
	ctx := context.Background()
	system := newResourceTestSystem(t)
	deps := toolDependencies{now: time.Now, wait: waitForPoll}
	policy := job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any"}
	jobA, err := system.Jobs.CreateRequest(ctx, "changes-a", work.Work{DOI: "10.1000/changes-a"}, "", "", policy, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	jobB, err := system.Jobs.CreateRequest(ctx, "changes-b", work.Work{DOI: "10.1000/changes-b"}, "", "", policy, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	record := func(jobID, kind string) {
		t.Helper()
		if err := system.Jobs.RecordEvent(ctx, jobID, kind, map[string]any{"n": kind}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		record(jobA, fmt.Sprintf("test.a%d", i))
		record(jobB, fmt.Sprintf("test.b%d", i))
	}

	var seen []int64
	var cursor int64
	pages := 0
	for {
		c := newTestClient(t, system, deps, emptyFactory)
		var page ChangesOutput
		callToolJSON(t, c, "papio_changes", map[string]any{"after_seq": float64(cursor), "limit": float64(2)}, &page)
		if len(page.Changes) > 2 {
			t.Fatalf("page has %d changes, limit 2", len(page.Changes))
		}
		for _, change := range page.Changes {
			seen = append(seen, change.Seq)
		}
		cursor = page.NextSeq
		pages++
		if pages == 2 {
			// A change committed mid-walk lands after the cursor and is not lost.
			record(jobB, "test.late")
		}
		if !page.Truncated {
			break
		}
		if pages > 50 {
			t.Fatal("feed never settled")
		}
	}

	var want []int64
	rows, err := system.Store.DB().QueryContext(ctx, `SELECT seq FROM events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		want = append(want, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if !slices.Equal(seen, want) {
		t.Fatalf("cursor walk saw seqs %v, events table holds %v", seen, want)
	}

	// An empty page keeps the cursor where it was.
	c := newTestClient(t, system, deps, emptyFactory)
	var empty ChangesOutput
	callToolJSON(t, c, "papio_changes", map[string]any{"after_seq": float64(cursor)}, &empty)
	if len(empty.Changes) != 0 || empty.Truncated || empty.NextSeq != cursor {
		t.Fatalf("caught-up page = %+v, want empty at next_seq %d", empty, cursor)
	}

	manifest := &batch.Manifest{
		SchemaVersion: batch.SchemaVersion, ID: "batch-abababababababababababababababab", CreatedAt: "2026-10-15T00:00:00Z",
		Works: []batch.ManifestWork{
			{RequestID: "r-a", JobID: jobA, Status: "submitted", Work: protocol.WorkRequest{SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "r-a", Identifiers: &protocol.Identifiers{DOI: "10.1000/changes-a"}}},
			{RequestID: "r-skip", Status: "skipped_owned", Work: protocol.WorkRequest{SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "r-skip", Identifiers: &protocol.Identifiers{DOI: "10.1000/owned"}}},
		},
	}
	if err := batch.Write(system.Config.DataDir, manifest); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []map[string]any{{"batch_id": manifest.ID}, {"batch_id": "latest"}, {"job_id": jobA}} {
		var scoped ChangesOutput
		callToolJSON(t, c, "papio_changes", scope, &scoped)
		if len(scoped.Changes) == 0 {
			t.Fatalf("scope %v returned no changes", scope)
		}
		for _, change := range scoped.Changes {
			if change.JobID != jobA {
				t.Fatalf("scope %v leaked change %+v", scope, change)
			}
		}
	}
}

func TestChangesRejectsBadArguments(t *testing.T) {
	system := newResourceTestSystem(t)
	c := newTestClient(t, system, toolDependencies{now: time.Now, wait: waitForPoll}, emptyFactory)
	// A work still being submitted has no job id yet; its job may already be
	// writing events, so a batch scope must refuse rather than page past them.
	inFlight := &batch.Manifest{
		SchemaVersion: batch.SchemaVersion, ID: "batch-cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd", CreatedAt: "2026-10-15T00:00:00Z",
		Works: []batch.ManifestWork{{RequestID: "r-x", Status: "submitted", Work: protocol.WorkRequest{SchemaVersion: protocol.WorkRequestSchemaVersion, RequestID: "r-x", Identifiers: &protocol.Identifiers{DOI: "10.1000/x"}}}},
	}
	if err := batch.Write(system.Config.DataDir, inFlight); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"negative cursor", map[string]any{"after_seq": float64(-1)}, "after_seq must be a non-negative whole number"},
		{"fractional cursor", map[string]any{"after_seq": 1.5}, "after_seq must be a non-negative whole number"},
		{"string cursor", map[string]any{"after_seq": "3"}, "after_seq must be a non-negative whole number"},
		{"limit too large", map[string]any{"limit": float64(maxChangesLimit + 1)}, fmt.Sprintf("limit must be 1 to %d", maxChangesLimit)},
		{"both scopes", map[string]any{"batch_id": "latest", "job_id": "x"}, "pass batch_id or job_id, not both"},
		{"unknown batch", map[string]any{"batch_id": "batch-00000000"}, `batch "batch-00000000": batch manifest not found`},
		{"malformed batch", map[string]any{"batch_id": "../etc"}, `batch "../etc": invalid batch ID "../etc"`},
		{"numeric job scope", map[string]any{"job_id": float64(123)}, "job_id must be a non-empty string"},
		{"blank job scope", map[string]any{"job_id": "  "}, "job_id must be a non-empty string"},
		{"bad batch type beside job", map[string]any{"batch_id": true, "job_id": "x"}, "batch_id must be a non-empty string"},
		{"batch still submitting", map[string]any{"batch_id": inFlight.ID}, `batch "batch-cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd" work 1 has no job yet (submission still running or interrupted); call again once papio_acquire_batch returns, or scope by job_id`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, c, "papio_changes", tc.args)
			if !res.IsError || resultText(res) != tc.want {
				t.Fatalf("result = %v %q, want error %q", res.IsError, resultText(res), tc.want)
			}
		})
	}
}
