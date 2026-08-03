// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"testing"

	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

// attributedJob creates one job with a consumer recorded, through the production
// submit path so the attribution is stored the way a real submission stores it.
func attributedJob(t *testing.T, router ipc.Router, requestID, doi, consumer string) string {
	t.Helper()
	params := map[string]any{
		"request": protocol.WorkRequest{
			SchemaVersion: protocol.WorkRequestSchemaVersion,
			RequestID:     requestID,
			Identifiers:   &protocol.Identifiers{DOI: doi},
		},
	}
	if consumer != "" {
		params["consumer"] = consumer
	}
	var submitted SubmitV2Result
	if rpcErr := callMethod(t, router, "acquire.submit_v2", params, &submitted); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	return submitted.JobID
}

// TestAttributionDoesNotWidenTheRatifiedListings is the fail-closed guarantee.
//
// internal/ipc decodes results with DisallowUnknownFields, recursively, and one
// binary is CLI, daemon, and native host — so an already-installed papio must
// still decode this daemon's answers. Consumer attribution and staleness
// therefore live on NEW methods, and the ratified ones must remain byte-shaped
// exactly as they were: this test decodes their results into the types an older
// CLI carries and fails if a single new key leaked into them.
func TestAttributionDoesNotWidenTheRatifiedListings(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	jobID := attributedJob(t, router, "wr_skew_ratified", "10.1000/skew-ratified", "inscribi")
	ctx := context.Background()
	if _, err := system.Jobs.OpenHumanAction(ctx, jobID, "openurl_handoff", "sign in", job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		method string
		params any
		into   any
	}{
		{"jobs.list", map[string]any{}, &[]job.Row{}},
		{"jobs.list_v2", map[string]any{"limit": 10}, &JobsPage{}},
		{"jobs.get", map[string]string{"job_id": jobID}, &JobDetail{}},
		{"actions.list", map[string]any{"open_only": true}, &[]job.HumanAction{}},
		{"actions.list_v2", map[string]any{"open_only": true, "limit": 10}, &ActionsPage{}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			raw, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatal(err)
			}
			data, rpcErr := router.Handle(context.Background(), ipc.Request{Method: tc.method, Params: raw})
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			// DecodeResult, not json.Unmarshal: this is the strict decoder an
			// older CLI actually uses, and the only one that catches a widened
			// result.
			if err := ipc.DecodeResult(data, tc.into); err != nil {
				t.Fatalf("an older papio can no longer decode %s: %v\npayload: %s", tc.method, err, data)
			}
		})
	}
}

