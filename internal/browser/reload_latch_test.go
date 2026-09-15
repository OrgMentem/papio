// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"testing"
	"time"

	"papio/internal/protocol"
)

func TestReloadLatchFreshHelloReleasesCurrentHolder(t *testing.T) {
	b, _, _, _ := newBridge(t)
	_ = settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	if _, _, err := b.RequestDevReload(); err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	msgs, _ := runSyncAs(t, b, sessA)
	if firstOfType(msgs, protocol.MsgDevReload) == nil {
		t.Fatalf("dev_reload not emitted, got %+v", msgs)
	}

	msgs, _ = runSyncAs(t, b, sessB, helloAs("0.15.0"))
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil || ack.Payload.(*protocol.HelloAckPayload).Role != sessionRoleHolder {
		t.Fatalf("fresh hello during reload latch = %+v, want role-holder hello_ack", msgs)
	}
	if busy := firstOfType(msgs, protocol.MsgError); busy != nil {
		t.Fatalf("fresh hello during reload latch was denied: %+v", msgs)
	}

	b.mu.Lock()
	holder := b.arbitration.holderSession()
	oldHolderKnown := b.arbitration.known(sessA)
	reservedFor := b.arbitration.reservedFor
	reservedUntil := b.arbitration.reservedUntil
	b.mu.Unlock()
	if holder == nil || holder.ID != sessB {
		t.Fatalf("holder = %+v, want session B", holder)
	}
	if oldHolderKnown {
		t.Fatal("session A remains known after its reload-latched replacement arrived")
	}
	if reservedFor != "" || !reservedUntil.IsZero() {
		t.Fatalf("reload reservation = %q until %v, want cleared", reservedFor, reservedUntil)
	}
	if _, _, takeovers := b.Sessions(); takeovers != 0 {
		t.Fatalf("takeovers = %d, want reload-latched holder treated as departed", takeovers)
	}
}

func TestReloadLatchExpiredHelloStillDenied(t *testing.T) {
	b, _, _, _ := newBridge(t)
	advance := settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.15.0"))
	if _, _, err := b.RequestDevReload(); err != nil {
		t.Fatalf("RequestDevReload: %v", err)
	}
	msgs, _ := runSyncAs(t, b, sessA)
	if firstOfType(msgs, protocol.MsgDevReload) == nil {
		t.Fatalf("dev_reload not emitted, got %+v", msgs)
	}

	advance(devReloadReservation + time.Nanosecond)
	runSyncAs(t, b, sessA)
	msgs, _ = runSyncAs(t, b, sessB, helloAs("0.15.0"))
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil || ack.Payload.(*protocol.HelloAckPayload).Role != sessionRolePending {
		t.Fatalf("hello after reload latch expiry = %+v, want role-pending hello_ack", msgs)
	}
	busy := firstOfType(msgs, protocol.MsgError)
	if busy == nil || busy.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("hello after reload latch expiry = %+v, want session_busy", msgs)
	}

	b.mu.Lock()
	holder := b.arbitration.holderSession()
	b.mu.Unlock()
	if holder == nil || holder.ID != sessA {
		t.Fatalf("holder = %+v, want live session A", holder)
	}
}
