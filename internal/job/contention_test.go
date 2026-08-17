// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"testing"

	"papio/internal/work"
)

// contendingJob creates a job and drives it to state, returning its id.
func contendingJob(t *testing.T, js *Store, ctx context.Context, requestID, doi, state string) string {
	t.Helper()
	w := work.Work{DOI: doi, Title: "An Example Paper", Authors: []string{"Author, A."}, Year: 2020}
	id, err := js.CreateRequest(ctx, requestID, w, "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatalf("create %s: %v", requestID, err)
	}
	if state == StateQueued {
		return id
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("queued->resolving: %v", err)
	}
	if state != StateResolving {
		if err := js.Transition(ctx, id, StateResolving, state, nil); err != nil {
			t.Fatalf("resolving->%s: %v", state, err)
		}
	}
	return id
}

func TestOtherWorkWaitingSeesJobsThatWillMakeRequests(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()

	// One job, nothing else: no contention, so a per-job credit share must
	// not bind. An unspent allowance cannot be carried forward, so deferring
	// the only running job would cost throughput and buy nothing.
	mine := contendingJob(t, js, ctx, "wr_c_0001", "10.1002/mine", StateResolving)
	waiting, err := js.OtherWorkWaiting(ctx, mine)
	if err != nil {
		t.Fatalf("OtherWorkWaiting: %v", err)
	}
	if waiting {
		t.Fatal("waiting = true with only the asking job present")
	}

	// A second job in retry_wait is exactly the victim this share exists to
	// protect: it was refused and is waiting for its turn.
	contendingJob(t, js, ctx, "wr_c_0002", "10.1002/other", StateRetryWait)
	waiting, err = js.OtherWorkWaiting(ctx, mine)
	if err != nil {
		t.Fatalf("OtherWorkWaiting: %v", err)
	}
	if !waiting {
		t.Fatal("waiting = false with another job parked in retry_wait")
	}
}

func TestOtherWorkWaitingIgnoresJobsParkedOnPeople(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	mine := contendingJob(t, js, ctx, "wr_c_0010", "10.1002/mine", StateResolving)

	// Measured on the operator's store: of 139 jobs in awaiting_human, zero
	// had made a wire attempt since parking. Counting them would make
	// contention permanently true on any long-lived install, turning a share
	// that binds under pressure into one that always binds.
	for _, state := range []string{StateAwaitingHuman, StateReady, StateFailed, StateUnavailable, StateCancelled} {
		id := contendingJob(t, js, ctx, "wr_c_"+state, "10.1002/"+state, StateResolving)
		if err := js.Transition(ctx, id, StateResolving, state, nil); err != nil {
			t.Fatalf("cannot reach %s directly: %v", state, err)
		}
		waiting, err := js.OtherWorkWaiting(ctx, mine)
		if err != nil {
			t.Fatalf("OtherWorkWaiting: %v", err)
		}
		if waiting {
			t.Fatalf("a job in %s counted as waiting for an allowance", state)
		}
	}
}

func TestOtherWorkWaitingExcludesTheAskingJob(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	mine := contendingJob(t, js, ctx, "wr_c_0020", "10.1002/mine", StateResolving)

	// A job must never be its own contention: that would make the share bind
	// against the only work running, which is precisely the case the design
	// says must proceed.
	waiting, err := js.OtherWorkWaiting(ctx, mine)
	if err != nil {
		t.Fatalf("OtherWorkWaiting: %v", err)
	}
	if waiting {
		t.Fatal("the asking job counted itself as contention")
	}

	// Asking on behalf of nobody (empty id) sees that same job, which is what
	// makes unattributed egress unable to accidentally exempt itself.
	waiting, err = js.OtherWorkWaiting(ctx, "")
	if err != nil {
		t.Fatalf("OtherWorkWaiting: %v", err)
	}
	if !waiting {
		t.Fatal("waiting = false for an empty job id with a resolving job present")
	}
}
