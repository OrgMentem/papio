// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"path/filepath"
	"testing"

	"papio/internal/protocol"
)

// A terminal grab must reach the extension exactly once per successful poll,
// while its durable row keeps answering status: a push the extension never
// observed (Sync discarded the reply, or the daemon restarted after marking)
// is recovered through reconciliation instead of being lost.
func TestPollDeliversTerminalGrabOnceAndKeepsStatusReplayable(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	b.svc.Validate = grabDOIValidate("10.1234/grab.notify.once")
	ctx := context.Background()
	runSync(t, b, hello())

	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Notify Once")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	pending, err := b.grabs.PendingNotifications(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != g.ID {
		t.Fatalf("pending = %v, %v; want the terminal grab", pending, err)
	}

	msgs, _ := runSync(t, b)
	got := firstOfType(msgs, protocol.MsgPdfGrabResult)
	if got == nil {
		t.Fatalf("no pdf_grab_result frame: %+v", msgs)
	}
	p := got.Payload.(*protocol.PdfGrabResultPayload)
	if p.GrabID != g.ID || p.Outcome != "job_created" {
		t.Fatalf("payload = %+v, want job_created for %s", p, g.ID)
	}

	pending, err = b.grabs.PendingNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after delivery = %d, want 0 (no duplicate push)", len(pending))
	}
	again, _ := runSync(t, b)
	if dup := firstOfType(again, protocol.MsgPdfGrabResult); dup != nil {
		t.Fatalf("duplicate pdf_grab_result on next poll: %+v", dup.Payload)
	}

	// The consumed notification stays replayable: the extension's restart
	// reconciliation queries exactly this status for a completed download
	// whose push never arrived.
	statusMsgs, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabStatusRequest, "", map[string]any{
		"request_id": "grab-status-replay-0001", "grab_id": g.ID,
	}))
	status := firstOfType(statusMsgs, protocol.MsgPdfGrabStatusResult)
	if status == nil {
		t.Fatalf("no pdf_grab_status_result frame: %+v", statusMsgs)
	}
	sp := status.Payload.(*protocol.PdfGrabStatusResultPayload)
	if sp.GrabID != g.ID || sp.State != "job_created" || sp.Outcome != "job_created" {
		t.Fatalf("status payload = %+v, want replayable job_created", sp)
	}
}

// Marking a terminal grab notified (a lost push) must not erase its outcome:
// Get and the status wire keep reporting it so reconciliation can deliver it
// exactly once after restart.
func TestTerminalGrabStatusSurvivesNotifiedMark(t *testing.T) {
	b, _, cfg, _ := newBridge(t)
	b.svc.Validate = grabDOIValidate("10.1234/grab.notify.lost")
	ctx := context.Background()
	runSync(t, b, hello())

	g, err := b.grabs.Allocate(ctx, "pdf.example.org", "Lost Push")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.EffectiveAdoptionRoot(), "grabs", g.ID)
	writeFixturePDF(t, filepath.Join(dir, "main.pdf"))
	if err := b.SweepGrabs(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// Simulate Sync discarding the reply (or a crash) after the mark: the
	// notification is consumed but the extension never observed it.
	if err := b.grabs.MarkNotified(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	got, err := b.grabs.Get(ctx, g.ID)
	if err != nil || got == nil {
		t.Fatalf("grab lookup: %v", err)
	}
	if got.State != "job_created" || got.Outcome != "job_created" {
		t.Fatalf("grab = %+v, want terminal job_created after notified mark", got)
	}

	statusMsgs, _ := runSync(t, b, inFrame(t, protocol.MsgPdfGrabStatusRequest, "", map[string]any{
		"request_id": "grab-status-lost-0001", "grab_id": g.ID,
	}))
	status := firstOfType(statusMsgs, protocol.MsgPdfGrabStatusResult)
	if status == nil {
		t.Fatalf("no pdf_grab_status_result frame: %+v", statusMsgs)
	}
	sp := status.Payload.(*protocol.PdfGrabStatusResultPayload)
	if sp.State != "job_created" || sp.Outcome != "job_created" {
		t.Fatalf("status payload = %+v, want terminal job_created for lost push", sp)
	}
}
