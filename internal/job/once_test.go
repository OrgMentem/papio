// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import (
	"context"
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
