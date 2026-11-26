// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

// cancelRecorder subscribes to js and remembers each notified job with the
// state the job had in the store when the hook ran.
type cancelRecorder struct {
	mu     sync.Mutex
	jobs   []string
	states []string
}

func recordCancellations(t *testing.T, js *Store) (*cancelRecorder, func()) {
	t.Helper()
	recorder := &cancelRecorder{}
	unsubscribe := js.OnCancelled(func(jobID string) {
		row, err := js.Get(context.Background(), jobID)
		state := "unreadable"
		if err == nil {
			state = row.State
		}
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		recorder.jobs = append(recorder.jobs, jobID)
		recorder.states = append(recorder.states, state)
	})
	return recorder, unsubscribe
}

func (r *cancelRecorder) notified() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.jobs)
}

// TestCancelNotifiesSubscribersAfterTheCommit pins the in-process notice a
// scheduler uses to stop a cancelled job's processor at once: it fires once
// per committed cancellation, after the job reads cancelled, and not for a
// no-op cancel, another transition, or a subscriber that has left.
func TestCancelNotifiesSubscribersAfterTheCommit(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	recorder, unsubscribe := recordCancellations(t, js)

	id, err := js.CreateRequest(ctx, "wr_cancel_hook_1", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Cancel(ctx, id, TerminalReasonCancelledByUser); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := recorder.notified(); !slices.Equal(got, []string{id}) || recorder.states[0] != StateCancelled {
		t.Fatalf("notified %v in states %v; want %q once, already cancelled", got, recorder.states, id)
	}
	// Cancelling a job that is already terminal commits nothing.
	if err := js.Cancel(ctx, id, TerminalReasonCancelledByUser); err != nil {
		t.Fatalf("repeated Cancel: %v", err)
	}
	if got := recorder.notified(); len(got) != 1 {
		t.Fatalf("a no-op cancel notified %v; want only the first", got)
	}

	// A transition that is not a cancellation says nothing.
	failing, err := js.CreateRequest(ctx, "wr_cancel_hook_2", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.ClaimNext(ctx, "owner", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, failing, StateQueued, StateResolving, nil); err != nil {
		t.Fatal(err)
	}
	if err := js.Transition(ctx, failing, StateResolving, StateFailed, nil, WithTerminalReason("test")); err != nil {
		t.Fatal(err)
	}
	if got := recorder.notified(); len(got) != 1 {
		t.Fatalf("a failed transition notified %v; want only the cancellation", got)
	}

	unsubscribe()
	later, err := js.CreateRequest(ctx, "wr_cancel_hook_3", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if err := js.Cancel(ctx, later, TerminalReasonCancelledByUser); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := recorder.notified(); len(got) != 1 {
		t.Fatalf("an unsubscribed hook was notified of %v", got)
	}
}

// TestCancelHookCoversDismissalAndReviewRejection: every path that commits a
// job as cancelled reaches the subscriber, not only Store.Cancel called
// directly.
func TestCancelHookCoversDismissalAndReviewRejection(t *testing.T) {
	js := testStore(t)
	ctx := context.Background()
	recorder, unsubscribe := recordCancellations(t, js)
	defer unsubscribe()

	dismissed, _, dismissAction := parkIdentityReview(t, js, "wr_cancel_hook_dismiss")
	if _, err := js.DismissHumanAction(ctx, dismissAction, 1); err != nil {
		t.Fatalf("DismissHumanAction: %v", err)
	}
	if got := recorder.notified(); !slices.Equal(got, []string{dismissed}) {
		t.Fatalf("dismissal notified %v; want %q", got, dismissed)
	}

	rejected, _, rejectAction := parkIdentityReview(t, js, "wr_cancel_hook_reject")
	if resolution, err := js.ResolveReviewCAS(ctx, ResolveReviewInput{ActionID: rejectAction, Verdict: "reject", ExpectedRevision: 1}); err != nil || resolution.Outcome != ReviewApplied || resolution.State != StateCancelled {
		t.Fatalf("ResolveReviewCAS(reject) = %+v, %v; want applied and cancelled", resolution, err)
	}
	if got := recorder.notified(); !slices.Equal(got, []string{dismissed, rejected}) {
		t.Fatalf("rejection notified %v; want the dismissed then the rejected job", got)
	}
}
