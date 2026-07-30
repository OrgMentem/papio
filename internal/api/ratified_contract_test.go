// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/work"
)

// ratifiedConsumerMethods is the IPC surface promised to the first external
// consumer by ADR-0009. Removing or renaming one breaks that consumer, so this
// list is deliberately pinned to the live router rather than documentation.
var ratifiedConsumerMethods = []string{
	"jobs.list_v2",
	"actions.list_v2",
	"actions.open",
	"jobs.receipt",
	"jobs.add_component",
	"jobs.repair_awaiting_human",
}

func TestRatifiedConsumerContract(t *testing.T) {
	t.Run("methods are served", func(t *testing.T) {
		// Router construction only closes over system; these handlers dereference
		// it when invoked. Include shutdown so this is the daemon's complete set.
		served := RouterWithShutdown(nil, func() {}).Methods
		for _, method := range ratifiedConsumerMethods {
			if _, ok := served[method]; !ok {
				t.Errorf("ratified method %q is not served by the live router; ADR-0009 forbids removing it", method)
			}
		}
	})

	t.Run("list envelopes prove truncation", func(t *testing.T) {
		system := testSystem(t)
		router := Router(system)
		ctx := context.Background()
		for i := range 3 {
			id, err := system.Jobs.CreateRequest(ctx, fmt.Sprintf("ratified-page-%02d", i),
				work.Work{DOI: fmt.Sprintf("10.1000/ratified-page-%02d", i)}, "", "",
				job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20},
				nil, job.PrincipalCLI)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := system.Jobs.OpenHumanAction(ctx, id, "openurl_handoff", "handoff"); err != nil {
				t.Fatal(err)
			}
		}

		for _, tc := range []struct {
			name      string
			limit     int
			wantRows  int
			truncated bool
		}{
			{name: "more rows than limit", limit: 2, wantRows: 2, truncated: true},
			{name: "fewer rows than limit", limit: 4, wantRows: 3, truncated: false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				assertRatifiedPage(t, router, "jobs.list_v2", "jobs", tc.limit, tc.wantRows, tc.truncated)
				assertRatifiedPage(t, router, "actions.list_v2", "actions", tc.limit, tc.wantRows, tc.truncated)
			})
		}
	})

	t.Run("actions open accepts job_ids", func(t *testing.T) {
		// DecodeParams runs before the nil-system no-op, so this exercises the
		// strict wire decoder without opening a browser session.
		router := RouterWithShutdown(nil, func() {})
		if rpcErr := callMethod(t, router, "actions.open", map[string]any{"job_ids": []string{"job_ratified"}}, nil); rpcErr != nil {
			t.Fatalf("actions.open with job_ids = %+v, want accepted params", rpcErr)
		}
	})

	t.Run("component envelope", func(t *testing.T) {
		system := testSystem(t)
		system.App.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
			return pdf.ValidationReport{
				Payload:    pdf.PayloadReport{OK: true},
				Structural: pdf.StructuralReport{Valid: true},
			}, nil
		}
		ctx := context.Background()
		id, err := system.Jobs.CreateRequest(ctx, "ratified-component", work.Work{DOI: "10.1000/ratified-component"}, "", "",
			job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalCLI)
		if err != nil {
			t.Fatal(err)
		}
		const mainSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if err := system.Jobs.UpsertArtifact(ctx, job.Artifact{SHA256: mainSHA, SizeBytes: 1, MIME: "application/pdf", Path: filepath.Join(t.TempDir(), "main.pdf"), IdentityResult: "pass"}); err != nil {
			t.Fatal(err)
		}
		if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
			t.Fatal(err)
		}
		if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil, job.WithArtifact(mainSHA)); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(system.App.Config.EffectiveAdoptionRoot(), id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "supplement.pdf")
		if err := os.WriteFile(path, []byte("fixture supplement"), 0o600); err != nil {
			t.Fatal(err)
		}

		var result map[string]json.RawMessage
		if rpcErr := callMethod(t, Router(system), "jobs.add_component", map[string]string{
			"job_id": id, "path": path, "role": job.ComponentSupplement,
		}, &result); rpcErr != nil {
			t.Fatal(rpcErr)
		}
		assertRatifiedEnvelope(t, result, "components", false)
	})

	t.Run("receipt JSON keys", func(t *testing.T) {
		data, err := json.Marshal(Receipt{
			JobID:           "job_ratified",
			RequestID:       "request_ratified",
			State:           job.StateUnavailable,
			Terminal:        true,
			TerminalReason:  string(job.TerminalReasonNoLegalCandidates),
			Principal:       string(job.PrincipalCLI),
			AttemptedTiers:  []string{"open_access"},
			Components:      []job.Component{{Role: job.ComponentMain, SHA256: "sha", CreatedAt: "now"}},
			BundleAvailable: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		assertRatifiedKeySet(t, object,
			"job_id", "request_id", "state", "terminal", "terminal_reason", "principal",
			"attempted_tiers", "components", "bundle_available")
	})
}

func assertRatifiedPage(t *testing.T, router ipc.Router, method, rowsKey string, limit, wantRows int, wantTruncated bool) {
	t.Helper()
	var object map[string]json.RawMessage
	if rpcErr := callMethod(t, router, method, map[string]int{"limit": limit}, &object); rpcErr != nil {
		t.Fatalf("%s limit %d = %+v", method, limit, rpcErr)
	}
	if got := assertRatifiedEnvelope(t, object, rowsKey, wantTruncated); got != wantRows {
		t.Fatalf("%s limit %d returned %d %s rows, want %d", method, limit, got, rowsKey, wantRows)
	}
}

func assertRatifiedEnvelope(t *testing.T, object map[string]json.RawMessage, rowsKey string, wantTruncated bool) int {
	t.Helper()
	rows, ok := object[rowsKey]
	if !ok {
		t.Fatalf("response omitted documented %q row key: %s", rowsKey, object)
	}
	var decodedRows []json.RawMessage
	if err := json.Unmarshal(rows, &decodedRows); err != nil {
		t.Fatalf("decode %q rows: %v", rowsKey, err)
	}
	rawTruncated, ok := object["truncated"]
	if !ok {
		t.Fatalf("response omitted documented %q key: %s", "truncated", object)
	}
	var truncated bool
	if err := json.Unmarshal(rawTruncated, &truncated); err != nil {
		t.Fatalf("decode truncated boolean: %v", err)
	}
	if truncated != wantTruncated {
		t.Fatalf("truncated = %t, want %t", truncated, wantTruncated)
	}
	return len(decodedRows)
}

func assertRatifiedKeySet(t *testing.T, object map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(object) != len(want) {
		t.Fatalf("JSON key count = %d (%v), want %d (%v)", len(object), object, len(want), want)
	}
	for _, key := range want {
		if _, ok := object[key]; !ok {
			t.Errorf("JSON omitted documented key %q", key)
		}
	}
}
