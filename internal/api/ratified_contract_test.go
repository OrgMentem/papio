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
// consumer by ADR-0009, extended with acquire.submit_v2 by ADR-0010. Removing
// or renaming one breaks that consumer, so this list is deliberately pinned to
// the live router rather than documentation.
var ratifiedConsumerMethods = []string{
	"acquire.submit_v2",
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

	// acquire.submit_v2 is the only ratified method that CREATES durable state,
	// so its params are pinned as tightly as its result. The six prior methods
	// take small parameter objects (job_id, limit, state, job_ids, path/role);
	// this one embeds a whole work-request/1 document, and internal/ipc decodes
	// params with DisallowUnknownFields — so a newer consumer sending a field an
	// older daemon lacks has its entire call rejected, not just the field. Only
	// the identity subset below plus access_mode_override is promised; the
	// policy fields stay served but unratified so a consumer cannot pin papio's
	// policy vocabulary.
	//
	// Every frozen value below is written as a WIRE LITERAL rather than read
	// from a production constant. A pin that derives its expectation from the
	// implementation stays green when the implementation is renamed in step
	// with it, which is exactly the drift this suite exists to catch.
	t.Run("submit_v2 accepts every ratified identity key", func(t *testing.T) {
		for _, identifiers := range []map[string]any{
			{"doi": "10.1000/ratified-submit"},
			{"pmid": "31234567"},
			{"arxiv": "2301.08745"},
			{"isbn": "9780306406157"},
			{"openalex": "W2741809807"},
		} {
			for key := range identifiers {
				t.Run(key, func(t *testing.T) {
					system := testSystem(t)
					var result map[string]json.RawMessage
					rpcErr := callMethod(t, Router(system), "acquire.submit_v2", map[string]any{
						"request": map[string]any{
							"schema_version": "work-request/1",
							"request_id":     "wr_ratified_" + key,
							"identifiers":    identifiers,
						},
					}, &result)
					if rpcErr != nil {
						t.Fatalf("acquire.submit_v2 with identifiers.%s = %+v, want accepted", key, rpcErr)
					}
					assertRatifiedKeySet(t, result, "job_id", "existing")
				})
			}
		}
	})

	t.Run("submit_v2 accepts the ratified request shape", func(t *testing.T) {
		for _, mode := range []string{"conservative", "assisted", "delegated"} {
			t.Run(mode, func(t *testing.T) {
				system := testSystem(t)
				var result map[string]json.RawMessage
				rpcErr := callMethod(t, Router(system), "acquire.submit_v2", map[string]any{
					"request": map[string]any{
						"schema_version":       "work-request/1",
						"request_id":           "wr_ratified_" + mode,
						"identifiers":          map[string]any{"doi": "10.1000/ratified-" + mode},
						"title":                "A ratified submission",
						"authors":              []string{"A. Author"},
						"year":                 2026,
						"access_mode_override": mode,
					},
					"auto_import": false,
					"force":       false,
				}, &result)
				if rpcErr != nil {
					t.Fatalf("acquire.submit_v2 with access_mode_override %q = %+v, want accepted", mode, rpcErr)
				}
				assertRatifiedKeySet(t, result, "job_id", "existing")

				// `existing` carries no omitempty: a consumer distinguishing
				// "queued" from "a live job already owns this work" must never
				// have to treat an absent key as false.
				var existing bool
				if err := json.Unmarshal(result["existing"], &existing); err != nil {
					t.Fatalf("decode existing: %v", err)
				}
				if existing {
					t.Fatal("first submission reported existing = true")
				}
			})
		}
	})

	t.Run("submit_v2 rejects an unknown param", func(t *testing.T) {
		// Fail-closed params are half the contract: a typo in a consumer's
		// payload must be an error, never a silently ignored field.
		router := RouterWithShutdown(nil, func() {})
		rpcErr := callMethod(t, router, "acquire.submit_v2", map[string]any{
			"request":     map[string]any{"schema_version": "work-request/1", "request_id": "wr_ratified_bad"},
			"idempotency": "not-a-papio-concept",
		}, nil)
		if rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("unknown param = %+v, want invalid_argument", rpcErr)
		}
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
