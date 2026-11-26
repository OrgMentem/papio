// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestHeartbeatReportsCancelledJobAsLostLease pins papio-bcd62aa64125e723: a
// cancel clears the holder's lease just as the processor's own final
// transition does, so Heartbeat used to answer both with nil and the scheduler
// never stopped the processor. A processor never cancels its own job, so a
// cancelled job is a lost lease; the processor's own terminal and parked
// transitions stay a normal completion because it keeps working after them.
func TestHeartbeatReportsCancelledJobAsLostLease(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		finish  func(js *Store, id string) error
		wantErr bool
	}{
		{
			name:    "external cancel",
			finish:  func(js *Store, id string) error { return js.Cancel(ctx, id, TerminalReasonCancelledByUser) },
			wantErr: true,
		},
		{
			name: "own ready transition",
			finish: func(js *Store, id string) error {
				return js.Transition(ctx, id, StateResolving, StateReady, nil)
			},
		},
		{
			name: "own retry_wait transition",
			finish: func(js *Store, id string) error {
				return js.Transition(ctx, id, StateResolving, StateRetryWait, nil, WithRetryAt(time.Now().Add(time.Hour)))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			id, err := js.CreateRequest(ctx, "wr_heartbeat_cancel", testWork(), "", "", testPolicy(), nil, PrincipalUnknown)
			if err != nil {
				t.Fatal(err)
			}
			if row, err := js.ClaimNext(ctx, "owner1", time.Minute); err != nil || row == nil || row.ID != id {
				t.Fatalf("claim = %#v, %v", row, err)
			}
			if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
				t.Fatalf("to resolving: %v", err)
			}
			if err := js.Heartbeat(ctx, id, "owner1", time.Minute); err != nil {
				t.Fatalf("heartbeat of a held lease: %v", err)
			}
			if err := tc.finish(js, id); err != nil {
				t.Fatalf("finish: %v", err)
			}
			err = js.Heartbeat(ctx, id, "owner1", time.Minute)
			if tc.wantErr {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("heartbeat after external cancel = %v, want ErrConflict (lost lease)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("heartbeat after the processor's own transition = %v, want nil", err)
			}
		})
	}
}
