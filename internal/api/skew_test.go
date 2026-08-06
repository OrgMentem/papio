// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
)

// attributedJob creates one job with a consumer recorded, through the production
// submit path so the attribution is stored the way a real submission stores it.
//
// acquire.submit_v3, not v2: ADR-0010 froze v2's params, so attribution rides the
// successor method.
func attributedJob(t *testing.T, router ipc.Router, requestID, doi, consumer string) string {
	t.Helper()
	params := map[string]any{
		"request": protocol.WorkRequest{
			SchemaVersion: protocol.WorkRequestSchemaVersion,
			RequestID:     requestID,
			Identifiers:   &protocol.Identifiers{DOI: doi},
		},
	}
	method := "acquire.submit_v2"
	if consumer != "" {
		params["consumer"] = consumer
		method = "acquire.submit_v3"
	}
	var submitted SubmitV2Result
	if rpcErr := callMethod(t, router, method, params, &submitted); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	return submitted.JobID
}

// These legacy types are a snapshot of the shipped ratified wire shape. They
// must only ever change alongside a deliberate, ADR-recorded contract change;
// using current production result types here would let producer and decoder
// widen together and make this skew test meaningless.
type legacyPolicy struct {
	AccessMode     string   `json:"access_mode"`
	DesiredVersion string   `json:"desired_version"`
	Resolver       string   `json:"resolver,omitempty"`
	MaxCostUSD     *float64 `json:"max_cost_usd,omitempty"`
	SourcesAllow   []string `json:"sources_allow,omitempty"`
	SourcesDeny    []string `json:"sources_deny,omitempty"`
	FetchMaxBytes  int64    `json:"fetch_max_bytes"`
	AutoImport     bool     `json:"auto_import,omitempty"`
	Collection     string   `json:"collection,omitempty"`
}

type legacyWork struct {
	DOI       string   `json:"doi,omitempty"`
	PMID      string   `json:"pmid,omitempty"`
	ArXiv     string   `json:"arxiv,omitempty"`
	ISBN      string   `json:"isbn,omitempty"`
	OpenAlex  string   `json:"openalex,omitempty"`
	Title     string   `json:"title,omitempty"`
	Authors   []string `json:"authors,omitempty"`
	Container string   `json:"container,omitempty"`
	Year      int      `json:"year,omitempty"`
}

type legacyJobRow struct {
	ID                  string       `json:"id"`
	WorkRequestID       string       `json:"work_request_id"`
	State               string       `json:"state"`
	Policy              legacyPolicy `json:"policy"`
	ArtifactSHA256      string       `json:"artifact_sha256,omitempty"`
	SelectedCandidateID int64        `json:"selected_candidate_id,omitempty"`
	SpentUSD            float64      `json:"spent_usd"`
	TerminalReason      string       `json:"terminal_reason,omitempty"`
	RetryAt             string       `json:"retry_at,omitempty"`
	CreatedAt           string       `json:"created_at"`
	UpdatedAt           string       `json:"updated_at"`
	Work                legacyWork   `json:"work"`
	ZotioItemKey        string       `json:"zotio_item_key,omitempty"`
}

type legacyHumanAction struct {
	ID               int64  `json:"id"`
	JobID            string `json:"job_id"`
	Kind             string `json:"kind"`
	Status           string `json:"status"`
	Detail           string `json:"detail,omitempty"`
	RequiresAuth     bool   `json:"requires_auth"`
	BlockedBy        string `json:"blocked_by,omitempty"`
	CreatedAt        string `json:"created_at"`
	CandidateID      int64  `json:"candidate_id,omitempty"`
	QuarantinePath   string `json:"quarantine_path,omitempty"`
	QuarantineSHA256 string `json:"quarantine_sha256,omitempty"`
	Revision         int64  `json:"revision"`
}

type legacyJobsPage struct {
	Jobs      []legacyJobRow `json:"jobs"`
	Truncated bool           `json:"truncated"`
}

type legacyJobDetail struct {
	Job     *legacyJobRow       `json:"job"`
	Events  []map[string]any    `json:"events"`
	Actions []legacyHumanAction `json:"actions"`
}

