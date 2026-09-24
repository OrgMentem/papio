// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"papio/internal/store"
)

// The event shapes below are copied from the live store's 2026-09-23 run,
// one per delivery route, with job ids, drive ids, filenames and digests
// replaced. Each fixture is the attempt's own stream up to the promotion.

type producerFixtureEvent struct {
	kind   string
	detail map[string]any
}

func producerJob(t *testing.T, js *Store, requestID, source string, events []producerFixtureEvent) (string, int64) {
	t.Helper()
	ctx := context.Background()
	jobID, err := js.CreateRequest(ctx, requestID, testWork(), "", "", testPolicy(), nil, PrincipalCLI)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{StateQueued, StateResolving}, {StateResolving, StateFetching}} {
		if err := js.Transition(ctx, jobID, step[0], step[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range events {
		if err := js.RecordEvent(ctx, jobID, event.kind, event.detail); err != nil {
			t.Fatal(err)
		}
	}
	if err := js.Transition(ctx, jobID, StateFetching, StateValidating, nil); err != nil {
		t.Fatal(err)
	}
	url := "browser://adopted-download"
	if source != "browser" {
		url = "https://oa.example.test/paper.pdf"
	}
	if _, err := js.InsertCandidates(ctx, jobID, []Candidate{{
		JobID: jobID, Source: source, URLRedacted: url, URLKey: requestID,
		Version: "unknown", AccessBasis: "manual", ReuseLicense: "unknown", Rank: 0,
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := js.NextPendingCandidate(ctx, jobID)
	if err != nil || candidate == nil {
		t.Fatalf("candidate = %+v, %v", candidate, err)
	}
	return jobID, candidate.ID
}

// promoteForProducer runs the one production promotion path: a prepared
// main publication finalized validating -> ready.
func promoteForProducer(t *testing.T, js *Store, jobID string, candidateID int64, sha string) ArtifactProducerRecord {
	t.Helper()
	ctx := context.Background()
	input := publicationInput("publication_"+jobID, jobID, &candidateID, PublicationRoleMain, sha)
	input.FromState, input.ToState = StateValidating, StateReady
	input.TransitionDetail = map[string]any{"candidate_id": candidateID, "sha256": sha}
	if err := js.PreparePublication(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err := js.FinalizePublication(ctx, input.ID, func() (PromotionResult, error) {
		return PromotionResult{Path: input.Artifact.Path, Created: true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	var records []string
	rows, err := js.S.DB().QueryContext(ctx,
		`SELECT detail_json FROM events WHERE job_id = ? AND kind = ?`, jobID, ArtifactProducerEvent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		records = append(records, raw)
	}
	if len(records) != 1 {
		t.Fatalf("artifact.producer events = %d, want exactly one per promotion", len(records))
	}
	var record ArtifactProducerRecord
	if err := json.Unmarshal([]byte(records[0]), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func genericTuple(attempt string) map[string]any {
	return map[string]any{"effect_kind": "generic_drive", "drive_attempt_id": attempt, "ordinal": 0, "strategy": "generic", "revision": "1"}
}

func assertProducer(t *testing.T, got ArtifactProducerRecord, producer Producer, interventions ...Intervention) {
	t.Helper()
	if got.Producer != producer {
		t.Fatalf("producer = %q (basis %q), want %q; record %+v", got.Producer, got.Basis, producer, got)
	}
	if interventions == nil {
		interventions = []Intervention{}
	}
	if !reflect.DeepEqual(got.Interventions, interventions) {
		t.Fatalf("interventions = %v, want %v", got.Interventions, interventions)
	}
}

func TestGenericDriveDownloadRecordsAdapterProducer(t *testing.T) {
	js := testStore(t)
	sha := strings.Repeat("1", 64)
	// A jamanetwork paper: the primo adapter reported no_entitlement, the
	// operator opened the handoff, and the generic drive downloaded the PDF.
	jobID, candidateID := producerJob(t, js, "wr_producer_adapter", "browser", []producerFixtureEvent{
		{"browser.provider_outcome", map[string]any{"adapter_id": "primo", "adapter_version": "0.4.0", "outcome": "no_entitlement"}},
		{"job.retry_requested", map[string]any{"action_id": 1, "action_revision": 1, "reason": "operator_redrive"}},
		{"handoff.opened", map[string]any{"batch_size": 1, "consumer": "browser-page", "principal": "cli"}},
		{"browser.provider_drive_epoch_started", map[string]any{"drive_attempt_id": "drive-adapter", "ordinal": 0, "revision": "1", "strategy": "generic"}},
		{"browser.provider_drive_epoch_result", map[string]any{"detail": "PDF download completed", "drive_attempt_id": "drive-adapter", "ordinal": 0, "outcome": "success", "revision": "1", "strategy": "generic"}},
		{"browser.download_complete", map[string]any{"download_id": 458, "filename": "paper.pdf", "producer": genericTuple("drive-adapter"), "size_bytes": 1018860}},
		{"browser.download_complete", map[string]any{"filename": "paper.pdf", "producer": genericTuple("drive-adapter"), "sha256": sha}},
	})
	got := promoteForProducer(t, js, jobID, candidateID, sha)
	assertProducer(t, got, ProducerAdapter, InterventionOpen)
	if got.AdapterID != "generic" || got.EffectKind != "generic_drive" || got.Basis != "download_digest" || got.OpenedBy != "cli" {
		t.Fatalf("record = %+v", got)
	}
}

func TestInstitutionalWileyDownloadWithoutTupleStaysUnknown(t *testing.T) {
	js := testStore(t)
	// The Wiley paper of 08:29Z: papio's claim opened the tab, a challenge
	// was cleared, and the download arrived with no effect tuple. Nothing on
	// record says whether the wiley adapter or the operator clicked.
	jobID, candidateID := producerJob(t, js, "wr_producer_wiley", "browser", []producerFixtureEvent{
		{"handoff.opened", map[string]any{"batch_size": 1, "consumer": "browser-page", "principal": "cli"}},
		{"browser.institutional_effect_result", map[string]any{"binding_id": "binding_x", "claim_id": "claim_x", "effect_ordinal": 1, "outcome": "acknowledged"}},
		{"browser.error", map[string]any{"code": "challenge_blocked"}},
		{"browser.challenge_cleared", map[string]any{}},
		{"browser.download_started", map[string]any{"download_id": 430, "filename": "paper.pdf"}},
		{"browser.download_complete", map[string]any{"download_id": 430, "filename": "paper.pdf", "size_bytes": 2713444}},
		{"browser.delivery_context", map[string]any{"download_id": 430, "page_host": "advanced.onlinelibrary.wiley.com", "route": "direct", "session_evidence": "none"}},
	})
	got := promoteForProducer(t, js, jobID, candidateID, strings.Repeat("2", 64))
	assertProducer(t, got, ProducerUnknown, InterventionOpen, InterventionChallenge)
	if got.AdapterID != "" || got.Basis != "no_effect_tuple" {
		t.Fatalf("record = %+v; an unbound download must not name an adapter", got)
	}
}

func TestAgentDecisionInTheDeliveringDriveRecordsAgent(t *testing.T) {
	js := testStore(t)
	sha := strings.Repeat("3", 64)
	jobID, candidateID := producerJob(t, js, "wr_producer_agent", "browser", []producerFixtureEvent{
		{"handoff.opened", map[string]any{"batch_size": 1, "principal": "cli"}},
		// An earlier drive whose agent decision led nowhere.
		{"browser.provider_drive_epoch_started", map[string]any{"drive_attempt_id": "drive-old", "ordinal": 0, "revision": "1", "strategy": "generic"}},
		{"browser.agent_decision_completed", map[string]any{"outcome": "decision", "permit_id": "permit_old", "request_id": "request_old", "choice": "c7"}},
		{"browser.provider_drive_epoch_started", map[string]any{"drive_attempt_id": "drive-agent", "ordinal": 0, "revision": "1", "strategy": "generic"}},
		{"browser.agent_decision_completed", map[string]any{"outcome": "decision", "permit_id": "permit_agent", "request_id": "request_agent", "choice": "c26"}},
		{"browser.provider_drive_epoch_result", map[string]any{"detail": "PDF download completed", "drive_attempt_id": "drive-agent", "ordinal": 0, "outcome": "success", "revision": "1", "strategy": "generic"}},
		{"browser.download_complete", map[string]any{"filename": "paper.pdf", "producer": genericTuple("drive-agent"), "sha256": sha}},
	})
	for permit, drive := range map[string]string{"permit_old": "drive-old", "permit_agent": "drive-agent"} {
		insertDrivePermit(t, js, jobID, permit, drive)
	}
	got := promoteForProducer(t, js, jobID, candidateID, sha)
	assertProducer(t, got, ProducerAgent, InterventionOpen)
	if got.AgentDecisionID != "request_agent" || got.AdapterID != "" {
		t.Fatalf("record = %+v, want the decision taken under the delivering drive's permit", got)
	}
}

func TestAgentDecisionUnderAnotherDriveDoesNotClaimTheDownload(t *testing.T) {
	js := testStore(t)
	sha := strings.Repeat("4", 64)
	jobID, candidateID := producerJob(t, js, "wr_producer_agent_other", "browser", []producerFixtureEvent{
		{"browser.provider_drive_epoch_started", map[string]any{"drive_attempt_id": "drive-old", "ordinal": 0, "revision": "1", "strategy": "generic"}},
		{"browser.agent_decision_completed", map[string]any{"outcome": "decision", "permit_id": "permit_old", "request_id": "request_old"}},
		{"browser.provider_drive_epoch_started", map[string]any{"drive_attempt_id": "drive-new", "ordinal": 0, "revision": "1", "strategy": "generic"}},
		{"browser.download_complete", map[string]any{"filename": "paper.pdf", "producer": genericTuple("drive-new"), "sha256": sha}},
	})
	insertDrivePermit(t, js, jobID, "permit_old", "drive-old")
	got := promoteForProducer(t, js, jobID, candidateID, sha)
	assertProducer(t, got, ProducerAdapter)
}

func insertDrivePermit(t *testing.T, js *Store, jobID, permitID, drive string) {
	t.Helper()
	now := store.Now()
	if _, err := js.S.DB().ExecContext(context.Background(), `INSERT INTO effect_permits
		(id, job_id, job_attempt_revision, browser_holder_generation, safety_domain_id, effect_kind,
		 drive_attempt_id, ordinal, strategy, revision, status, created_at, updated_at)
		VALUES (?, ?, 1, 1, 'institution:example', 'generic_drive', ?, 0, 'generic', '1', 'settled', ?, ?)`,
		permitID, jobID, drive, now, now); err != nil {
		t.Fatal(err)
	}
}

func TestPDFGrabAdoptionRecordsManualFile(t *testing.T) {
	js := testStore(t)
	jobID, candidateID := producerJob(t, js, "wr_producer_grab", "browser", nil)
	now := store.Now()
	if _, err := js.S.DB().ExecContext(context.Background(), `INSERT INTO pdf_grabs
		(id, url_host, title, state, job_id, created_at, updated_at)
		VALUES ('grab_producer', 'pdf.example.test', 'paper', 'job_created', ?, ?, ?)`, jobID, now, now); err != nil {
		t.Fatal(err)
	}
	got := promoteForProducer(t, js, jobID, candidateID, strings.Repeat("5", 64))
	assertProducer(t, got, ProducerManual, InterventionManualFile)
	if got.Basis != "pdf_grab" {
		t.Fatalf("basis = %q", got.Basis)
	}
}

func TestDownloadAfterAdapterGaveUpRecordsManual(t *testing.T) {
	js := testStore(t)
	// The adapter reported ui_changed on the delivering visit; no drive or
	// open followed, so the PDF that then arrived was saved by a person.
	jobID, candidateID := producerJob(t, js, "wr_producer_ui_changed", "browser", []producerFixtureEvent{
		{"handoff.opened", map[string]any{"batch_size": 1, "principal": "cli"}},
		{"browser.provider_outcome", map[string]any{"adapter_id": "psycnet", "adapter_version": "0.2.1", "outcome": "ui_changed"}},
		{"browser.download_complete", map[string]any{"download_id": 9, "filename": "paper.pdf", "size_bytes": 1}},
	})
	got := promoteForProducer(t, js, jobID, candidateID, strings.Repeat("6", 64))
	assertProducer(t, got, ProducerManual, InterventionOpen, InterventionManualFile)
	if got.AdapterID != "psycnet" || got.AdapterVersion != "0.2.1" || got.Basis != "adapter_stopped" {
		t.Fatalf("record = %+v, want the adapter that stopped", got)
	}
}

func TestTermsStopIsAnInterventionNotAManualDownload(t *testing.T) {
	js := testStore(t)
	// After terms are accepted the page reloads and the adapter may click
	// the PDF itself, so the stop does not make the download manual.
	jobID, candidateID := producerJob(t, js, "wr_producer_terms", "browser", []producerFixtureEvent{
		{"handoff.opened", map[string]any{"batch_size": 1, "principal": "cli"}},
		{"browser.auth_pending", map[string]any{}},
		{"browser.auth_returned", map[string]any{"elapsed_ms": 3124}},
		{"browser.provider_outcome", map[string]any{"adapter_id": "jstor", "adapter_version": "0.3.2", "outcome": "terms_acceptance_required"}},
		{"browser.download_started", map[string]any{"download_id": 456, "filename": "paper.pdf"}},
	})
	got := promoteForProducer(t, js, jobID, candidateID, strings.Repeat("7", 64))
	assertProducer(t, got, ProducerUnknown, InterventionOpen, InterventionSignIn, InterventionTerms)
}

func TestViewerCaptureRecordAttributesBytesAdoptedBeforeTheirDownloadReport(t *testing.T) {
	js := testStore(t)
	// The loopback run of 2026-09-24: Firefox captured ScienceDirect's signed
	// viewer response, and the adoption sweep promoted the saved file before
	// download_complete reached the daemon. Only the capture record, which the
	// extension sends before it saves, was on file.
	jobID, candidateID := producerJob(t, js, "wr_producer_viewer_capture", "browser", []producerFixtureEvent{
		{"handoff.opened", map[string]any{"batch_size": 1, "principal": "cli"}},
		{"browser.institutional_effect_result", map[string]any{"binding_id": "binding_v", "claim_id": "claim_v", "effect_ordinal": 1, "outcome": "acknowledged"}},
		{"browser.viewer_capture", map[string]any{"mechanism": "stream_capture", "adapter_id": "sciencedirect", "adapter_version": "sciencedirect/0.4.0"}},
		{"browser.download_started", map[string]any{"download_id": 1, "filename": "paper.pdf"}},
	})
	got := promoteForProducer(t, js, jobID, candidateID, strings.Repeat("d", 64))
	assertProducer(t, got, ProducerViewerCapture, InterventionOpen)
	if got.AdapterID != "sciencedirect" || got.AdapterVersion != "sciencedirect/0.4.0" || got.Basis != "viewer_capture" || got.OpenedBy != "cli" {
		t.Fatalf("record = %+v, want the capture and the adapter that armed it", got)
	}
}

func TestViewerCaptureThatFailedDoesNotClaimALaterManualDownload(t *testing.T) {
	js := testStore(t)
	// The capture was incomplete, so the extension asked for the viewer's
	// Download button; the file that then arrived was saved by a person.
	jobID, candidateID := producerJob(t, js, "wr_producer_viewer_capture_failed", "browser", []producerFixtureEvent{
		{"handoff.opened", map[string]any{"batch_size": 1, "principal": "cli"}},
		{"browser.viewer_capture", map[string]any{"mechanism": "download_rule", "adapter_id": "sciencedirect", "adapter_version": "sciencedirect/0.4.0"}},
		{"browser.provider_outcome", map[string]any{"outcome": "native_viewer_download_required", "detail": "the browser download of the viewer URL was interrupted"}},
		{"browser.download_complete", map[string]any{"download_id": 12, "filename": "paper.pdf", "size_bytes": 1}},
	})
	got := promoteForProducer(t, js, jobID, candidateID, strings.Repeat("e", 64))
	assertProducer(t, got, ProducerManual, InterventionOpen, InterventionManualFile)
	if got.Basis != "adapter_stopped" {
		t.Fatalf("record = %+v, want the stopped capture, not the capture", got)
	}
}

func TestDaemonOAFetchRecordsDaemonFetchUnattended(t *testing.T) {
	js := testStore(t)
	jobID, candidateID := producerJob(t, js, "wr_producer_oa", "unpaywall", nil)
	got := promoteForProducer(t, js, jobID, candidateID, strings.Repeat("8", 64))
	assertProducer(t, got, ProducerDaemonFetch)
	if got.Source != "unpaywall" || got.Basis != "resolver_candidate" {
		t.Fatalf("record = %+v", got)
	}
}

func TestBrowserDirectGetRecordsDaemonFetch(t *testing.T) {
	js := testStore(t)
	sha := strings.Repeat("9", 64)
	jobID, candidateID := producerJob(t, js, "wr_producer_direct", "browser", []producerFixtureEvent{
		{"browser.download_complete", map[string]any{"filename": "paper.pdf", "sha256": sha, "producer": map[string]any{
			"effect_kind": "direct_get", "drive_attempt_id": "drive-direct", "ordinal": 0, "strategy": "direct_get", "revision": "r1"}}},
	})
	got := promoteForProducer(t, js, jobID, candidateID, sha)
	assertProducer(t, got, ProducerDaemonFetch)
	if got.EffectKind != "direct_get" {
		t.Fatalf("record = %+v", got)
	}
}

func TestPacerOpenedJobIsUnattendedApartFromSignIn(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)

	// The pacer's open, after an operator open that the redrive superseded:
	// only the attempt that produced the bytes is attributed.
	paced, pacedCandidate := producerJob(t, js, "wr_producer_paced", "browser", []producerFixtureEvent{
		{"handoff.opened", map[string]any{"batch_size": 1, "principal": "cli"}},
		{"job.retry_requested", map[string]any{"action_id": 2, "action_revision": 1, "reason": "operator_redrive"}},
		{"handoff.opened", map[string]any{"batch_size": 1, "principal": PacerPrincipal}},
		{"browser.auth_pending", map[string]any{}},
		{"browser.auth_returned", map[string]any{"elapsed_ms": 4209}},
		{"browser.provider_drive_epoch_started", map[string]any{"drive_attempt_id": "drive-paced", "ordinal": 0, "revision": "1", "strategy": "generic"}},
		{"browser.download_complete", map[string]any{"download_id": 1, "filename": "paper.pdf", "producer": genericTuple("drive-paced"), "size_bytes": 1}},
	})
	got := promoteForProducer(t, js, paced, pacedCandidate, strings.Repeat("a", 64))
	assertProducer(t, got, ProducerAdapter, InterventionSignIn)
	if got.OpenedBy != PacerPrincipal || got.Attended() {
		t.Fatalf("record = %+v, want pacer-opened and unattended apart from sign-in", got)
	}

	operator, operatorCandidate := producerJob(t, js, "wr_producer_operator", "browser", []producerFixtureEvent{
		{"handoff.opened", map[string]any{"batch_size": 1, "principal": "cli"}},
		{"browser.download_complete", map[string]any{"download_id": 2, "filename": "paper.pdf", "size_bytes": 1}},
	})
	promoteForProducer(t, js, operator, operatorCandidate, strings.Repeat("b", 64))
	oa, oaCandidate := producerJob(t, js, "wr_producer_stats_oa", "arxiv", nil)
	promoteForProducer(t, js, oa, oaCandidate, strings.Repeat("c", 64))

	stats, err := js.ProducerStats(ctx, since, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Acquired != 3 || stats.Unattended != 1 || stats.SignInOnly != 1 || stats.Intervened != 1 || stats.Unrecorded != 0 {
		t.Fatalf("stats = %+v, want 3 acquired: 1 unattended, 1 sign-in only, 1 intervened", stats)
	}
	want := map[Producer]int{ProducerAdapter: 1, ProducerAgent: 0, ProducerViewerCapture: 0, ProducerDaemonFetch: 1, ProducerManual: 0, ProducerUnknown: 1}
	if !reflect.DeepEqual(stats.Producers, want) {
		t.Fatalf("producers = %v, want %v", stats.Producers, want)
	}
	if stats.OpenedBy[PacerPrincipal] != 1 || stats.OpenedBy["cli"] != 1 || stats.Interventions[InterventionSignIn] != 1 || stats.Interventions[InterventionOpen] != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestProducerStatsCountsReadyWithoutARecord(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	since := time.Now().Add(-time.Hour)
	jobID, _ := producerJob(t, js, "wr_producer_legacy", "browser", nil)
	// A ready transition written without the publication path, as every
	// promotion before this record existed was.
	if err := js.Transition(ctx, jobID, StateValidating, StateReady, map[string]any{"sha256": strings.Repeat("d", 64)}, WithArtifact(strings.Repeat("d", 64))); err != nil {
		t.Fatal(err)
	}
	stats, err := js.ProducerStats(ctx, since, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Acquired != 0 || stats.Unrecorded != 1 {
		t.Fatalf("stats = %+v, want the unrecorded promotion counted apart", stats)
	}
}

// A period that begins or ends inside a second must split that second by
// instant. Every row here is written in the store's own text form; under
// time.RFC3339Nano, which trims trailing fraction zeros, "…:05Z" sorted after
// the later since "…:05.1Z", and "…:06.59561Z" after the later until
// "…:06.595612Z", so the event before the period was counted and the two
// inside it were not.
func TestProducerStatsSplitsASecondByInstant(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	second := time.Date(2026, 9, 24, 6, 19, 5, 0, time.UTC)
	since, until := second.Add(100*time.Millisecond), second.Add(time.Second+595612*time.Microsecond)
	for _, event := range []struct {
		at               time.Time
		jobID, kind, raw string
	}{
		{second, "job-before", ArtifactProducerEvent, `{"producer":"manual"}`},
		{second.Add(time.Second + 595610*time.Microsecond), "job-inside", ArtifactProducerEvent, `{"producer":"adapter"}`},
		{second.Add(time.Second + 595600*time.Microsecond), "job-unrecorded", "job.transition", `{"to":"ready"}`},
		{until, "job-at-end", ArtifactProducerEvent, `{"producer":"agent"}`},
	} {
		if _, err := js.S.DB().ExecContext(ctx,
			`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, ?, ?)`,
			event.jobID, store.FormatTime(event.at), event.kind, event.raw); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := js.ProducerStats(ctx, since, until)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Acquired != 1 || stats.Producers[ProducerAdapter] != 1 || stats.Unrecorded != 1 {
		t.Fatalf("stats = %+v, want only the adapter event and the unrecorded promotion inside [since, until)", stats)
	}
}
