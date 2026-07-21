// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Multi-browser session arbitration: exactly one session holds the
// offer/handoff flow; competitors wait as pending instead of silently
// stealing it (the two-browser fight from the 2026-07-20 field report).

package browser

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"papio/internal/protocol"
)

const (
	sessA = "aaaa1111aaaa1111aaaa1111aaaa1111"
	sessB = "bbbb2222bbbb2222bbbb2222bbbb2222"
)

func helloAs(version string) []byte {
	return []byte(`{"protocol":"papio-browser/1","type":"hello","msg_id":"client-hello-arb","seq":0,"payload":{"extension_version":"` + version + `"}}`)
}

// settableClock rigs the bridge clock; returns an advance function.
func settableClock(b *Bridge) func(time.Duration) {
	current := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return current }
	return func(d time.Duration) { current = current.Add(d) }
}

func TestSecondBrowserHelloIsDeniedNotStolen(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_arb_deny", handoffWork())

	msgs, _ := runSyncAs(t, b, sessA, helloAs("0.4.0"))
	if firstOfType(msgs, protocol.MsgHelloAck) == nil || firstOfType(msgs, protocol.MsgJobOffer) == nil {
		t.Fatalf("holder must receive hello_ack + job_offer, got %+v", msgs)
	}

	msgs, _ = runSyncAs(t, b, sessB, helloAs("0.3.1"))
	busy := firstOfType(msgs, protocol.MsgError)
	if busy == nil || busy.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("second browser must be denied with session_busy, got %+v", msgs)
	}
	if firstOfType(msgs, protocol.MsgJobOffer) != nil {
		t.Fatal("denied session must not receive offers")
	}

	// The holder's identity is stable: no version flap.
	version, _, connected := b.SessionInfo()
	if !connected || version != "0.4.0" {
		t.Fatalf("SessionInfo = %q/%v, want holder 0.4.0", version, connected)
	}

	// Pending session polls receive nothing; the parked job stays A's.
	msgs, _ = runSyncAs(t, b, sessB)
	if len(msgs) != 0 {
		t.Fatalf("pending poll = %+v, want empty", msgs)
	}
	sessions, denied, _ := b.Sessions()
	if len(sessions) != 2 || !sessions[0].Holder || sessions[0].ID != sessA || denied != 1 {
		t.Fatalf("sessions = %+v denied = %d", sessions, denied)
	}
	_ = id
}

func TestNonHolderStatelessFramesPassAndHandoffFramesBlock(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_arb_frames", handoffWork())
	runSyncAs(t, b, sessA, helloAs("0.4.0"))
	runSyncAs(t, b, sessB, helloAs("0.3.1"))

	// Stateless page_acquire from the non-holder browser still works.
	msgs, _ := runSyncAs(t, b, sessB, inFrame(t, protocol.MsgPageAcquire, "",
		map[string]any{"url": "https://journals.example.test/article", "doi": "10.1234/arb-test"}))
	ack := firstOfType(msgs, protocol.MsgPageAcquireAck)
	if ack == nil || ack.Payload.(*protocol.PageAcquireAckPayload).JobID == "" {
		t.Fatalf("page_acquire from pending session must submit, got %+v", msgs)
	}

	// A handoff frame from the non-holder is refused and records nothing.
	msgs, _ = runSyncAs(t, b, sessB, inFrame(t, protocol.MsgJobAccept, id, map[string]any{}))
	busy := firstOfType(msgs, protocol.MsgError)
	if busy == nil || busy.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("job_accept from pending session = %+v, want session_busy", msgs)
	}
	events, _ := jobs.Events(context.Background(), id)
	for _, e := range events {
		if e["kind"] == "browser.job_accept" {
			t.Fatal("non-holder job_accept must not be recorded")
		}
	}
}

