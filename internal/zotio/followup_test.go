// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"errors"
	"testing"
	"time"
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

// Only the Web API's 404 is sync lag. Any other failure is not known to heal
// by waiting, so the retrier must not repeat a write it cannot explain.
func TestFollowUpRetrierLeavesOtherFailuresAlone(t *testing.T) {
	service, cli, jobID := connectorImportService(t,
		errors.New("zotio items: collection service unavailable"), errors.New("zotio items: enrichment unavailable"), "")
	_, failedAt := latestZotioEvent(t, service, jobID, "zotio.enrich")
	runFollowUps(t, service, failedAt.Add(time.Hour))
	if cli.collectionCalls != 1 || cli.enrichCalls != 1 {
		t.Fatalf("retried a non-404 failure: collection=%d enrich=%d", cli.collectionCalls, cli.enrichCalls)
	}
}
