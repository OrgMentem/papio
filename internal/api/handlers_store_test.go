// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/watch"
	"papio/internal/work"
)

// storeHandlerUnavailableJob parks one job in the terminal `unavailable`
// state. That is the state every store-backed failure read model selects on,
// so one fixture serves jobs.failures, jobs.incidents, failures.list_v1 and
// jobs.diagnose_v1/v2. detail rides the decisive resolving -> unavailable
// transition, which is the event those read models classify from.
func storeHandlerUnavailableJob(t *testing.T, system *bootstrap.System, requestID string, reason job.TerminalReason, detail map[string]any) string {
	t.Helper()
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, requestID, work.Work{
		Title: "Store handler " + requestID, DOI: "10.1000/" + requestID,
	}, "", "", job.Policy{
		AccessMode:     config.ModeConservative,
		DesiredVersion: "any",
		FetchMaxBytes:  1 << 20,
	}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateUnavailable, detail,
		job.WithTerminalReason(reason)); err != nil {
		t.Fatal(err)
	}
	return id
}

// storeHandlerHandoffJob parks one job on an open openurl_handoff action. That
// is the exact authorization shape AcquireEffectPermit fences on, so it is the
// only way to reach a live effect permit through the public job API.
func storeHandlerHandoffJob(t *testing.T, system *bootstrap.System, requestID string) string {
	t.Helper()
	ctx := context.Background()
	id, err := system.Jobs.CreateRequest(ctx, requestID, work.Work{
		Title: "Store handler " + requestID, DOI: "10.1000/" + requestID,
	}, "", "", job.Policy{
		AccessMode:     config.ModeConservative,
		DesiredVersion: "any",
		FetchMaxBytes:  1 << 20,
	}, nil, job.PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateQueued, job.StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := system.Jobs.Transition(ctx, id, job.StateResolving, job.StateAwaitingHuman, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Jobs.OpenHumanAction(ctx, id, "openurl_handoff",
		"institutional handoff for the effect permit fixture", job.Access(true, "paywall")); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStoreHandlerJobsFailuresGroupsTerminalJobs(t *testing.T) {
	system := testSystem(t)
	id := storeHandlerUnavailableJob(t, system, "wr_store_failures", job.TerminalReasonCandidatesExhausted, nil)
	router := Router(system)

	var result failuresResult
	if rpcErr := callMethod(t, router, "jobs.failures", map[string]any{"since": "24h", "limit": 10}, &result); rpcErr != nil {
		t.Fatalf("jobs.failures = %+v", rpcErr)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("failures = %+v, want exactly one group", result.Failures)
	}
	group := result.Failures[0]
	if group.State != job.StateUnavailable || group.Count != 1 || group.Sample != id {
		t.Fatalf("group = %+v, want unavailable/count 1/sample %s", group, id)
	}
	// The job carries no candidate, so the group reports the terminal reason
	// rather than a provider host.
	if group.Reason != string(job.TerminalReasonCandidatesExhausted) || group.Provider != "-" {
		t.Fatalf("group = %+v, want reason %q and provider -", group, job.TerminalReasonCandidatesExhausted)
	}

	// A window that closes before the job was updated must exclude it: this
	// proves `since` reached the query rather than being parsed and dropped.
	var empty failuresResult
	if rpcErr := callMethod(t, router, "jobs.failures", map[string]any{
		"since": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	}, &empty); rpcErr != nil {
		t.Fatalf("future since = %+v", rpcErr)
	}
	if len(empty.Failures) != 0 {
		t.Fatalf("failures inside a future window = %+v, want none", empty.Failures)
	}
}

func TestStoreHandlerJobsFailuresRejectsBadParams(t *testing.T) {
	router := Router(testSystem(t))
	for _, params := range []map[string]any{
		{"limit": -1},
		{"since": "not-a-duration"},
		{"since": "-3h"},
		{"limit": "ten"},
		{"window": "24h"},
	} {
		if rpcErr := callMethod(t, router, "jobs.failures", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("jobs.failures %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}

// TestStoreHandlerJobsIncidentsReturnsMatchingEventDerivedGroup drives a
// matching terminal job and its decisive provider outcome through the RPC
// handler. The handler must return the aggregate that comes from the event.
func TestStoreHandlerJobsIncidentsReturnsMatchingEventDerivedGroup(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	id := storeHandlerUnavailableJob(t, system, "wr_store_incidents", job.TerminalReasonNoEntitlement, nil)
	if err := system.Store.AppendEvent(ctx, id, "browser.provider_outcome", map[string]any{
		"outcome": "no_entitlement", "host": "provider.example.com", "safety_domain": "provider.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	events, err := system.Jobs.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	event := events[len(events)-1]
	eventAt, err := time.Parse(time.RFC3339Nano, event["at"].(string))
	if err != nil {
		t.Fatal(err)
	}

	var result incidentsResult
	rpcErrCh := make(chan *ipc.RPCError, 1)
	go func() {
		rpcErrCh <- callMethod(t, Router(system), "jobs.incidents", map[string]any{
			"since": "24h", "limit": 10,
		}, &result)
	}()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case rpcErr := <-rpcErrCh:
		if rpcErr != nil {
			t.Fatalf("jobs.incidents = %+v", rpcErr)
		}
	case <-timer.C:
		t.Fatal("jobs.incidents did not return within one second; possible nested cursor deadlock")
	}

	if len(result.Incidents) != 1 {
		t.Fatalf("incidents = %+v, want exactly one event-derived group", result.Incidents)
	}
	group := result.Incidents[0]
	if group.Fingerprint == "" || group.SafetyDomain != "provider.example.com" || group.HostFamily != "example.com" || group.Outcome != "no_entitlement" || group.Jobs != 1 {
		t.Fatalf("incident = %+v, want provider.example.com/example.com/no_entitlement for one job", group)
	}
	if !group.FirstSeen.Equal(eventAt) || !group.LastSeen.Equal(eventAt) {
		t.Fatalf("incident time range = %s to %s, want %s", group.FirstSeen, group.LastSeen, eventAt)
	}
}

func TestStoreHandlerJobsIncidentsRejectsBadParams(t *testing.T) {
	router := Router(testSystem(t))
	for _, params := range []map[string]any{
		{"limit": -1},
		{"since": "yesterday"},
		{"since": "-1h"},
		{"limit": []int{1}},
		{"scope": "all"},
	} {
		if rpcErr := callMethod(t, router, "jobs.incidents", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("jobs.incidents %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}

func TestStoreHandlerWatchDigestReturnsRecordedEntries(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	watched, err := system.Watches.Create(ctx, watch.CreateInput{
		Query: "store handler digest", Collection: "Reading", CadenceHours: 24, PerRunCap: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := system.Watches.RecordDigest(ctx, watched.ID, time.Now(), []watch.DigestEntry{{
		WorkKey: "10.1000/store-digest", Title: "Store digest", DOI: "10.1000/store-digest",
	}}); err != nil {
		t.Fatal(err)
	}
	router := Router(system)

	var result WatchDigestResult
	if rpcErr := callMethod(t, router, "watch.digest", map[string]any{"id": watched.ID, "limit": 10}, &result); rpcErr != nil {
		t.Fatalf("watch.digest = %+v", rpcErr)
	}
	if result.WatchID != watched.ID || len(result.Entries) != 1 || result.Entries[0].WorkKey != "10.1000/store-digest" {
		t.Fatalf("digest = %+v", result)
	}

	// A watch id with no row is not_found, never internal: watch.Store.Digest
	// loads the watch first and reports sql.ErrNoRows, and the shared failure
	// helper is what turns that into the code callers branch on.
	rpcErr := callMethod(t, router, "watch.digest", map[string]any{"id": watched.ID + 4242}, nil)
	if rpcErr == nil || rpcErr.Code != "not_found" {
		t.Fatalf("unknown watch digest = %+v, want not_found", rpcErr)
	}
}

func TestStoreHandlerWatchDigestRejectsBadParams(t *testing.T) {
	router := Router(testSystem(t))
	for _, params := range []map[string]any{
		{"id": 0},
		{"id": -1},
		{},
		{"id": "seven"},
		{"watch_id": 1},
	} {
		if rpcErr := callMethod(t, router, "watch.digest", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("watch.digest %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}

func TestStoreHandlerWatchDigestWithoutWatchlistsFailsPrecondition(t *testing.T) {
	system := testSystem(t)
	system.Watches = nil
	rpcErr := callMethod(t, Router(system), "watch.digest", map[string]any{"id": 1}, nil)
	if rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("watch.digest without watchlists = %+v, want precondition_failed", rpcErr)
	}
}

func TestStoreHandlerFailuresListV1AggregatesAndTruncates(t *testing.T) {
	system := testSystem(t)
	first := storeHandlerUnavailableJob(t, system, "wr_store_summary_one", job.TerminalReasonCandidatesExhausted, nil)
	storeHandlerUnavailableJob(t, system, "wr_store_summary_two", job.TerminalReasonNoEntitlement, nil)
	router := Router(system)

	var page FailuresPage
	if rpcErr := callMethod(t, router, "failures.list_v1", map[string]any{}, &page); rpcErr != nil {
		t.Fatalf("failures.list_v1 = %+v", rpcErr)
	}
	if len(page.Failures) != 2 || page.Truncated {
		t.Fatalf("page = %+v, want two reason groups and truncated=false", page)
	}
	byReason := map[string]int{}
	for _, summary := range page.Failures {
		byReason[summary.Reason] = summary.Count
		if summary.Provider != "unknown" {
			t.Fatalf("summary %+v, want provider unknown for a candidate-less job", summary)
		}
	}
	if byReason[string(job.TerminalReasonCandidatesExhausted)] != 1 || byReason[string(job.TerminalReasonNoEntitlement)] != 1 {
		t.Fatalf("reason counts = %v", byReason)
	}

	// A limit below the group count must report truncation rather than
	// silently hiding a group.
	var capped FailuresPage
	if rpcErr := callMethod(t, router, "failures.list_v1", map[string]any{"limit": 1}, &capped); rpcErr != nil {
		t.Fatalf("capped failures.list_v1 = %+v", rpcErr)
	}
	if len(capped.Failures) != 1 || !capped.Truncated {
		t.Fatalf("capped page = %+v, want one group and truncated=true", capped)
	}

	// by_provider re-keys the aggregate: both jobs share the "unknown"
	// provider, so two reason groups collapse into one provider group whose
	// non-key column reports the spread.
	var byProvider FailuresPage
	if rpcErr := callMethod(t, router, "failures.list_v1", map[string]any{"by_provider": true}, &byProvider); rpcErr != nil {
		t.Fatalf("by_provider failures.list_v1 = %+v", rpcErr)
	}
	if len(byProvider.Failures) != 1 || byProvider.Failures[0].Count != 2 || byProvider.Failures[0].Reason != "multiple" {
		t.Fatalf("by_provider page = %+v", byProvider)
	}
	if byProvider.Failures[0].ExampleJobID == "" || byProvider.Failures[0].ExampleJobID == first+"-missing" {
		t.Fatalf("by_provider example job = %q", byProvider.Failures[0].ExampleJobID)
	}
}

func TestStoreHandlerFailuresListV1RejectsBadParams(t *testing.T) {
	router := Router(testSystem(t))
	for _, params := range []map[string]any{
		{"limit": "twenty"},
		{"by_provider": "yes"},
		{"provider": "example.com"},
	} {
		if rpcErr := callMethod(t, router, "failures.list_v1", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("failures.list_v1 %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}

func TestStoreHandlerFailuresListV1WithoutStoreFailsPrecondition(t *testing.T) {
	system := testSystem(t)
	system.Store = nil
	rpcErr := callMethod(t, Router(system), "failures.list_v1", map[string]any{}, nil)
	if rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("failures.list_v1 without a store = %+v, want precondition_failed", rpcErr)
	}
}

func TestStoreHandlerPageBulkStatsReportsFunnel(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	// One scan row (batch_id "") and one submit row (real batch_id): the two
	// halves of the funnel PageBulkStats computes over.
	if _, err := system.Store.DB().ExecContext(ctx, `
		INSERT INTO page_bulk_runs
			(detector_id, source_origin, detected_raw, canonical_unique, batch_id, opened_at, rendered_record_count_hint)
		VALUES ('generic-identifiers/1', '', 4, 2, '', '2026-08-01T00:00:00Z', 8)`); err != nil {
		t.Fatal(err)
	}
	if _, err := system.Store.DB().ExecContext(ctx, `
		INSERT INTO page_bulk_runs
			(detector_id, source_origin, selected, submitted, batch_id, opened_at, submitted_at)
		VALUES ('generic-identifiers/1', '', 3, 3, 'batch_store_stats', '2026-08-01T00:00:00Z', '2026-08-01T00:00:01Z')`); err != nil {
		t.Fatal(err)
	}

	var page PageBulkStatsPage
	if rpcErr := callMethod(t, Router(system), "stats.page_bulk", map[string]any{}, &page); rpcErr != nil {
		t.Fatalf("stats.page_bulk = %+v", rpcErr)
	}
	if len(page.Origins) != 1 {
		t.Fatalf("origins = %+v, want one row", page.Origins)
	}
	row := page.Origins[0]
	if row.TotalScanSessions != 1 || row.UsefulScanRate != 1 || row.SubmitConversion != 1 {
		t.Fatalf("row = %+v, want one useful scan converted by one submit", row)
	}
	if row.BulkLeverage == nil || *row.BulkLeverage != 3 {
		t.Fatalf("bulk leverage = %v, want 3", row.BulkLeverage)
	}
	if row.CanonicalUnique != 2 || row.RenderedRecordCountHint == nil || *row.RenderedRecordCountHint != 8 {
		t.Fatalf("row = %+v, want canonical 2 over hint 8", row)
	}
	if row.IdentifierYield == nil || *row.IdentifierYield != 0.25 {
		t.Fatalf("identifier yield = %v, want 0.25", row.IdentifierYield)
	}
}

func TestStoreHandlerPageBulkStatsRejectsBadParams(t *testing.T) {
	router := Router(testSystem(t))
	for _, params := range []map[string]any{
		{"limit": 1},
		{"origin_class": ""},
	} {
		if rpcErr := callMethod(t, router, "stats.page_bulk", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("stats.page_bulk %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}

func TestStoreHandlerPageBulkStatsWithoutStoreFailsPrecondition(t *testing.T) {
	system := testSystem(t)
	system.Store = nil
	rpcErr := callMethod(t, Router(system), "stats.page_bulk", map[string]any{}, nil)
	if rpcErr == nil || rpcErr.Code != "precondition_failed" {
		t.Fatalf("stats.page_bulk without a store = %+v, want precondition_failed", rpcErr)
	}
}

func TestStoreHandlerResolveEffectPermitSettlesUnknownCompletion(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	jobID := storeHandlerHandoffJob(t, system, "wr_store_permit")
	identity := job.EffectPermitIdentity{
		JobID: jobID, Kind: job.GenericDrive, DriveAttemptID: "attempt-store-permit",
		Ordinal: 0, Strategy: "generic", Revision: "r1",
	}
	permit, outcome, err := system.Jobs.AcquireEffectPermit(ctx, job.EffectPermitAcquireInput{
		Identity: identity, JobAttemptRevision: 1, BrowserHolderGeneration: 1,
		SafetyDomainID: "domain-store-permit", LeaseUntil: time.Now().Add(time.Minute),
		Authorization: job.EffectPermitEvent{Kind: "effect.authorized"},
	})
	if err != nil || outcome != job.EffectPermitAcquired || permit == nil {
		t.Fatalf("acquire permit outcome=%v permit=%+v err=%v", outcome, permit, err)
	}
	// An observation with no dispatch, download, acknowledgement, or proof is
	// exactly the unknown completion an operator override exists to close.
	if _, err := system.Jobs.ReconcileEffectPermit(ctx, job.EffectPermitObservation{
		PermitID: permit.ID, BrowserHolderGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	unknown, err := system.Jobs.GetEffectPermit(ctx, permit.ID)
	if err != nil || unknown == nil || unknown.Status != job.EffectPermitUnknownCompletion {
		t.Fatalf("permit before resolve = %+v err=%v", unknown, err)
	}

	var result struct {
		Resolved bool   `json:"resolved"`
		PermitID string `json:"permit_id"`
	}
	router := Router(system)
	if rpcErr := callMethod(t, router, "browser.effect_permit.resolve", map[string]any{
		"permit_id": permit.ID, "reason": "  the late result path was unavailable  ",
	}, &result); rpcErr != nil {
		t.Fatalf("browser.effect_permit.resolve = %+v", rpcErr)
	}
	if !result.Resolved || result.PermitID != permit.ID {
		t.Fatalf("resolve result = %+v, want resolved permit %s", result, permit.ID)
	}
	settled, err := system.Jobs.GetEffectPermit(ctx, permit.ID)
	if err != nil || settled == nil || settled.Status != job.EffectPermitSettled {
		t.Fatalf("permit after resolve = %+v err=%v, want settled", settled, err)
	}

	// A replay finds a settled permit. That is a stale override, and the
	// handler reports it as invalid_argument, never not_found or internal.
	if rpcErr := callMethod(t, router, "browser.effect_permit.resolve", map[string]any{
		"permit_id": permit.ID, "reason": "second override attempt",
	}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("replayed resolve = %+v, want invalid_argument", rpcErr)
	}
	// A permit id that never existed takes the same stale classification.
	if rpcErr := callMethod(t, router, "browser.effect_permit.resolve", map[string]any{
		"permit_id": "permit_store_missing", "reason": "override a permit that does not exist",
	}, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
		t.Fatalf("missing permit resolve = %+v, want invalid_argument", rpcErr)
	}
}

func TestStoreHandlerResolveEffectPermitRejectsBadParams(t *testing.T) {
	router := Router(testSystem(t))
	for _, params := range []map[string]any{
		{},
		{"permit_id": "permit_store", "reason": ""},
		{"permit_id": "permit_store", "reason": "   "},
		{"permit_id": "   ", "reason": "operator override"},
		{"reason": "operator override"},
		{"permit_id": "permit_store", "reason": "operator override", "force": true},
	} {
		if rpcErr := callMethod(t, router, "browser.effect_permit.resolve", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("browser.effect_permit.resolve %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}

func TestStoreHandlerDiagnoseJobProjectsV1AndV2(t *testing.T) {
	system := testSystem(t)
	// The cutover decision rides the decisive transition, which is the only
	// place the v2 projection reads it from.
	detail := job.WithCutoverDecision(nil, job.InstitutionCutoverDecision{
		Blocker: job.InstitutionCutoverBlockerNoLegalRoute, CanaryReadyRouteExists: true,
	})
	if detail == nil {
		t.Fatal("cutover detail is nil; the decision fixture is invalid")
	}
	id := storeHandlerUnavailableJob(t, system, "wr_store_diagnose", job.TerminalReasonCandidatesExhausted, detail)
	router := Router(system)

	var v1 JobDiagnosis
	if rpcErr := callMethod(t, router, "jobs.diagnose_v1", map[string]any{"job_id": id}, &v1); rpcErr != nil {
		t.Fatalf("jobs.diagnose_v1 = %+v", rpcErr)
	}
	if v1.JobID != id || v1.State != job.StateUnavailable || v1.Reason != job.DiagnosisReasonUnavailable {
		t.Fatalf("v1 diagnosis = %+v", v1)
	}
	if !v1.CanRetry || v1.Action != nil {
		t.Fatalf("v1 diagnosis = %+v, want retryable with no open action", v1)
	}

	var v2 JobDiagnosisV2
	if rpcErr := callMethod(t, router, "jobs.diagnose_v2", map[string]any{"job_id": id}, &v2); rpcErr != nil {
		t.Fatalf("jobs.diagnose_v2 = %+v", rpcErr)
	}
	if v2.Diagnosis != v1 {
		t.Fatalf("v2 base diagnosis = %+v, want the exact v1 body %+v", v2.Diagnosis, v1)
	}
	if v2.InstitutionCutover == nil {
		t.Fatal("v2 diagnosis dropped the recorded institution cutover decision")
	}
	if v2.InstitutionCutover.Blocker != job.InstitutionCutoverBlockerNoLegalRoute || !v2.InstitutionCutover.CanaryReadyRouteExists {
		t.Fatalf("v2 cutover = %+v", v2.InstitutionCutover)
	}

	// The wire difference is the whole reason the two wrappers exist: v1 must
	// never grow the field, because strict older clients reject it.
	var v1Fields map[string]json.RawMessage
	if rpcErr := callMethod(t, router, "jobs.diagnose_v1", map[string]any{"job_id": id}, &v1Fields); rpcErr != nil {
		t.Fatalf("jobs.diagnose_v1 fields = %+v", rpcErr)
	}
	if _, present := v1Fields["institution_cutover"]; present {
		t.Fatalf("v1 result carries institution_cutover: %v", v1Fields)
	}
	var v2Fields map[string]json.RawMessage
	if rpcErr := callMethod(t, router, "jobs.diagnose_v2", map[string]any{"job_id": id}, &v2Fields); rpcErr != nil {
		t.Fatalf("jobs.diagnose_v2 fields = %+v", rpcErr)
	}
	if _, present := v2Fields["institution_cutover"]; !present {
		t.Fatalf("v2 result omits institution_cutover: %v", v2Fields)
	}
}

func TestStoreHandlerDiagnoseJobClassifiesMissingAndBadParams(t *testing.T) {
	router := Router(testSystem(t))
	for _, method := range []string{"jobs.diagnose_v1", "jobs.diagnose_v2"} {
		// A job id that does not exist is not_found, not internal: callers
		// branch on that distinction.
		if rpcErr := callMethod(t, router, method, map[string]any{"job_id": "job_store_missing"}, nil); rpcErr == nil || rpcErr.Code != "not_found" {
			t.Fatalf("%s missing job = %+v, want not_found", method, rpcErr)
		}
		for _, params := range []map[string]any{
			{},
			{"job_id": ""},
			{"job_id": "   "},
			{"job_id": 7},
			{"job": "job_store_missing"},
			{"job_id": "job_store_missing", "verbose": true},
		} {
			if rpcErr := callMethod(t, router, method, params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
				t.Fatalf("%s %v = %+v, want invalid_argument", method, params, rpcErr)
			}
		}
	}
}

func TestStoreHandlerWorkPulseReadsSnapshot(t *testing.T) {
	system := testSystem(t)
	ctx := context.Background()
	if _, err := system.Jobs.CreateRequest(ctx, "wr_store_pulse", work.Work{
		Title: "Store handler pulse", DOI: "10.1000/wr_store_pulse",
	}, "", "", job.Policy{
		AccessMode: config.ModeConservative, DesiredVersion: "any", FetchMaxBytes: 1 << 20,
	}, nil, job.PrincipalCLI); err != nil {
		t.Fatal(err)
	}

	var result PulseResult
	if rpcErr := callMethod(t, Router(system), "work.pulse_v1", map[string]any{
		"request_id": "pulse_store_1", "schema_versions": []int{1},
	}, &result); rpcErr != nil {
		t.Fatalf("work.pulse_v1 = %+v", rpcErr)
	}
	if result.RequestID != "pulse_store_1" || result.Schema != 1 || result.GeneratedAt == "" {
		t.Fatalf("pulse = %+v", result)
	}
	if result.ProjectionComplete == nil || !*result.ProjectionComplete {
		t.Fatalf("projection complete = %v, want true for a healthy read", result.ProjectionComplete)
	}
	if result.NonterminalTotal == nil || *result.NonterminalTotal != 1 {
		t.Fatalf("nonterminal total = %v, want the one queued job", result.NonterminalTotal)
	}
}

func TestStoreHandlerWorkPulseRejectsBadParams(t *testing.T) {
	router := Router(testSystem(t))
	longID := ""
	for len(longID) <= 64 {
		longID += "x"
	}
	for _, params := range []map[string]any{
		{},
		{"request_id": "", "schema_versions": []int{1}},
		{"request_id": longID, "schema_versions": []int{1}},
		{"request_id": "pulse_store_bad", "schema_versions": []int{}},
		{"request_id": "pulse_store_bad", "schema_versions": []int{2}},
		{"request_id": "pulse_store_bad", "schema_versions": []int{1, 1}},
		{"request_id": "pulse_store_bad", "schema_versions": 1},
		{"request_id": "pulse_store_bad", "schema_versions": []int{1}, "detail": true},
	} {
		if rpcErr := callMethod(t, router, "work.pulse_v1", params, nil); rpcErr == nil || rpcErr.Code != "invalid_argument" {
			t.Fatalf("work.pulse_v1 %v = %+v, want invalid_argument", params, rpcErr)
		}
	}
}

func TestStoreHandlerWorkPulseWithoutServiceIsUnavailable(t *testing.T) {
	system := testSystem(t)
	system.Pulse = nil
	rpcErr := callMethod(t, Router(system), "work.pulse_v1", map[string]any{
		"request_id": "pulse_store_absent", "schema_versions": []int{1},
	}, nil)
	if rpcErr == nil || rpcErr.Code != "unavailable" {
		t.Fatalf("work.pulse_v1 without a pulse service = %+v, want unavailable", rpcErr)
	}
}

// notifyUnavailableMessage is the exact text the shared notifyUnavailable
// helper returns. Asserting it (not just the `unavailable` code) is what
// separates the nil-dependency guard from any other unavailable path, such
// as a platform capability refusal or a transport failure.
const notifyUnavailableMessage = "notification router is unavailable; restart the papio daemon"

func TestStoreHandlerNotifyUnavailableShow(t *testing.T) {
	system := testSystem(t)
	system.Notify = nil
	var result NotifyShowResult
	rpcErr := callMethod(t, Router(system), "notify.show_v1", struct{}{}, &result)
	if rpcErr == nil || rpcErr.Code != "unavailable" || rpcErr.Message != notifyUnavailableMessage {
		t.Fatalf("notify.show_v1 without a notify router = %+v, want the shared unavailable error", rpcErr)
	}
	if len(result.Rows) != 0 || result.Preset != "" {
		t.Fatalf("notify.show_v1 result = %+v, want no routing table on the guarded path", result)
	}
}

func TestStoreHandlerNotifyUnavailablePreview(t *testing.T) {
	system := testSystem(t)
	system.Notify = nil
	// The category is deliberately unknown: the guard runs before category
	// decoding, so a `invalid_argument` here would prove the router reached
	// past the shared helper instead of returning from it.
	rpcErr := callMethod(t, Router(system), "notify.preview_v1", map[string]any{
		"category": "not_a_real_category", "count": 7,
	}, nil)
	if rpcErr == nil || rpcErr.Code != "unavailable" || rpcErr.Message != notifyUnavailableMessage {
		t.Fatalf("notify.preview_v1 without a notify router = %+v, want the shared unavailable error", rpcErr)
	}
}

func TestStoreHandlerNotifyUnavailableTest(t *testing.T) {
	system := testSystem(t)
	system.Notify = nil
	// Same reasoning as preview, and it also pins the guard ahead of the
	// platform capability probe, whose unavailable message differs.
	rpcErr := callMethod(t, Router(system), "notify.test_v1", map[string]any{
		"category": "not_a_real_category",
	}, nil)
	if rpcErr == nil || rpcErr.Code != "unavailable" || rpcErr.Message != notifyUnavailableMessage {
		t.Fatalf("notify.test_v1 without a notify router = %+v, want the shared unavailable error", rpcErr)
	}
}
