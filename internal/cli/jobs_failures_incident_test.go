// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/incident"
)

func TestJobsFailuresIncidentRowsUseEnvelope(t *testing.T) {
	var out, errOut bytes.Buffer
	first := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	want := incident.Group{
		Fingerprint: "0123456789abcdef0123456789abcdef", SafetyDomain: "sage",
		HostFamily: "example.edu", Outcome: "ui_changed", Jobs: 3,
		FirstSeen: first, LastSeen: first.Add(time.Hour),
	}
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		switch method {
		case "jobs.failures":
			*result.(*jobsFailuresResult) = jobsFailuresResult{}
		case "jobs.incidents":
			*result.(*jobsIncidentsResult) = jobsIncidentsResult{Incidents: []incident.Group{want}}
		default:
			t.Fatalf("method = %q", method)
		}
		return nil
	})
	root.SetArgs([]string{"--json", "jobs", "failures"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs failures: %v", err)
	}
	var page map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page["failures"] == nil || page["truncated"] == nil {
		t.Fatalf("page = %#v, want failures envelope", page)
	}
	var got []incident.Group
	if err := json.Unmarshal(page["failures"], &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Fingerprint != want.Fingerprint || got[0].Jobs != want.Jobs {
		t.Fatalf("incident rows = %#v, want %#v", got, []incident.Group{want})
	}
}
