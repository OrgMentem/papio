// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"papio/internal/artifact"
	"papio/internal/job"
	"papio/internal/redact"
	"papio/internal/store"
	"papio/internal/work"
)

// Both follow-up errors are what zotio printed for job_cb931061ba59b7de0229e07fb6
// on 2026-09-24, one and three seconds after it created the parent through
// the desktop connector: the Web API, where both commands read the parent,
// did not have it yet.
const (
	writePlane404CollectionErr = "zotio items: → writing via Zotero Web API: https://api.zotero.org/users/1\nError: GET /items/PA12RE34 returned HTTP 404: Not found"
	writePlane404EnrichErr     = "zotio items: → writing via Zotero Web API: https://api.zotero.org/users/1 (reads stay local)\nError: mutation incomplete"
	writePlane404EnrichOut     = `{"schema_version":1,"ok":false,"operation":"items.enrich","mode":"apply","plan":{"summary":{"selected":1,"planned":1}},"result":{"summary":{"attempted":1,"applied":0,"failed":1},"items":[{"op_id":"missing_abstract:PA12RE34","key":"PA12RE34","status":"failed","reason":"GET /items/PA12RE34 returned HTTP 404: Not found"}]},"journal":{"run_id":null}}`
)

// connectorImportService reproduces that job's apply: a new parent created
// through the connector, with a policy collection and auto-enrich on. The
// follow-ups fail with the given errors; the enrichment error carries its
// mutation envelope on stdout, as zotio prints it.
func connectorImportService(t *testing.T, collectionErr, enrichErr error, enrichOut string) (*Service, *planCLI, string) {
	t.Helper()
	cli := &planCLI{
		manifest:      `{"schema_version":2,"entries":[{"path":"paper.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"10.1002/example","status":"resolved","item":{"itemType":"journalArticle","title":"Example Paper","DOI":"10.1002/example"}}]}`,
		preview:       `{"ok":true,"mode":"preview","plan":{"summary":{"planned":1,"no_op":0,"invalid":0}},"result":null}`,
		apply:         `{"schema_version":1,"ok":true,"operation":"import.apply","mode":"apply","plan":{"summary":{"planned":1}},"result":{"summary":{"attempted":1,"applied":1,"no_op":0,"conflicts":0,"failed":0},"items":[{"op_id":"import.apply:001:create","key":"PA12RE34","status":"applied","reason":{"attachment_key":"AT56CH90","committed":true,"key":"PA12RE34","parent_key":"PA12RE34","via":"connector"}}]}}`,
		collectionErr: collectionErr,
		enrichErr:     enrichErr,
		enrichOut:     enrichOut,
	}
	service, jobID := readyPlanService(t, "", cli)
	service.AutoEnrich = true
	enablePlanAutoImport(t, service, jobID, "Trust in AI advice")
	status, parentKey, attachmentKey, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil || status != "applied" || parentKey != "PA12RE34" || attachmentKey != "AT56CH90" {
		t.Fatalf("import = (%q, %q, %q, %v), want applied PA12RE34/AT56CH90", status, parentKey, attachmentKey, err)
	}
	return service, cli, jobID
}

func webAPINotFoundImport(t *testing.T) (*Service, *planCLI, string) {
	t.Helper()
	return connectorImportService(t, errors.New(writePlane404CollectionErr), errors.New(writePlane404EnrichErr), writePlane404EnrichOut)
}

// latestZotioEvent returns the newest event of kind and when it was recorded.
func latestZotioEvent(t *testing.T, service *Service, jobID, kind string) (map[string]any, time.Time) {
	t.Helper()
	events, err := service.Bundle.Jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	var at time.Time
	for _, event := range events {
		if event["kind"] != kind {
			continue
		}
		detail, _ = event["detail"].(map[string]any)
		raw, _ := event["at"].(string)
		if at, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			t.Fatalf("%s at = %q: %v", kind, raw, err)
		}
	}
	if detail == nil {
		t.Fatalf("no %s event in %#v", kind, events)
	}
	return detail, at
}

