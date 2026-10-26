// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOnceReceiptRetainsConvergedTerminalJob(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	pol := testPolicy()
	w := testWork()
	first, err := js.CreateRequestForWork(ctx, "other-request", w, "", "", pol, nil, Attribution{}, false)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := js.CreateOnceRequestForWork(ctx, "durable-watch-request", w, "", "", pol, nil, Attribution{})
	if err != nil || joined.JobID != first.JobID || !joined.Existing {
		t.Fatalf("convergence: %+v %v", joined, err)
	}
	if err := js.Transition(ctx, first.JobID, StateQueued, StateCancelled, nil); err != nil {
		t.Fatal(err)
	}
	again, err := js.CreateOnceRequestForWork(ctx, "durable-watch-request", w, "", "", pol, nil, Attribution{})
	if err != nil || again.JobID != first.JobID || !again.Existing {
		t.Fatalf("terminal replay: %+v %v", again, err)
	}
	var count int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("job count=%d %v", count, err)
	}
	w.DOI = "10.1234/different"
	if _, err := js.CreateOnceRequestForWork(ctx, "durable-watch-request", w, "", "", pol, nil, Attribution{}); err == nil {
		t.Fatal("changed request identity reused a receipt")
	}
}
func TestOncePublishedRequestDoesNotJoinAnyVersionAcquisition(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	pol := testPolicy()
	pol.DesiredVersion = "any"
	w := testWork()
	anyVersion, err := js.CreateRequestForWork(ctx, "any-version", w, "", "", pol, nil, Attribution{}, false)
	if err != nil {
		t.Fatal(err)
	}
	pol.DesiredVersion = "published"
	published, err := js.CreateOnceRequestForWork(ctx, "publication-notice", w, "", "", pol, nil, Attribution{})
	if err != nil || published.JobID == anyVersion.JobID {
		t.Fatalf("publication request: %+v %v", published, err)
	}
	row, err := js.Get(ctx, published.JobID)
	if err != nil || row.Policy.DesiredVersion != "published" {
		t.Fatalf("published policy: %+v %v", row, err)
	}
}

func TestOnceCanonicalWorkRefusesDifferentFilingIntent(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	w := testWork()
	firstPol := testPolicy()
	firstPol.AutoImport = true
	first, err := js.CreateRequestForWork(ctx, "plain-acquire", w, "", "", firstPol, nil, Attribution{}, false)
	if err != nil {
		t.Fatal(err)
	}
	campaign := testPolicy()
	_, err = js.CreateOnceRequestForWork(ctx, "campaign-x-work-1", w, "", "campaign_x", campaign, nil, Attribution{})
	if !errors.Is(err, ErrOnceConflict) {
		t.Fatalf("campaign once-only with different filing intent = %v; want ErrOnceConflict", err)
	}
	if msg := err.Error(); !strings.Contains(msg, first.JobID) || !strings.Contains(msg, "filing intent differs") {
		t.Fatalf("conflict = %q; want it to name live job %q and the differing intent", msg, first.JobID)
	}
	var count int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE state NOT IN ('cancelled','failed','ready','imported','unavailable')`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("live jobs = %d, %v; want one live job, no duplicate", count, err)
	}
	var receipts int
	if err := js.S.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM submission_receipts WHERE request_id='campaign-x-work-1'`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("receipts = %d, %v; want no receipt committed for the refused request", receipts, err)
	}
	same := testPolicy()
	same.AutoImport = true
	joined, err := js.CreateOnceRequestForWork(ctx, "campaign-x-work-2", w, "", "", same, nil, Attribution{})
	if err != nil || joined.JobID != first.JobID || !joined.Existing {
		t.Fatalf("same-intent once-only = %+v, %v; want existing job %q", joined, err, first.JobID)
	}
}

func TestOnceConflictIsTyped(t *testing.T) {
	ctx := context.Background()
	js := testStore(t)
	pol := testPolicy()
	w := testWork()
	if _, err := js.CreateOnceRequestForWork(ctx, "durable-request", w, "", "", pol, nil, Attribution{}); err != nil {
		t.Fatal(err)
	}
	changed := testWork()
	changed.DOI = "10.1234/different"
	if _, err := js.CreateOnceRequestForWork(ctx, "durable-request", changed, "", "", pol, nil, Attribution{}); !errors.Is(err, ErrOnceConflict) {
		t.Fatalf("changed identity replay = %v; want ErrOnceConflict", err)
	}
	if _, err := js.CreateRequestForWork(ctx, "shared-request", testWork(), "", "", pol, nil, Attribution{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateOnceRequestForWork(ctx, "shared-request", testWork(), "", "other-collection", pol, nil, Attribution{}); !errors.Is(err, ErrOnceConflict) {
		t.Fatalf("same request with changed filing intent = %v; want ErrOnceConflict", err)
	}
}
