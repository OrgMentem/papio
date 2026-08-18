// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Multi-browser session arbitration: exactly one session holds the
// offer/handoff flow; competitors wait as pending instead of silently
// stealing it (the two-browser fight from the 2026-07-20 field report).

package browser

import (
	"context"
	"encoding/json"
	"slices"
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

	msgs, _ := runSyncAs(t, b, sessA, helloAs("0.6.0"))
	if firstOfType(msgs, protocol.MsgHelloAck) == nil || firstOfType(msgs, protocol.MsgJobOffer) == nil {
		t.Fatalf("holder must receive hello_ack + job_offer, got %+v", msgs)
	}

	msgs, _ = runSyncAs(t, b, sessB, helloAs("0.5.1"))
	busy := firstOfType(msgs, protocol.MsgError)
	if busy == nil || busy.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("second browser must be denied with session_busy, got %+v", msgs)
	}
	if firstOfType(msgs, protocol.MsgJobOffer) != nil {
		t.Fatal("denied session must not receive offers")
	}

	// The holder's identity is stable: no version flap.
	version, _, connected := b.SessionInfo()
	if !connected || version != "0.6.0" {
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

// TestHelloAckNamesTheGrantedRole pins the two-frame denial. A denied hello
// used to get session_busy and nothing else, so that browser learned no
// daemon features and locally refused even the holder-independent surfaces
// the dispatcher already admits from it — a PDF grab among them. It now hears
// what the daemon can do first, then that it is not the holder.
func TestHelloAckNamesTheGrantedRole(t *testing.T) {
	b, _, _, _ := newBridge(t)

	msgs, _ := runSyncAs(t, b, sessA, helloAs("0.14.0"))
	ack := firstOfType(msgs, protocol.MsgHelloAck)
	if ack == nil || ack.Payload.(*protocol.HelloAckPayload).Role != "holder" {
		t.Fatalf("granted hello = %+v, want a hello_ack with role holder", msgs)
	}

	msgs, _ = runSyncAs(t, b, sessB, helloAs("0.14.0"))
	if len(msgs) != 2 {
		t.Fatalf("denied hello returned %d frames, want the ack and the refusal: %+v", len(msgs), msgs)
	}
	if msgs[0].Type != protocol.MsgHelloAck {
		t.Fatalf("first frame = %q, want the ack ahead of the refusal", msgs[0].Type)
	}
	pending := msgs[0].Payload.(*protocol.HelloAckPayload)
	if pending.Role != "pending" {
		t.Fatalf("denied hello role = %q, want pending", pending.Role)
	}
	if !slices.Contains(pending.Features, pdfGrabV1Feature) {
		t.Fatalf("pending ack features = %v, want the daemon's full capability list", pending.Features)
	}
	if msgs[1].Type != protocol.MsgError ||
		msgs[1].Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("second frame = %+v, want the session_busy refusal", msgs[1])
	}

	if _, denied, _ := b.Sessions(); denied != 1 {
		t.Fatalf("denied hellos = %d, want the denial still counted", denied)
	}
}

