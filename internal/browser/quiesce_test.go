// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
)

// An open handoff used to be re-offered on every session-live tick for as long
// as it stayed open. One that nobody could complete produced roughly sixty tabs
// over three days. job.QuiesceAfter bounds that: papio stops volunteering, the
// action stays open, and an explicit `papio actions open` still drives it.

// backdateAction ages an open action past the quiesce window without touching
// the bridge clock, so a fresh sibling in the same test is a live control.
func backdateAction(t *testing.T, jobs *job.Store, jobID string, age time.Duration) {
	t.Helper()
	if _, err := jobs.S.DB().ExecContext(context.Background(),
		`UPDATE human_actions SET created_at = ? WHERE job_id = ? AND status = 'open'`,
		time.Now().UTC().Add(-age).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatalf("backdate action for %s: %v", jobID, err)
	}
}

func offeredJobIDs(msgs []*protocol.BrowserMessage) map[string]bool {
	out := map[string]bool{}
	for _, m := range msgs {
		if m.Type == protocol.MsgJobOffer {
			out[m.JobID] = true
		}
	}
	return out
}

func TestQuiescedHandoffIsNotAutoOfferedButAFreshOneIs(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	stale := park(t, jobs, "wr_quiesce_stale", handoffWork())
	fresh := park(t, jobs, "wr_quiesce_fresh", handoffWork())
	backdateAction(t, jobs, stale, job.QuiesceAfter+time.Hour)

	offered := offeredJobIDs(mustSync(t, b, hello()))
	if offered[stale] {
		t.Fatalf("a handoff open past %s was auto-offered; that is the tab loop", job.QuiesceAfter)
	}
	if !offered[fresh] {
		t.Fatalf("fresh handoff was not offered: %v — the window must not swallow live work", offered)
	}
}

func TestQuiesceBoundaryIsTheWindowItself(t *testing.T) {
	// Just inside the window is still ordinary work. The off-by-one matters:
	// getting it wrong by an hour silently shortens the window for everyone.
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_quiesce_boundary", handoffWork())
	backdateAction(t, jobs, id, job.QuiesceAfter-time.Hour)

	if !offeredJobIDs(mustSync(t, b, hello()))[id] {
		t.Fatal("handoff one hour short of the window was withheld")
	}
}

func TestExplicitOpenDrivesAQuiescedHandoff(t *testing.T) {
	// The escape hatch, and the reason going quiet is not expiry. `papio
	// actions open` is user intent; a session-live tick is not.
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_quiesce_focus", handoffWork())
	backdateAction(t, jobs, id, job.QuiesceAfter+time.Hour)

	if offeredJobIDs(mustSync(t, b, helloAs(HandoffFocusMinExtensionVersion)))[id] {
		t.Fatal("precondition: the quiesced handoff should not have been auto-offered")
	}
	queued, sessionLive, err := b.FocusHandoffs(context.Background(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if !sessionLive || queued != 1 {
		t.Fatalf("focus result = queued:%d live:%t, want 1,true — a quiesced action is still openable", queued, sessionLive)
	}
	if !offeredJobIDs(mustSync(t, b))[id] {
		t.Fatal("explicit open did not drive the quiesced handoff")
	}
}

func TestFreshLoginDoesNotResurrectAQuiescedSibling(t *testing.T) {
	// The specific mechanism behind the reported tab storm: one institutional
	// sign-in released every parked sibling on that profile, however old.
	b, jobs, _, _ := newBridge(t)
	b.cfg.Browser.Resolvers = map[string]config.Institution{
		"alpha": {OpenURLBase: "https://alpha.example.edu/openurl"},
	}
	source := parkInstitutional(t, jobs, "wr_quiesce_source", handoffWork(), "alpha")
	live := parkInstitutional(t, jobs, "wr_quiesce_live_sibling", handoffWork(), "alpha")
	stale := parkInstitutional(t, jobs, "wr_quiesce_stale_sibling", handoffWork(), "alpha")
	backdateAction(t, jobs, stale, job.QuiesceAfter+time.Hour)

	mustSync(t, b, hello())
	b.mu.Lock()
	b.offered = map[string]bool{source: true}
	b.cancelSent = map[string]bool{}
	b.reofferPending = map[string]bool{}
	b.reofferSourceJobID = ""
	b.reofferProfile = ""
	b.mu.Unlock()

	mustSync(t, b, inFrame(t, protocol.MsgSessionEvidence, "", map[string]any{
		"evidence":    "warm_verified",
		"origin_hint": "https://alpha.example.edu",
		"at":          "2026-08-03T12:00:00Z",
	}))

	if !reoffered(t, jobs, live) {
		t.Fatal("the live sibling was not released; the reoffer path itself is broken")
	}
	if reoffered(t, jobs, stale) {
		t.Fatalf("a sibling open past %s rode someone else's login", job.QuiesceAfter)
	}
}

func reoffered(t *testing.T, jobs *job.Store, jobID string) bool {
	t.Helper()
	events, err := jobs.Events(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event["kind"] == "browser.handoff_reoffered" {
			return true
		}
	}
	return false
}

func mustSync(t *testing.T, b *Bridge, frames ...json.RawMessage) []*protocol.BrowserMessage {
	t.Helper()
	msgs, _ := runSync(t, b, frames...)
	return msgs
}