func TestStaleHolderYieldsToLiveSession(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	advance := settableClock(b)
	id := park(t, jobs, "wr_arb_stale", handoffWork())

	runSyncAs(t, b, sessA, helloAs("0.3.1"))
	runSyncAs(t, b, sessB, helloAs("0.4.0")) // denied, pending

	// A goes silent (killed native host); B keeps polling.
	advance(sessionStaleAfter + time.Second)
	msgs, _ := runSyncAs(t, b, sessB)
	if firstOfType(msgs, protocol.MsgHelloAck) == nil {
		t.Fatalf("promoted session must receive its withheld hello_ack, got %+v", msgs)
	}
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer == nil || offer.JobID != id {
		t.Fatalf("promoted session must receive the outstanding offer, got %+v", msgs)
	}
	version, _, _ := b.SessionInfo()
	if version != "0.4.0" {
		t.Fatalf("holder after takeover = %q, want 0.4.0", version)
	}
	if _, _, takeovers := b.Sessions(); takeovers != 1 {
		t.Fatalf("takeovers = %d, want 1", takeovers)
	}
}

func TestGoodbyeReleasesSessionImmediately(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSyncAs(t, b, sessA, helloAs("0.3.1"))

	if _, err := b.Sync(context.Background(), sessA, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, connected := b.SessionInfo(); connected {
		t.Fatal("goodbye must release the holder")
	}
	// The next browser takes the session with no stale wait.
	msgs, _ := runSyncAs(t, b, sessB, helloAs("0.4.0"))
	if firstOfType(msgs, protocol.MsgHelloAck) == nil {
		t.Fatalf("post-goodbye hello must be granted, got %+v", msgs)
	}
}

func TestClaimSwitchesHolderAndReoffersHandoffs(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_arb_claim", handoffWork())
	runSyncAs(t, b, sessA, helloAs("0.3.1"))
	runSyncAs(t, b, sessB, helloAs("0.4.0"))

	if err := b.Claim("nope"); err == nil {
		t.Fatal("claiming an unknown session must error")
	}
	if err := b.Claim(sessB[:12]); err != nil { // prefix match
		t.Fatal(err)
	}
	msgs, _ := runSyncAs(t, b, sessB)
	if firstOfType(msgs, protocol.MsgHelloAck) == nil || firstOfType(msgs, protocol.MsgJobOffer) == nil {
		t.Fatalf("claimed session must receive hello_ack + re-offer, got %+v", msgs)
	}
	// The demoted holder stays pending and receives nothing.
	msgs, _ = runSyncAs(t, b, sessA)
	if len(msgs) != 0 {
		t.Fatalf("demoted holder poll = %+v, want empty", msgs)
	}
	sessions, _, _ := b.Sessions()
	if len(sessions) != 2 || sessions[0].ID != sessB || !sessions[0].Holder {
		t.Fatalf("sessions after claim = %+v", sessions)
	}
	_ = id
}

func TestLegacyHostsKeepLastHelloWins(t *testing.T) {
	b, _, _, _ := newBridge(t)
	if _, err := b.Sync(context.Background(), "", false, []json.RawMessage{helloAs("0.3.1")}); err != nil {
		t.Fatal(err)
	}
	if version, _, _ := b.SessionInfo(); version != "0.3.1" {
		t.Fatalf("legacy holder = %q", version)
	}
	// A second legacy hello replaces the first: the historical behavior.
	if _, err := b.Sync(context.Background(), "", false, []json.RawMessage{helloAs("0.4.0")}); err != nil {
		t.Fatal(err)
	}
	if version, _, _ := b.SessionInfo(); version != "0.4.0" {
		t.Fatalf("legacy last-hello-wins broken, holder = %q", version)
	}
	// A session-aware host immediately displaces a legacy holder.
	msgs, _ := runSyncAs(t, b, sessA, helloAs("0.5.0"))
	if firstOfType(msgs, protocol.MsgHelloAck) == nil {
		t.Fatalf("session-aware hello must displace legacy, got %+v", msgs)
	}
	if version, _, _ := b.SessionInfo(); version != "0.5.0" {
		t.Fatalf("holder after displacement = %q", version)
	}
}