func TestNonHolderStatelessFramesPassAndHandoffFramesBlock(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_arb_frames", handoffWork())
	runSyncAs(t, b, sessA, helloAs("0.6.0"))
	runSyncAs(t, b, sessB, helloAs("0.5.1"))

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

// TestNonHolderHumanInitiatedFramesAreAdmitted pins the six inbox frames that
// used to come back session_busy from a second browser. The refusal was
// invisible while a denied hello carried no features — the extension learned
// no daemon capabilities, so it never put these frames on the wire — and
// became a click-time failure the moment a pending session started receiving
// a real hello_ack with a "pending" role.
//
// Each frame qualifies on both halves of the admission rule. None opens,
// focuses, groups or closes a tab; none emits an offer, handoff, cancel or
// focus frame; none allocates or holds an effect permit; none starts a
// provider drive, direct get, terms acceptance or institutional navigation.
// And none claims bridge-routed authority: no handler takes the session ID,
// depends on being the session daemon-initiated work routes to, or touches
// holder-scoped bridge state. What they do mutate is the daemon's own
// records — a watch digest, a human action, a delivery request — which is a
// human decision about daemon state, and the browser the human clicked in is
// the right browser by construction.
//
// The assertion is the type-specific result frame, not merely the absence of
// session_busy: the arbitration gate returns an error frame instead of the
// handler's answer, so the answer's presence is what proves dispatch reached
// the handler.
func TestNonHolderHumanInitiatedFramesAreAdmitted(t *testing.T) {
	cases := []struct {
		name  string
		want  string
		frame func(t *testing.T, jobID string) json.RawMessage
	}{
		{"stats_request", protocol.MsgStatsResponse, func(t *testing.T, _ string) json.RawMessage {
			return inFrame(t, protocol.MsgStatsRequest, "", protocol.StatsRequestPayload{
				RequestID: "arb-stats-0001",
			})
		}},
		{"activity_request", protocol.MsgActivityResponse, func(t *testing.T, _ string) json.RawMessage {
			return inFrame(t, protocol.MsgActivityRequest, "", protocol.ActivityRequestPayload{
				RequestID: "arb-activity-0001", Limit: 1,
			})
		}},
		{"triage_decide", protocol.MsgTriageDecideResult, func(t *testing.T, _ string) json.RawMessage {
			return inFrame(t, protocol.MsgTriageDecide, "", protocol.TriageDecidePayload{
				RequestID: "arb-dismiss-0001", ItemID: "arb-watch-hit-absent", Op: "dismiss",
				WatchScope: json.RawMessage(`"all"`),
			})
		}},
		{"human_action_resolve", protocol.MsgHumanActionResolveResult, func(t *testing.T, _ string) json.RawMessage {
			return inFrame(t, protocol.MsgHumanActionResolve, "", protocol.HumanActionResolvePayload{
				RequestID: "arb-resolve-0001", ActionID: 999999, Verdict: "dismiss", ExpectedRevision: 1,
			})
		}},
		{"delivery_reconcile_request", protocol.MsgDeliveryReconcileResult, func(t *testing.T, jobID string) json.RawMessage {
			return inFrame(t, protocol.MsgDeliveryReconcileRequest, "", protocol.DeliveryReconcilePayload{
				RequestID: "arb-delivery-0001", JobID: jobID, Operation: "confirm_request_absent",
			})
		}},
		{"review_preview_request", protocol.MsgReviewPreviewResult, func(t *testing.T, _ string) json.RawMessage {
			return inFrame(t, protocol.MsgReviewPreviewRequest, "", protocol.ReviewPreviewRequestPayload{
				RequestID: "arb-preview-0001", ActionID: 999999,
			})
		}},
		// The inbox is a plain extension page and is routinely open in a
		// session that does not hold the bridge, so its ranked candidate
		// picker has to be served to a non-holder or the feature simply does
		// not work in the browser the operator is actually using. Both frames
		// name an absent grab on purpose: what is under test is that a routine
		// failure comes back as a structured response rather than as an error
		// frame or a session teardown, which is the disposition AGENTS.md
		// records reviewPreview getting wrong.
		{"pdf_grab_suggest_request", protocol.MsgPdfGrabSuggestResponse, func(t *testing.T, _ string) json.RawMessage {
			return inFrame(t, protocol.MsgPdfGrabSuggestRequest, "", protocol.PdfGrabSuggestRequestPayload{
				RequestID: "arb-suggest-0001", GrabID: "grab_00000000000000000000000000", Limit: 5,
			})
		}},
		{"pdf_grab_confirm_request", protocol.MsgPdfGrabConfirmResponse, func(t *testing.T, jobID string) json.RawMessage {
			return inFrame(t, protocol.MsgPdfGrabConfirmRequest, "", protocol.PdfGrabConfirmRequestPayload{
				RequestID: "arb-confirm-0001", GrabID: "grab_00000000000000000000000000", JobID: jobID,
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			jobID := park(t, jobs, "wr_arb_admitted", handoffWork())
			runSyncAs(t, b, sessA, helloAs("1.2.3"))
			runSyncAs(t, b, sessB, helloAs("1.2.3"))

			msgs, _ := runSyncAs(t, b, sessB, tc.frame(t, jobID))
			if errMsg := firstOfType(msgs, protocol.MsgError); errMsg != nil &&
				errMsg.Payload.(*protocol.ErrorPayload).Code == "session_busy" {
				t.Fatalf("%s from a non-holder was refused: %v", tc.name, msgs)
			}
			if firstOfType(msgs, tc.want) == nil {
				t.Fatalf("%s from a non-holder produced no %s: %v", tc.name, tc.want, msgs)
			}
			// Serving a non-holder must not hand it the session slot either.
			if b.holder == nil || b.holder.ID != sessA {
				t.Fatalf("holder = %+v, want the session slot left with %s", b.holder, sessA)
			}
		})
	}
}

// TestNonHolderRoutedAuthorityFramesStayRefused pins the other direction of
// the same boundary. Widening the whitelist for human-initiated frames must
// not widen it into the offer/handoff flow: acting on a routed offer from a
// browser the daemon did not route it to is the silent session fight the
// arbitration exists to prevent, and handoff_link_request mints the link that
// flow navigates with.
func TestNonHolderRoutedAuthorityFramesStayRefused(t *testing.T) {
	cases := []struct {
		name  string
		frame func(t *testing.T, jobID string) json.RawMessage
	}{
		{"job_accept", func(t *testing.T, jobID string) json.RawMessage {
			return inFrame(t, protocol.MsgJobAccept, jobID, map[string]any{})
		}},
		{"job_reject", func(t *testing.T, jobID string) json.RawMessage {
			return inFrame(t, protocol.MsgJobReject, jobID, map[string]any{})
		}},
		{"handoff_link_request", func(t *testing.T, jobID string) json.RawMessage {
			return inFrame(t, protocol.MsgHandoffLinkRequest, "", protocol.HandoffLinkRequestPayload{
				JobID: jobID, RequestID: "arb-link-0001",
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			jobID := park(t, jobs, "wr_arb_refused", handoffWork())
			runSyncAs(t, b, sessA, helloAs("1.2.3"))
			runSyncAs(t, b, sessB, helloAs("1.2.3"))

			msgs, _ := runSyncAs(t, b, sessB, tc.frame(t, jobID))
			errMsg := firstOfType(msgs, protocol.MsgError)
			if errMsg == nil || errMsg.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
				t.Fatalf("%s from a non-holder = %v, want session_busy", tc.name, msgs)
			}
		})
	}
}

func TestStaleHolderYieldsToLiveSession(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	advance := settableClock(b)
	id := park(t, jobs, "wr_arb_stale", handoffWork())

	runSyncAs(t, b, sessA, helloAs("0.5.1"))
	runSyncAs(t, b, sessB, helloAs("0.6.0")) // denied, pending

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
	if version != "0.6.0" {
		t.Fatalf("holder after takeover = %q, want 0.4.0", version)
	}
	if _, _, takeovers := b.Sessions(); takeovers != 1 {
		t.Fatalf("takeovers = %d, want 1", takeovers)
	}
}

func TestGoodbyeReleasesSessionImmediately(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSyncAs(t, b, sessA, helloAs("0.5.1"))

	if _, err := b.Sync(context.Background(), sessA, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, connected := b.SessionInfo(); connected {
		t.Fatal("goodbye must release the holder")
	}
	// The next browser takes the session with no stale wait.
	msgs, _ := runSyncAs(t, b, sessB, helloAs("0.6.0"))
	if firstOfType(msgs, protocol.MsgHelloAck) == nil {
		t.Fatalf("post-goodbye hello must be granted, got %+v", msgs)
	}
}

func TestClaimSwitchesHolderAndReoffersHandoffs(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_arb_claim", handoffWork())
	runSyncAs(t, b, sessA, helloAs("0.5.1"))
	runSyncAs(t, b, sessB, helloAs("0.6.0"))

	if _, err := b.Claim("nope"); err == nil {
		t.Fatal("claiming an unknown session must error")
	}
	resolved, err := b.Claim(sessB[:12]) // prefix match
	if err != nil {
		t.Fatal(err)
	}
	if resolved != sessB {
		t.Fatalf("resolved claim id = %q, want full %q", resolved, sessB)
	}
	msgs, _ := runSyncAs(t, b, sessB)
	if firstOfType(msgs, protocol.MsgHelloAck) == nil || firstOfType(msgs, protocol.MsgJobOffer) == nil {
		t.Fatalf("claimed session must receive hello_ack + re-offer, got %+v", msgs)
	}
	// The demoted holder is told once — silence let its extension keep
	// reporting a live papio connection it no longer had — then goes quiet.
	msgs, _ = runSyncAs(t, b, sessA)
	busy := firstOfType(msgs, protocol.MsgError)
	if len(msgs) != 1 || busy == nil || busy.Payload.(*protocol.ErrorPayload).Code != "session_busy" {
		t.Fatalf("demoted holder poll = %+v, want one session_busy", msgs)
	}
	if msgs, _ = runSyncAs(t, b, sessA); len(msgs) != 0 {
		t.Fatalf("second demoted holder poll = %+v, want empty", msgs)
	}
	sessions, _, _ := b.Sessions()
	if len(sessions) != 2 || sessions[0].ID != sessB || !sessions[0].Holder {
		t.Fatalf("sessions after claim = %+v", sessions)
	}
	_ = id
}

func TestClaimPrefixMatchingHolderAndPendingIsAmbiguous(t *testing.T) {
	b, _, _, _ := newBridge(t)
	// Both ids share the "aaaa" prefix; sessA holds, the other pends.
	similar := "aaaa9999aaaa9999aaaa9999aaaa9999"
	runSyncAs(t, b, sessA, helloAs("0.6.0"))
	runSyncAs(t, b, similar, helloAs("0.5.1"))

	if _, err := b.Claim("aaaa"); err == nil {
		t.Fatal("prefix matching holder AND a pending session must be ambiguous, not a silent no-op")
	}
	if resolved, err := b.Claim(sessA); err != nil || resolved != sessA {
		t.Fatalf("exact holder claim = %q, %v; want no-op success", resolved, err)
	}
	version, _, _ := b.SessionInfo()
	if version != "0.6.0" {
		t.Fatalf("holder changed by ambiguous/no-op claims: %q", version)
	}
}

func TestPromotedOutdatedSessionIsToldNotSilentlySeated(t *testing.T) {
	b, _, _, _ := newBridge(t)
	advance := settableClock(b)
	runSyncAs(t, b, sessA, helloAs("0.6.0"))
	runSyncAs(t, b, sessB, helloAs("0.0.1")) // below MinExtensionVersion, denied+pending

	advance(sessionStaleAfter + time.Second)
	msgs, _ := runSyncAs(t, b, sessB) // stale takeover promotes the outdated session
	outdated := firstOfType(msgs, protocol.MsgError)
	if outdated == nil || outdated.Payload.(*protocol.ErrorPayload).Code != "extension_outdated" {
		t.Fatalf("promoted outdated session must be told extension_outdated, got %+v", msgs)
	}
}

func TestOutdatedPendingSessionStillPassesStatelessFrames(t *testing.T) {
	b, _, _, _ := newBridge(t)
	runSyncAs(t, b, sessA, helloAs("0.6.0"))
	runSyncAs(t, b, sessB, helloAs("0.0.1"))

	msgs, _ := runSyncAs(t, b, sessB, inFrame(t, protocol.MsgPageAcquire, "",
		map[string]any{"url": "https://journals.example.test/outdated-pending", "doi": "10.1234/outdated-pending"}))
	if firstOfType(msgs, protocol.MsgPageAcquireAck) == nil {
		t.Fatalf("stateless page_acquire from an outdated pending session must pass, got %+v", msgs)
	}
}

func TestLegacyHostsKeepLastHelloWins(t *testing.T) {
	b, _, _, _ := newBridge(t)
	if _, err := b.Sync(context.Background(), "", false, []json.RawMessage{helloAs("0.5.1")}); err != nil {
		t.Fatal(err)
	}
	if version, _, _ := b.SessionInfo(); version != "0.5.1" {
		t.Fatalf("legacy holder = %q", version)
	}
	// A second legacy hello replaces the first: the historical behavior.
	if _, err := b.Sync(context.Background(), "", false, []json.RawMessage{helloAs("0.6.0")}); err != nil {
		t.Fatal(err)
	}
	if version, _, _ := b.SessionInfo(); version != "0.6.0" {
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