// TestNewMethodResultsDecodeStrictlyIntoTheirCLITypes walks the other direction:
// every new method's payload must satisfy the same fail-closed decoder the CLI
// applies, or the feature works in tests that unmarshal loosely and fails
// against a real socket.
func TestNewMethodResultsDecodeStrictlyIntoTheirCLITypes(t *testing.T) {
	system := testSystem(t)
	system.Config.Actions = config.Actions{StaleAfterSeconds: 1}
	router := Router(system)
	jobID := attributedJob(t, router, "wr_skew_new", "10.1000/skew-new", "inscribi")
	ctx := context.Background()
	if _, err := system.Jobs.OpenHumanAction(ctx, jobID, "openurl_handoff", "sign in", job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.RecordValidationReport(ctx, job.ValidationRecord{
		JobID: jobID, CandidateID: 1, SHA256: "abc", Outcome: "pass",
		Document: `{"schema_version":"validation-report/1","identity":{"result":"pass"}}`,
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		method string
		params any
		into   any
	}{
		{"jobs.list_v3", map[string]any{"limit": 10}, &JobsPageV3{}},
		{"jobs.list_v3", map[string]any{"limit": 10, "consumer": "inscribi"}, &JobsPageV3{}},
		{"jobs.get_v2", map[string]string{"job_id": jobID}, &JobDetailV2{}},
		{"actions.list_v3", map[string]any{"open_only": true, "limit": 10}, &ActionsPageV3{}},
		{"actions.list_v3", map[string]any{"open_only": true, "limit": 10, "consumer": "inscribi"}, &ActionsPageV3{}},
		{"artifacts.validation", map[string]string{"job_id": jobID}, &ValidationResult{}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			raw, err := json.Marshal(tc.params)
			if err != nil {
				t.Fatal(err)
			}
			data, rpcErr := router.Handle(context.Background(), ipc.Request{Method: tc.method, Params: raw})
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			if err := ipc.DecodeResult(data, tc.into); err != nil {
				t.Fatalf("%s result does not decode strictly: %v\npayload: %s", tc.method, err, data)
			}
		})
	}
}

// TestSubmitConsumerIsRejectedRatherThanIgnoredByAnOlderShape documents the
// param-side contract: `consumer` is a sibling of `request`, so a daemon that
// predates it refuses the call. This asserts the current daemon accepts it and
// that a misspelling is refused — the property that makes silent attribution
// loss impossible.
func TestSubmitConsumerIsRejectedRatherThanIgnoredByAnOlderShape(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "wr_skew_param",
		Identifiers:   &protocol.Identifiers{DOI: "10.1000/skew-param"},
	}
	var submitted SubmitV2Result
	if rpcErr := callMethod(t, router, "acquire.submit_v2",
		map[string]any{"request": request, "consumer": "inscribi"}, &submitted); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	recorded, ok, err := system.Jobs.Consumer(context.Background(), submitted.JobID)
	if err != nil || !ok || recorded != "inscribi" {
		t.Fatalf("recorded consumer = %q ok=%t err=%v, want inscribi", recorded, ok, err)
	}

	request.RequestID = "wr_skew_param_typo"
	request.Identifiers = &protocol.Identifiers{DOI: "10.1000/skew-param-typo"}
	rpcErr := callMethod(t, router, "acquire.submit_v2",
		map[string]any{"request": request, "consumers": "inscribi"}, nil)
	if rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("misspelled param error = %v, want invalid_argument: a dropped consumer must never be silent", rpcErr)
	}
}

// TestReusedLiveJobKeepsItsOriginalConsumer: convergence on a live job must not
// reassign the work to whoever submitted second. Attribution answers "who
// created this acquisition", and the second caller did not.
func TestReusedLiveJobKeepsItsOriginalConsumer(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	const doi = "10.1000/skew-reused"
	first := attributedJob(t, router, "wr_skew_reused_first", doi, "instructor-a")

	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "wr_skew_reused_second",
		Identifiers:   &protocol.Identifiers{DOI: doi},
	}
	var second SubmitV2Result
	if rpcErr := callMethod(t, router, "acquire.submit_v2",
		map[string]any{"request": request, "consumer": "instructor-b"}, &second); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if !second.Existing || second.JobID != first {
		t.Fatalf("second submission = %+v, want the existing job %s", second, first)
	}
	recorded, _, err := system.Jobs.Consumer(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if recorded != "instructor-a" {
		t.Fatalf("consumer after convergence = %q, want the original instructor-a", recorded)
	}
}

// TestUnattributedJobOmitsTheConsumerKey keeps absence honest all the way to the
// wire: no attribution means no key, never an empty string a consumer could
// mistake for a name.
func TestUnattributedJobOmitsTheConsumerKey(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	if _, err := system.Jobs.CreateRequest(context.Background(), "wr_skew_bare",
		work.Work{DOI: "10.1000/skew-bare"}, "", "",
		job.Policy{AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20},
		nil, job.PrincipalCLI); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Jobs []map[string]json.RawMessage `json:"jobs"`
	}
	if rpcErr := callMethod(t, router, "jobs.list_v3", map[string]any{"limit": 10}, &page); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("rows = %d, want 1", len(page.Jobs))
	}
	if raw, ok := page.Jobs[0]["consumer"]; ok {
		t.Fatalf("unattributed row carries a consumer key (%s); absence must be absent", raw)
	}
	if _, ok := page.Jobs[0]["id"]; !ok {
		t.Fatalf("row lost its ratified keys: %v", page.Jobs[0])
	}
}
