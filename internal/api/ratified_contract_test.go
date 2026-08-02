// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"papio/internal/artifact"
	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/work"
)

// ratifiedConsumerMethods is the IPC surface promised to the first external
// consumer by ADR-0009, extended with acquire.submit_v2 by ADR-0010 and with
// the two collection readers by ADR-0011. Removing or renaming one breaks that
// consumer, so this list is deliberately pinned to the live router rather than
// documentation.
var ratifiedConsumerMethods = []string{
	"acquire.submit_v2",
	"jobs.list_v2",
	"actions.list_v2",
	"actions.open",
	"jobs.receipt",
	"jobs.add_component",
	"jobs.repair_awaiting_human",
	"bundle.document",
	"artifacts.locate",
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
			if _, err := system.Jobs.OpenHumanAction(ctx, id, "openurl_handoff", "handoff", job.Access(false, "")); err != nil {
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

	// The collection readers are pure reads. bundle.export_v2 and artifacts.get
	// were deliberately NOT ratified in their place: export_v2 requires
	// output_dir and materialises a directory, and artifacts.get returns the
	// job.Artifact persistence struct including identity_result, which is
	// last-writer-wins across every job sharing a digest and which ADR-0007
	// forbids projecting from an artifact.
	t.Run("collection readers take job_id and reject unknown params", func(t *testing.T) {
		for _, method := range []string{"bundle.document", "artifacts.locate"} {
			t.Run(method, func(t *testing.T) {
				router := RouterWithShutdown(nil, func() {})
				if rpcErr := callMethod(t, router, method, map[string]any{}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
					t.Fatalf("missing job_id = %+v, want invalid_argument", rpcErr)
				}
				rpcErr := callMethod(t, router, method, map[string]any{
					"job_id": "job_ratified_reader", "output_dir": "/tmp/nope",
				}, nil)
				if rpcErr == nil || rpcErr.Code != "invalid_argument" {
					t.Fatalf("unknown param = %+v, want invalid_argument", rpcErr)
				}
			})
		}
	})

	t.Run("bundle.document returns the document as text and writes nothing", func(t *testing.T) {
		system, jobID, _ := readyBundleSystem(t)
		bundles := filepath.Join(system.Config.DataDir, "bundles")
		var result map[string]json.RawMessage
		if rpcErr := callMethod(t, Router(system), "bundle.document",
			map[string]string{"job_id": jobID}, &result); rpcErr != nil {
			t.Fatalf("bundle.document = %+v", rpcErr)
		}
		assertRatifiedKeySet(t, result, "schema_version", "document")

		// Text, not an object: the whole reason this method exists rather than
		// bundle.export_v2 is that a nested bundle body would freeze the
		// document's shape into the RPC contract forever.
		var document string
		if err := json.Unmarshal(result["document"], &document); err != nil {
			t.Fatalf("document is not a JSON string: %v", err)
		}
		decoded, err := protocol.DecodeAcquisitionBundle([]byte(document))
		if err != nil {
			t.Fatalf("document does not decode as a bundle: %v", err)
		}
		var advertised string
		if err := json.Unmarshal(result["schema_version"], &advertised); err != nil {
			t.Fatal(err)
		}
		if advertised != decoded.SchemaVersion {
			t.Fatalf("schema_version %q disagrees with the document's %q", advertised, decoded.SchemaVersion)
		}
		if _, err := os.Stat(bundles); !os.IsNotExist(err) {
			t.Fatalf("a ratified reader materialised %s (stat err = %v)", bundles, err)
		}
	})

	t.Run("artifacts.locate omits the fields ADR-0007 forbids projecting", func(t *testing.T) {
		system, jobID, want := readyBundleSystem(t)
		var keys map[string]json.RawMessage
		if rpcErr := callMethod(t, Router(system), "artifacts.locate",
			map[string]string{"job_id": jobID}, &keys); rpcErr != nil {
			t.Fatalf("artifacts.locate = %+v", rpcErr)
		}
		assertRatifiedKeySet(t, keys, "sha256", "size_bytes", "mime", "path")

		// Key presence alone would pass a handler that swapped, zeroed, or
		// retyped every value, so the values are compared to the artifact the
		// fixture actually promoted.
		var got ArtifactLocation
		if rpcErr := callMethod(t, Router(system), "artifacts.locate",
			map[string]string{"job_id": jobID}, &got); rpcErr != nil {
			t.Fatalf("artifacts.locate = %+v", rpcErr)
		}
		if got != want {
			t.Fatalf("location = %+v, want %+v", got, want)
		}
	})
}

// readyBundleSystem builds a system holding one job whose bundle can actually
// be produced: an accepted candidate, a promoted artifact that verifies, and a
// passing acquisition identity. The ratified readers are frozen forever, so
// their contract test asserts against a real response rather than a stub.
func readyBundleSystem(t *testing.T) (*bootstrap.System, string, ArtifactLocation) {
	t.Helper()
	ctx := context.Background()
	system := testSystem(t)
	id, err := system.Jobs.CreateRequest(ctx, "wr_ratified_reader",
		work.Work{DOI: "10.1000/ratified-reader", Title: "A Ratified Reader", Authors: []string{"Ada Lovelace"}, Year: 2026},
		"", "", job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20},
		nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := system.Jobs.InsertCandidates(ctx, id, []job.Candidate{{
		JobID: id, Source: "unpaywall", URLRedacted: "https://example.test/paper.pdf", URLKey: "ratified-url-key",
		LandingRedacted: "https://example.test/article", Version: "published", AccessBasis: "open_access",
		ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1, Rank: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, _ := system.Jobs.NextPendingCandidate(ctx, id)
	if candidate == nil {
		t.Fatal("candidate missing")
	}
	if err := system.Jobs.MarkCandidate(ctx, candidate.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	quarantine, err := system.Artifacts.QuarantineDir(id)
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(quarantine, "fixture.tmp")
	if err := os.WriteFile(temp, []byte("%PDF-1.4\nfixture\n%%EOF"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, size, err := artifact.HashFile(temp)
	if err != nil {
		t.Fatal(err)
	}
	path, err := system.Artifacts.Promote(temp, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.UpsertArtifact(ctx, job.Artifact{
		SHA256: sha, SizeBytes: size, MIME: "application/pdf", PageCount: 1,
		Path: path, IdentityResult: "pass", CreatedAt: "2026-08-01T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateReady, nil,
		job.WithArtifact(sha), job.WithCandidate(candidate.ID)); err != nil {
		t.Fatal(err)
	}
	return system, id, ArtifactLocation{SHA256: sha, SizeBytes: size, MIME: "application/pdf", Path: path}
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