func runFollowUps(t *testing.T, service *Service, now time.Time) {
	t.Helper()
	service.Now = func() time.Time { return now }
	if err := service.FollowUpRetrier().RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// The enrichment failure reached the operator as class "unknown" with a hint
// that was zotio's write-route notice plus "mutation incomplete", because the
// 404 lives only in the mutation envelope on stdout and papio classified the
// error line alone. Both follow-ups must name the 404, which is what marks
// them as sync lag rather than a fault, and say so in the hint.
func TestConnectorImportFollowUpsClassifyWebAPINotFound(t *testing.T) {
	service, _, jobID := webAPINotFoundImport(t)
	for _, kind := range []string{"zotio.collection_filing", "zotio.enrich"} {
		detail := zotioEventDetail(t, service, jobID, kind)
		if detail["status"] != "error" || detail["error_class"] != ErrorClassZoteroHTTP4xx ||
			detail["error_http_status"] != float64(404) || detail["error_hint"] != webAPINotSyncedHint {
			t.Errorf("%s event = %#v, want a classified Zotero HTTP 404 naming the sync lag", kind, detail)
		}
	}
}

// Apply is the only caller of either follow-up and the job has left ready, so
// before the retrier nothing ever tried again: the paper stayed outside its
// collection and without its abstract after Zotero had synced it.
func TestFollowUpRetrierFilesAndEnrichesOnceZoteroSyncs(t *testing.T) {
	service, cli, jobID := webAPINotFoundImport(t)
	_, filingFailedAt := latestZotioEvent(t, service, jobID, "zotio.collection_filing")
	_, enrichFailedAt := latestZotioEvent(t, service, jobID, "zotio.enrich")

	runFollowUps(t, service, filingFailedAt.Add(followUpRetryDelays[0]-time.Second))
	if cli.collectionCalls != 1 || cli.enrichCalls != 1 {
		t.Fatalf("retried inside the first delay: collection=%d enrich=%d", cli.collectionCalls, cli.enrichCalls)
	}

	cli.collectionErr, cli.enrichErr, cli.enrichOut = nil, nil, ""
	runFollowUps(t, service, enrichFailedAt.Add(followUpRetryDelays[0]))
	if cli.collectionCalls != 2 || cli.enrichCalls != 2 || cli.applyCalls != 1 {
		t.Fatalf("calls: collection=%d enrich=%d import=%d, want one retry of each follow-up and no second import",
			cli.collectionCalls, cli.enrichCalls, cli.applyCalls)
	}
	filing, _ := latestZotioEvent(t, service, jobID, "zotio.collection_filing")
	if filing["status"] != "applied" || filing["collection"] != "Trust in AI advice" {
		t.Fatalf("retried filing event = %#v", filing)
	}
	if enrich, _ := latestZotioEvent(t, service, jobID, "zotio.enrich"); enrich["status"] != "applied" || enrich["parent_key"] != "PA12RE34" {
		t.Fatalf("retried enrich event = %#v", enrich)
	}

	runFollowUps(t, service, enrichFailedAt.Add(30*24*time.Hour))
	if cli.collectionCalls != 2 || cli.enrichCalls != 2 {
		t.Fatalf("settled follow-ups ran again: collection=%d enrich=%d", cli.collectionCalls, cli.enrichCalls)
	}
}

// A library whose desktop never syncs must not be written to forever: each
// retry waits its delay after the previous failure, and the schedule ends.
func TestFollowUpRetrierGivesUpAfterSchedule(t *testing.T) {
	service, cli, jobID := webAPINotFoundImport(t)
	for i, delay := range followUpRetryDelays {
		_, failedAt := latestZotioEvent(t, service, jobID, "zotio.collection_filing")
		runFollowUps(t, service, failedAt.Add(delay-time.Second))
		if cli.collectionCalls != i+1 {
			t.Fatalf("retry %d ran before its %s delay: calls=%d", i+1, delay, cli.collectionCalls)
		}
		runFollowUps(t, service, failedAt.Add(delay))
		if cli.collectionCalls != i+2 {
			t.Fatalf("retry %d did not run after its %s delay: calls=%d", i+1, delay, cli.collectionCalls)
		}
	}
	_, failedAt := latestZotioEvent(t, service, jobID, "zotio.collection_filing")
	runFollowUps(t, service, failedAt.Add(30*24*time.Hour))
	if want := len(followUpRetryDelays) + 1; cli.collectionCalls != want {
		t.Fatalf("collection calls = %d, want %d: the schedule must end", cli.collectionCalls, want)
	}
}

// A failure that waiting cannot explain is not repeated: the request itself
// is wrong, or the write may already have happened. The timeout case is the
// fail-closed one — zotio may have reached Zotero before the deadline.
func TestFollowUpRetrierLeavesOtherFailuresAlone(t *testing.T) {
	for name, failure := range map[string]string{
		"unclassified": "zotio items: collection service unavailable",
		"timeout":      "zotio command timed out after 2m0s",
		"forbidden":    "zotio items: POST /collections returned HTTP 403: Forbidden",
	} {
		t.Run(name, func(t *testing.T) {
			service, cli, jobID := connectorImportService(t, errors.New(failure), errors.New(failure), "")
			_, failedAt := latestZotioEvent(t, service, jobID, "zotio.enrich")
			runFollowUps(t, service, failedAt.Add(30*24*time.Hour))
			if cli.collectionCalls != 1 || cli.enrichCalls != 1 {
				t.Fatalf("retried %s: collection=%d enrich=%d", name, cli.collectionCalls, cli.enrichCalls)
			}
		})
	}
}

// A Zotero outage, a rate limit and a refused connection are all reasons to
// ask again, and both follow-ups converge rather than add, so repeating one
// is safe even when Zotero committed the write and lost the response. Before,
// only the Web API's 404 entered the schedule: a 503 or a refused connection
// left the paper outside its collection and without its metadata for good.
func TestFollowUpRetrierRetriesTransientOutage(t *testing.T) {
	for name, tc := range map[string]struct{ collection, enrich, envelope string }{
		"service unavailable": {
			collection: "zotio items: → writing via Zotero Web API\nError: POST /collections returned HTTP 503: Service Unavailable",
			enrich:     "zotio items: → writing via Zotero Web API\nError: mutation incomplete",
			envelope:   `{"schema_version":1,"ok":false,"operation":"items.enrich","mode":"apply","result":{"summary":{"attempted":1,"applied":0,"failed":1},"items":[{"key":"PA12RE34","status":"failed","reason":"PATCH /items/PA12RE34 returned HTTP 503: Service Unavailable"}]}}`,
		},
		"rate limited": {
			collection: "zotio items: → writing via Zotero Web API\nError: POST /collections returned HTTP 429: Too Many Requests",
			enrich:     "zotio items: → writing via Zotero Web API\nError: PATCH /items/PA12RE34 returned HTTP 429: Too Many Requests",
		},
		"connection refused": {
			collection: "zotio items: writing via Zotero Web API: dial tcp 1.2.3.4:443: connect: connection refused",
			enrich:     "zotio items: writing via Zotero Web API: dial tcp 1.2.3.4:443: connect: connection refused",
		},
	} {
		t.Run(name, func(t *testing.T) {
			service, cli, jobID := connectorImportService(t, errors.New(tc.collection), errors.New(tc.enrich), tc.envelope)
			_, failedAt := latestZotioEvent(t, service, jobID, "zotio.enrich")

			runFollowUps(t, service, failedAt.Add(followUpRetryDelays[0]-time.Second))
			if cli.collectionCalls != 1 || cli.enrichCalls != 1 {
				t.Fatalf("retried inside the first delay: collection=%d enrich=%d", cli.collectionCalls, cli.enrichCalls)
			}

			cli.collectionErr, cli.enrichErr, cli.enrichOut = nil, nil, ""
			runFollowUps(t, service, failedAt.Add(followUpRetryDelays[0]))
			if cli.collectionCalls != 2 || cli.enrichCalls != 2 || cli.applyCalls != 1 {
				t.Fatalf("calls: collection=%d enrich=%d import=%d, want one retry of each and no second import",
					cli.collectionCalls, cli.enrichCalls, cli.applyCalls)
			}
			if filing, _ := latestZotioEvent(t, service, jobID, "zotio.collection_filing"); filing["status"] != "applied" {
				t.Fatalf("retried filing event = %#v", filing)
			}
			if enrich, _ := latestZotioEvent(t, service, jobID, "zotio.enrich"); enrich["status"] != "applied" {
				t.Fatalf("retried enrich event = %#v", enrich)
			}
		})
	}
}

// crashAfterRecordingImport leaves the durable state a process death between
// recording the import and running its follow-ups leaves: the exports ledger
// holds the successful apply, the job is imported, and neither follow-up has
// an event, nor does the completion marker. The CLI counters are reset so a
// later assertion counts only what the repair ran.
func crashAfterRecordingImport(t *testing.T, service *Service, cli *planCLI, jobID string) {
	t.Helper()
	if _, err := service.Store.DB().Exec(
		`DELETE FROM events WHERE job_id = ? AND kind IN (?, ?, ?)`,
		jobID, followUpCollectionFiling, followUpEnrich, followUpsComplete); err != nil {
		t.Fatal(err)
	}
	cli.collectionCalls, cli.enrichCalls = 0, 0
}

// markFollowUpsCompleteAt records another job's completion marker, which is
// what dates the epoch the repair scan reads.
func markFollowUpsCompleteAt(t *testing.T, service *Service, jobID string, at time.Time) {
	t.Helper()
	if _, err := service.Store.DB().Exec(
		`INSERT INTO events (job_id, at, kind, detail_json) VALUES (?, ?, ?, '{"status":"done"}')`,
		jobID, store.FormatTime(at), followUpsComplete); err != nil {
		t.Fatal(err)
	}
}

// Nothing drives an imported job again, so a death between the recorded
// import and its follow-ups used to leave the paper outside its collection
// and without its metadata for good. Maintenance must finish both, once.
func TestFollowUpRetrierRepairsImportThatRecordedNoFollowUp(t *testing.T) {
	service, cli, jobID := connectorImportService(t, nil, nil, "")
	if _, at := latestZotioEvent(t, service, jobID, followUpsComplete); at.IsZero() {
		t.Fatal("a finished import records no completion marker, so a crash cannot be told from a policy")
	}
	crashAfterRecordingImport(t, service, cli, jobID)
	// A store that has been recording markers: an earlier import completed.
	markFollowUpsCompleteAt(t, service, "job_earlier_import", time.Now().Add(-30*24*time.Hour))

	runFollowUps(t, service, time.Now())
	if cli.collectionCalls != 1 || cli.enrichCalls != 1 || cli.applyCalls != 1 {
		t.Fatalf("repair: collection=%d enrich=%d import=%d, want one of each follow-up and no second import",
			cli.collectionCalls, cli.enrichCalls, cli.applyCalls)
	}
	if filing, _ := latestZotioEvent(t, service, jobID, "zotio.collection_filing"); filing["status"] != "applied" ||
		filing["collection"] != "Trust in AI advice" {
		t.Fatalf("repaired filing event = %#v", filing)
	}
	if enrich, _ := latestZotioEvent(t, service, jobID, "zotio.enrich"); enrich["status"] != "applied" ||
		enrich["parent_key"] != "PA12RE34" {
		t.Fatalf("repaired enrich event = %#v", enrich)
	}
	row, err := service.Bundle.Jobs.Get(context.Background(), jobID)
	if err != nil || row.State != job.StateImported {
		t.Fatalf("job state = %q, %v; want imported", row.State, err)
	}

	runFollowUps(t, service, time.Now())
	if cli.collectionCalls != 1 || cli.enrichCalls != 1 {
		t.Fatalf("repaired follow-ups ran again: collection=%d enrich=%d", cli.collectionCalls, cli.enrichCalls)
	}
}

// An import that the same death left in ready is replayed from the exports
// ledger. That replay filed the collection but never enriched, so the parent
// kept no DOI or abstract however often the import retry ran it.
func TestApplyReplayCompletesFollowUpsMissedByACrash(t *testing.T) {
	service, cli, jobID := connectorImportService(t, nil, nil, "")
	crashAfterRecordingImport(t, service, cli, jobID)

	status, parentKey, _, err := service.PlanAndApply(context.Background(), jobID)
	if err != nil || status != "applied" || parentKey != "PA12RE34" {
		t.Fatalf("replay = (%q, %q, %v), want the recorded import", status, parentKey, err)
	}
	if cli.collectionCalls != 1 || cli.enrichCalls != 1 || cli.applyCalls != 1 {
		t.Fatalf("replay: collection=%d enrich=%d import=%d, want one of each follow-up and no second import",
			cli.collectionCalls, cli.enrichCalls, cli.applyCalls)
	}

	if _, _, _, err := service.PlanAndApply(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	if cli.collectionCalls != 1 || cli.enrichCalls != 1 {
		t.Fatalf("a second replay repeated the follow-ups: collection=%d enrich=%d", cli.collectionCalls, cli.enrichCalls)
	}
}

// The repair has no age cutoff, so its bound is the epoch Apply records
// before it claims an import. An import older than that instant predates the
// marker itself, so its missing follow-ups are not evidence of a crash, and
// repairing them would rewrite the operator's library from today's
// configuration.
func TestFollowUpRepairIgnoresImportsFromBeforeTheMarkerEpoch(t *testing.T) {
	service, cli, jobID := connectorImportService(t, nil, nil, "")
	crashAfterRecordingImport(t, service, cli, jobID)

	// Apply dated the epoch before it claimed the import, so exactly one
	// epoch exists and it predates the import it bounds.
	var epochs int
	if err := service.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind = ? AND job_id IS NULL`, followUpsComplete).Scan(&epochs); err != nil {
		t.Fatal(err)
	}
	if epochs != 1 {
		t.Fatalf("epoch rows = %d, want exactly one from Apply", epochs)
	}

	// A legacy import from before papio wrote markers carries no completion
	// marker for a reason that is not a crash. Backdate this victim behind
	// the epoch to stand in for one: the pass must leave it alone.
	var epochAt string
	if err := service.Store.DB().QueryRow(
		`SELECT at FROM events WHERE kind = ? AND job_id IS NULL ORDER BY at ASC LIMIT 1`, followUpsComplete).Scan(&epochAt); err != nil {
		t.Fatal(err)
	}
	epochTime, err := time.Parse(time.RFC3339Nano, epochAt)
	if err != nil {
		t.Fatalf("epoch at = %q: %v", epochAt, err)
	}
	if _, err := service.Store.DB().Exec(
		`UPDATE exports SET created_at = ? WHERE job_id = ? AND kind = 'zotio_apply'`,
		store.FormatTime(epochTime.Add(-30*24*time.Hour)), jobID); err != nil {
		t.Fatal(err)
	}
	runFollowUps(t, service, time.Now())
	if cli.collectionCalls != 0 || cli.enrichCalls != 0 {
		t.Fatalf("swept an import from before the epoch: collection=%d enrich=%d", cli.collectionCalls, cli.enrichCalls)
	}

	// Later passes do not re-date the epoch, and the legacy import stays out.
	runFollowUps(t, service, time.Now())
	if cli.collectionCalls != 0 || cli.enrichCalls != 0 {
		t.Fatalf("swept an import from before the epoch: collection=%d enrich=%d", cli.collectionCalls, cli.enrichCalls)
	}
	if err := service.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind = ? AND job_id IS NULL`, followUpsComplete).Scan(&epochs); err != nil {
		t.Fatal(err)
	}
	if epochs != 1 {
		t.Fatalf("epoch rows = %d, want exactly one", epochs)
	}

	// The same crash victim newer than the epoch is a crash, not policy, and
	// is repaired however old its earlier marker is.
	if _, err := service.Store.DB().Exec(
		`UPDATE exports SET created_at = ? WHERE job_id = ? AND kind = 'zotio_apply'`,
		store.Now(), jobID); err != nil {
		t.Fatal(err)
	}
	markFollowUpsCompleteAt(t, service, "job_earlier_import", time.Now().Add(-365*24*time.Hour))
	runFollowUps(t, service, time.Now())
	if cli.collectionCalls != 1 || cli.enrichCalls != 1 {
		t.Fatalf("repair behind the epoch: collection=%d enrich=%d", cli.collectionCalls, cli.enrichCalls)
	}
}

// A follow-up whose event was lost must not look finished: the completion
// marker commits only when every follow-up event was durably recorded, so
// maintenance still finds the work.
func TestFollowUpLeavesJobUnmarkedWhenEventRecordingFails(t *testing.T) {
	service, cli, jobID := connectorImportService(t, nil, nil, "")
	crashAfterRecordingImport(t, service, cli, jobID)

	if _, err := service.Store.DB().Exec(
		`CREATE TRIGGER fail_zotio_filing BEFORE INSERT ON events WHEN NEW.kind = 'zotio.collection_filing' BEGIN SELECT RAISE(ABORT, 'fail filing'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = service.Store.DB().Exec(`DROP TRIGGER IF EXISTS fail_zotio_filing`)
	})

	row, err := service.Bundle.Jobs.Get(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	collection := row.Policy.Collection
	applyJSON := recordedApplyJSON(t, service, jobID)
	var apply ApplyResult
	if err := json.Unmarshal([]byte(applyJSON), &apply); err != nil {
		t.Fatal(err)
	}
	plan := &Plan{JobID: jobID, Collection: collection}
	did, err := service.fileCollection(context.Background(), plan, &apply)
	if !did || err == nil {
		t.Fatalf("fileCollection = (%v, %v), want (true, error) when the event insert fails", did, err)
	}
	if countZotioEvents(t, service, jobID, followUpCollectionFiling) != 0 {
		t.Fatal("a failed event insert left a filing event behind")
	}
	if countZotioEvents(t, service, jobID, followUpsComplete) != 0 {
		t.Fatal("a failed filing event still recorded a completion marker candidate")
	}
	if cli.collectionCalls != 1 {
		t.Fatalf("collection calls = %d, want 1: the Zotero write ran, only its record failed", cli.collectionCalls)
	}

	// The repair must not mark the job while its filing event cannot be
	// recorded either: the work stays visible instead of being lost.
	runFollowUps(t, service, time.Now())
	if countZotioEvents(t, service, jobID, followUpsComplete) != 0 {
		t.Fatal("repair committed a completion marker without a durable filing event")
	}

	// Once the store accepts events again, the next pass finishes the same
	// work and only then marks it done.
	if _, err := service.Store.DB().Exec(`DROP TRIGGER fail_zotio_filing`); err != nil {
		t.Fatal(err)
	}
	cli.collectionCalls, cli.enrichCalls = 0, 0
	runFollowUps(t, service, time.Now())
	if filing := zotioEventDetail(t, service, jobID, followUpCollectionFiling); filing["status"] != "applied" {
		t.Fatalf("repaired filing event = %#v, want applied", filing)
	}
	if enrich := zotioEventDetail(t, service, jobID, followUpEnrich); enrich["status"] != "applied" {
		t.Fatalf("repaired enrich event = %#v, want applied", enrich)
	}
	if countZotioEvents(t, service, jobID, followUpsComplete) == 0 {
		t.Fatal("repaired import carries no completion marker")
	}
}

// The daemon serves the socket before its first maintenance pass, so an
// import that crashes before that pass is the socket-availability window,
// not history: Apply already dated the epoch before it claimed the import,
// so the first maintenance pass must repair it with no earlier marker.
func TestFollowUpRepairIncludesCrashBeforeFirstMaintenance(t *testing.T) {
	service, cli, jobID := connectorImportService(t, nil, nil, "")
	var epochs int
	if err := service.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind = ? AND job_id IS NULL`, followUpsComplete).Scan(&epochs); err != nil {
		t.Fatal(err)
	}
	if epochs != 1 {
		t.Fatalf("epoch rows = %d, want the one Apply recorded before the import", epochs)
	}
	var epochAt, importedAt string
	if err := service.Store.DB().QueryRow(
		`SELECT at FROM events WHERE kind = ? AND job_id IS NULL ORDER BY at ASC LIMIT 1`, followUpsComplete).Scan(&epochAt); err != nil {
		t.Fatal(err)
	}
	if err := service.Store.DB().QueryRow(
		`SELECT created_at FROM exports WHERE job_id = ? AND kind = 'zotio_apply' ORDER BY id DESC LIMIT 1`, jobID).Scan(&importedAt); err != nil {
		t.Fatal(err)
	}
	if importedAt < epochAt {
		t.Fatalf("import created_at %q predates epoch %q: the durable boundary excludes the crash it should repair", importedAt, epochAt)
	}

	crashAfterRecordingImport(t, service, cli, jobID)
	runFollowUps(t, service, time.Now())
	if cli.collectionCalls != 1 || cli.enrichCalls != 1 {
		t.Fatalf("first maintenance after the crash: collection=%d enrich=%d, want one of each", cli.collectionCalls, cli.enrichCalls)
	}
	if countZotioEvents(t, service, jobID, followUpsComplete) == 0 {
		t.Fatal("repaired import carries no completion marker")
	}
}

// One maintenance pass runs at most maxFollowUpsPerPass Zotio commands. A
// repair that counts imports instead of commands runs two commands for one
// import and exceeds the bound.
func TestFollowUpRepairBoundsCommandsNotImports(t *testing.T) {
	service, cli, firstID := connectorImportService(t, nil, nil, "")
	jobIDs := []string{firstID}
	for range 3 {
		jobIDs = append(jobIDs, crashVictimJob(t, service, "Trust in AI advice"))
	}
	for _, id := range jobIDs {
		crashAfterRecordingImport(t, service, cli, id)
	}
	cli.collectionCalls, cli.enrichCalls = 0, 0

	runFollowUps(t, service, time.Now())
	total := cli.collectionCalls + cli.enrichCalls
	if total != maxFollowUpsPerPass {
		t.Fatalf("first pass commands = %d (collection=%d enrich=%d), want exactly %d",
			total, cli.collectionCalls, cli.enrichCalls, maxFollowUpsPerPass)
	}
	// The pass stopped mid-job or mid-scan, so later passes still have work:
	// nothing was marked finished without its events.
	secondCollection, secondEnrich := cli.collectionCalls, cli.enrichCalls
	runFollowUps(t, service, time.Now())
	if cli.collectionCalls+cli.enrichCalls <= secondCollection+secondEnrich {
		t.Fatalf("second pass made no progress: collection=%d enrich=%d",
			cli.collectionCalls, cli.enrichCalls)
	}
	for range 4 {
		runFollowUps(t, service, time.Now())
	}
	for _, id := range jobIDs {
		if countZotioEvents(t, service, id, followUpsComplete) == 0 {
			t.Fatalf("job %s carries no completion marker after bounded passes", id)
		}
	}
}

// countZotioEvents reports how many events of kind a job carries.
func countZotioEvents(t *testing.T, service *Service, jobID, kind string) int {
	t.Helper()
	var n int
	if err := service.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE job_id = ? AND kind = ?`, jobID, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// recordedApplyJSON returns the newest recorded zotio_apply result for a job.
func recordedApplyJSON(t *testing.T, service *Service, jobID string) string {
	t.Helper()
	var raw string
	if err := service.Store.DB().QueryRow(
		`SELECT result_json FROM exports WHERE job_id = ? AND kind = 'zotio_apply' ORDER BY id DESC LIMIT 1`, jobID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// crashVictimJob imports one more paper through the same service and store so
// a bound test can hold several crash victims behind one epoch. It returns
// the new job's ID with its follow-ups still recorded; the caller crashes it.
func crashVictimJob(t *testing.T, service *Service, collection string) string {
	t.Helper()
	ctx := context.Background()
	w := work.Work{
		DOI: "10.1002/example", Title: "Example Paper", Authors: []string{"Ada Lovelace"}, Year: 2024,
	}
	requestID := "request_plan_" + job.NewID("wr")
	jobID, err := service.Bundle.Jobs.CreateRequest(ctx, requestID, w, "", "", job.Policy{AccessMode: "conservative", DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil, job.PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Bundle.Jobs.InsertCandidates(ctx, jobID, []job.Candidate{{
		JobID: jobID, Source: "unpaywall", URLRedacted: redact.URL("https://example.test/paper.pdf"), URLKey: "url-key-" + jobID,
		LandingRedacted: "https://example.test/article", Version: "published", AccessBasis: "open_access",
		ReuseLicense: "cc-by-4.0", ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := service.Bundle.Jobs.NextPendingCandidate(ctx, jobID)
	if err != nil || candidate == nil {
		t.Fatalf("candidate = %+v, %v", candidate, err)
	}
	if err := service.Bundle.Jobs.MarkCandidate(ctx, candidate.ID, "accepted"); err != nil {
		t.Fatal(err)
	}
	quarantine, err := service.Bundle.Artifacts.QuarantineDir(jobID)
	if err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(quarantine, "paper.tmp")
	body := []byte("%PDF-1.4\nfixture " + w.Describe() + "\n%%EOF")
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, _, err := artifact.HashFile(temp)
	if err != nil {
		t.Fatal(err)
	}
	artifactPath, _, err := service.Bundle.Artifacts.Promote(temp, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Bundle.Jobs.UpsertArtifact(ctx, job.Artifact{
		SHA256: sha, SizeBytes: int64(len(body)), MIME: "application/pdf", PageCount: 1,
		TextChars: 1000, IdentityResult: "pass", Path: artifactPath,
	}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][2]string{{job.StateQueued, job.StateResolving}, {job.StateResolving, job.StateFetching}, {job.StateFetching, job.StateValidating}} {
		if err := service.Bundle.Jobs.Transition(ctx, jobID, edge[0], edge[1], nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Bundle.Jobs.Transition(ctx, jobID, job.StateValidating, job.StateReady, nil, job.WithCandidate(candidate.ID), job.WithArtifact(sha)); err != nil {
		t.Fatal(err)
	}
	row, err := service.Bundle.Jobs.Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	row.Policy.AutoImport = true
	row.Policy.Collection = collection
	policyJSON, err := json.Marshal(row.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Store.DB().Exec(`UPDATE jobs SET policy_json = ? WHERE id = ?`, string(policyJSON), jobID); err != nil {
		t.Fatal(err)
	}
	status, parentKey, _, err := service.PlanAndApply(ctx, jobID)
	if err != nil || status != "applied" || parentKey != "PA12RE34" {
		t.Fatalf("victim import = (%q, %q, %v), want applied PA12RE34", status, parentKey, err)
	}
	return jobID
}