type legacyActionsPage struct {
	Actions   []legacyHumanAction `json:"actions"`
	Truncated bool                `json:"truncated"`
}

var legacyJobRowWireKeys = []string{
	"id", "work_request_id", "state", "policy", "spent_usd", "created_at", "updated_at", "work",
}

var legacyHumanActionWireKeys = []string{
	"id", "job_id", "kind", "status", "detail", "requires_auth", "blocked_by", "created_at", "revision",
}

func rawLegacyRows(t *testing.T, data []byte, envelopeKey string) []json.RawMessage {
	t.Helper()
	if envelopeKey == "" {
		var rows []json.RawMessage
		if err := json.Unmarshal(data, &rows); err != nil {
			t.Fatalf("decode ratified rows: %v", err)
		}
		return rows
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode ratified envelope: %v", err)
	}
	raw, ok := envelope[envelopeKey]
	if !ok {
		t.Fatalf("ratified envelope omitted %q", envelopeKey)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode ratified %s rows: %v", envelopeKey, err)
	}
	return rows
}

func assertLegacyRowKeys(t *testing.T, raw json.RawMessage, want []string) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode ratified row: %v", err)
	}
	assertRatifiedKeySet(t, object, want...)
}

func assertLegacyRows(t *testing.T, data []byte, envelopeKey string, want []string) {
	t.Helper()
	rows := rawLegacyRows(t, data, envelopeKey)
	if len(rows) == 0 {
		t.Fatalf("ratified %s response contained no rows", envelopeKey)
	}
	for _, raw := range rows {
		assertLegacyRowKeys(t, raw, want)
	}
}

func assertLegacyJobDetailRows(t *testing.T, data []byte) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode jobs.get envelope: %v", err)
	}
	assertRatifiedKeySet(t, envelope, "job", "events", "actions")
	if raw, ok := envelope["job"]; !ok {
		t.Fatal("jobs.get envelope omitted job")
	} else {
		assertLegacyRowKeys(t, raw, legacyJobRowWireKeys)
	}
	var actions []json.RawMessage
	if err := json.Unmarshal(envelope["actions"], &actions); err != nil {
		t.Fatalf("decode jobs.get actions: %v", err)
	}
	if len(actions) == 0 {
		t.Fatal("jobs.get response contained no actions")
	}
	for _, raw := range actions {
		assertLegacyRowKeys(t, raw, legacyHumanActionWireKeys)
	}
}

func assertLegacyActionEnvelope(t *testing.T, data []byte) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode actions.list_v2 envelope: %v", err)
	}
	assertRatifiedKeySet(t, envelope, "actions", "truncated")
}

