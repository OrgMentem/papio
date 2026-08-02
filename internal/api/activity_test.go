// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/work"
)

func TestActivityListJoinsJobsAndReportsTruncation(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	jobID, err := system.Jobs.CreateRequest(ctx, "wr_activity_api", work.Work{Title: "Activity API title"}, "", "", job.Policy{
		AccessMode:     config.ModeConservative,
		DesiredVersion: "any",
		FetchMaxBytes:  1 << 20,
	}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Store.AppendEvent(ctx, jobID, "activity.first", map[string]any{"step": 1}); err != nil {
		t.Fatal(err)
	}
	if err := system.Store.AppendEvent(ctx, jobID, "activity.second", map[string]any{"step": 2}); err != nil {
		t.Fatal(err)
	}

	var page ActivityPage
	if rpcErr := callMethod(t, Router(system), "activity.list", map[string]any{
		"limit":  1,
		"job_id": jobID,
	}, &page); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(page.Entries) != 1 || !page.Truncated {
		t.Fatalf("page = %+v, want one entry and truncated=true", page)
	}
	entry := page.Entries[0]
	if entry.JobID != jobID || entry.JobTitle != "Activity API title" || entry.JobState != job.StateQueued {
		t.Fatalf("joined entry = %+v", entry)
	}
	if entry.Kind != "activity.second" || entry.Detail["step"] != float64(2) {
		t.Fatalf("entry = %+v, want newest detail", entry)
	}
}