func assertLegacyJobEnvelope(t *testing.T, data []byte) {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode jobs.list_v2 envelope: %v", err)
	}
	assertRatifiedKeySet(t, envelope, "jobs", "truncated")
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
		check  func(*testing.T, []byte)
	}{
		{"jobs.list", map[string]any{}, &[]legacyJobRow{},
			func(t *testing.T, data []byte) { assertLegacyRows(t, data, "", legacyJobRowWireKeys) }},
		{"jobs.list_v2", map[string]any{"limit": 10}, &legacyJobsPage{},
			func(t *testing.T, data []byte) {
				assertLegacyJobEnvelope(t, data)
				assertLegacyRows(t, data, "jobs", legacyJobRowWireKeys)
			}},
		{"jobs.get", map[string]string{"job_id": jobID}, &legacyJobDetail{},
			func(t *testing.T, data []byte) { assertLegacyJobDetailRows(t, data) }},
		{"actions.list", map[string]any{"open_only": true}, &[]legacyHumanAction{},
			func(t *testing.T, data []byte) { assertLegacyRows(t, data, "", legacyHumanActionWireKeys) }},
		{"actions.list_v2", map[string]any{"open_only": true, "limit": 10}, &legacyActionsPage{},
			func(t *testing.T, data []byte) {
				assertLegacyActionEnvelope(t, data)
				assertLegacyRows(t, data, "actions", legacyHumanActionWireKeys)
			}},
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
			tc.check(t, data)
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
// param-side contract: `consumer` is a sibling of `request` on submit_v3, so a
// daemon predating it refuses the call rather than dropping the attribution.
// This asserts the current daemon accepts it and that a misspelling is refused —
// the property that makes silent attribution loss impossible.
func TestSubmitConsumerIsRejectedRatherThanIgnoredByAnOlderShape(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     "wr_skew_param",
		Identifiers:   &protocol.Identifiers{DOI: "10.1000/skew-param"},
	}
	var submitted SubmitV2Result
	if rpcErr := callMethod(t, router, "acquire.submit_v3",
		map[string]any{"request": request, "consumer": "inscribi"}, &submitted); rpcErr != nil {
		t.Fatal(rpcErr)
	}
	recorded, ok, err := system.Jobs.Consumer(context.Background(), submitted.JobID)
	if err != nil || !ok || recorded != "inscribi" {
		t.Fatalf("recorded consumer = %q ok=%t err=%v, want inscribi", recorded, ok, err)
	}

	request.RequestID = "wr_skew_param_typo"
	request.Identifiers = &protocol.Identifiers{DOI: "10.1000/skew-param-typo"}
	rpcErr := callMethod(t, router, "acquire.submit_v3",
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
	if rpcErr := callMethod(t, router, "acquire.submit_v3",
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

// TestHandoffOpenAuditTrailNamesTheConsumer is the evidence ADR-0014 Decision 6
// substitutes for a gate. Autonomous drain is unratified but deliberately not
// enforced in code — a gate is theatre, since a script passes any flag a human
// passes, and papio's principle is that an agent driving the CLI gets exactly
// what a human gets. That trade is only honest if the prohibition is auditable,
// so "consumer X opened N human actions in M minutes" has to be answerable from
// the event stream.
//
// It exercises recordHandoffOpened directly: system.Browser is a concrete
// *browser.Bridge, nil under test, so openActions returns before reaching the
// audit write. openActions calls this helper only after FocusHandoffs succeeds —
// papio does not claim to have opened a tab it failed to open.
func TestHandoffOpenAuditTrailNamesTheConsumer(t *testing.T) {
	system := testSystem(t)
	router := Router(system)
	ctx := context.Background()
	attributed := attributedJob(t, router, "wr_audit_owned", "10.1000/audit-owned", "inscribi:project:psyc101")
	bare := attributedJob(t, router, "wr_audit_bare", "10.1000/audit-bare", "")

	recordHandoffOpened(ctx, system, []string{attributed, bare})

	events, err := system.Jobs.Events(ctx, attributed)
	if err != nil {
		t.Fatal(err)
	}
	var opened map[string]any
	for _, event := range events {
		if event["kind"] == handoffOpenedEvent {
			opened = event
		}
	}
	if opened == nil {
		t.Fatalf("no %s event recorded; the drain prohibition would be neither enforced nor observable", handoffOpenedEvent)
	}
	detail, ok := opened["detail"].(map[string]any)
	if !ok {
		t.Fatalf("event carries no detail: %+v", opened)
	}
	if detail["consumer"] != "inscribi:project:psyc101" {
		t.Fatalf("detail = %+v, want the owning consumer named", detail)
	}
	// batch_size is what separates one deliberate selector call from a loop of
	// them; without it a drain shows up as N events indistinguishable from one
	// operator opening N rows at once.
	if fmt.Sprint(detail["batch_size"]) != "2" {
		t.Fatalf("detail = %+v, want batch_size 2", detail)
	}
	if detail["principal"] == "" || detail["principal"] == nil {
		t.Fatalf("detail = %+v, want the transport principal recorded", detail)
	}

	// An unattributed job still gets an audit line: the absence of a consumer is
	// not a reason to lose the fact that a handoff was opened.
	bareEvents, err := system.Jobs.Events(ctx, bare)
	if err != nil {
		t.Fatal(err)
	}
	var bareOpened map[string]any
	for _, event := range bareEvents {
		if event["kind"] == handoffOpenedEvent {
			bareOpened = event
		}
	}
	if bareOpened == nil {
		t.Fatal("unattributed job recorded no handoff.opened event")
	}
	bareDetail := bareOpened["detail"].(map[string]any)
	if _, present := bareDetail["consumer"]; present {
		t.Fatalf("unattributed job claims a consumer: %+v", bareDetail)
	}
}
