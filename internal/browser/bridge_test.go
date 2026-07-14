// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"papio/internal/app"
	"papio/internal/artifact"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/pdf"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/work"
)

func newBridge(t *testing.T) (*Bridge, *job.Store, config.Config, string) {
	t.Helper()
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	})
	artifacts, err := artifact.New(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccessMode = config.ModeMaximal
	cfg.DataDir = data
	cfg.Browser.ExtensionID = strings.Repeat("a", 32)
	cfg.Browser.OpenURLBase = "https://openurl.example.edu/resolve"
	cfg.Browser.ActionExpirySeconds = 1800
	jobs := &job.Store{S: db}
	svc := app.New(cfg, jobs, artifacts, nil)
	svc.Validate = func(context.Context, string, string, work.Work) (pdf.ValidationReport, error) {
		return pdf.ValidationReport{
			Payload:    pdf.PayloadReport{OK: true},
			Structural: pdf.StructuralReport{Valid: true, Pages: 3},
			Text:       pdf.TextReport{Chars: 4000},
			Identity:   pdf.IdentityDecision{Result: pdf.IdentityPass, Evidence: []string{"doi match"}},
		}, nil
	}
	return NewBridge(jobs, svc, cfg), jobs, cfg, data
}

func handoffWork() work.Work {
	return work.Work{DOI: "10.1002/example.42", Title: "An Institutional Paper", Authors: []string{"Lovelace, Ada"}, Year: 2024}
}

// park drives a fresh job into awaiting_human with an open openurl_handoff
// action, exactly as the app's exhaustion routing does.
func park(t *testing.T, jobs *job.Store, reqID string, w work.Work) string {
	t.Helper()
	ctx := context.Background()
	id, err := jobs.CreateRequest(ctx, reqID, w, "", "",
		job.Policy{AccessMode: config.ModeMaximal, DesiredVersion: "any", FetchMaxBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{
		{job.StateQueued, job.StateResolving},
		{job.StateResolving, job.StateFetching},
		{job.StateFetching, job.StateAwaitingHuman},
	} {
		if err := jobs.Transition(ctx, id, step[0], step[1], map[string]any{"reason": "institutional_handoff"}); err != nil {
			t.Fatalf("%s->%s: %v", step[0], step[1], err)
		}
	}
	if _, err := jobs.OpenHumanAction(ctx, id, handoffActionKind, "handoff available"); err != nil {
		t.Fatal(err)
	}
	return id
}

var inSeq int64

func inFrame(t *testing.T, typ, jobID string, payload any) json.RawMessage {
	t.Helper()
	inSeq++
	env := map[string]any{
		"protocol": protocol.BrowserProtocolVersion,
		"type":     typ,
		"msg_id":   "client-msg-0001",
		"seq":      inSeq,
		"payload":  payload,
	}
	if jobID != "" {
		env["job_id"] = jobID
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// sync runs one Sync batch and decodes every outbound frame (asserting each is a
// valid papio-browser frame), returning the decoded messages and their raw bytes.
func runSync(t *testing.T, b *Bridge, frames ...json.RawMessage) ([]*protocol.BrowserMessage, []json.RawMessage) {
	t.Helper()
	out, err := b.Sync(context.Background(), frames)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	msgs := make([]*protocol.BrowserMessage, 0, len(out))
	for _, raw := range out {
		m, err := protocol.DecodeBrowserMessage(raw)
		if err != nil {
			t.Fatalf("outbound frame failed protocol decode: %v", err)
		}
		msgs = append(msgs, m)
	}
	return msgs, out
}

func hello() json.RawMessage {
	return json.RawMessage(`{"protocol":"papio-browser/1","type":"hello","msg_id":"client-hello-1","seq":0,"payload":{"extension_version":"1.2.3"}}`)
}

func firstOfType(msgs []*protocol.BrowserMessage, typ string) *protocol.BrowserMessage {
	for _, m := range msgs {
		if m.Type == typ {
			return m
		}
	}
	return nil
}

func countType(msgs []*protocol.BrowserMessage, typ string) int {
	n := 0
	for _, m := range msgs {
		if m.Type == typ {
			n++
		}
	}
	return n
}

func TestHelloIsAcknowledged(t *testing.T) {
	b, _, _, _ := newBridge(t)
	msgs, _ := runSync(t, b, hello())
	if firstOfType(msgs, protocol.MsgHelloAck) == nil {
		t.Fatalf("no hello_ack in %v", msgs)
	}
	if msgs[0].Seq != 1 {
		t.Fatalf("first outbound seq = %d, want 1", msgs[0].Seq)
	}
}

func TestFrameBeforeHelloIsProtocolError(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_prehello", handoffWork())
	if _, err := b.Sync(context.Background(), []json.RawMessage{inFrame(t, protocol.MsgJobAccept, id, map[string]any{})}); err == nil {
		t.Fatal("expected error for frame before hello")
	}
}

func TestHandoffJobOfferedExactlyOncePerHelloSession(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	id := park(t, jobs, "wr_offer", handoffWork())

	msgs, _ := runSync(t, b, hello())
	if got := countType(msgs, protocol.MsgJobOffer); got != 1 {
		t.Fatalf("job_offer count on hello = %d, want 1", got)
	}
	offer := firstOfType(msgs, protocol.MsgJobOffer)
	if offer.JobID != id {
		t.Fatalf("offer job_id = %s, want %s", offer.JobID, id)
	}
	p := offer.Payload.(*protocol.JobOfferPayload)

	// KEV encoding: url_ver + rft_id=info:doi/<doi>, URL-escaped.
	u, err := url.Parse(p.OpenURL)
	if err != nil {
		t.Fatalf("openurl parse: %v", err)
	}
	q := u.Query()
	if q.Get("url_ver") != "Z39.88-2004" {
		t.Fatalf("url_ver = %q", q.Get("url_ver"))
	}
	if q.Get("rft_id") != "info:doi/10.1002/example.42" {
		t.Fatalf("rft_id = %q", q.Get("rft_id"))
	}
	if !strings.Contains(p.OpenURL, "info%3Adoi%2F10.1002%2Fexample.42") {
		t.Fatalf("openurl not URL-escaped: %s", p.OpenURL)
	}
	if p.AccessMode != cfg.AccessMode {
		t.Fatalf("access_mode = %q", p.AccessMode)
	}
	if !slices.Contains(p.ProviderHosts, "springer.com") {
		t.Fatalf("provider_hosts = %v, missing springer.com", p.ProviderHosts)
	}
	if _, err := time.Parse(time.RFC3339, p.ExpiresAt); err != nil {
		t.Fatalf("expires_at not RFC3339: %q (%v)", p.ExpiresAt, err)
	}
	if p.Expected == nil || p.Expected.DOI != "10.1002/example.42" {
		t.Fatalf("expected hints = %+v", p.Expected)
	}

	// A subsequent poll in the same hello-session must not re-offer.
	msgs2, _ := runSync(t, b)
	if got := countType(msgs2, protocol.MsgJobOffer); got != 0 {
		t.Fatalf("re-offered %d times in same hello session", got)
	}

	// A new hello (service-worker restart) resets the session and re-offers.
	msgs3, _ := runSync(t, b, hello())
	if got := countType(msgs3, protocol.MsgJobOffer); got != 1 {
		t.Fatalf("job_offer after new hello = %d, want 1", got)
	}
}

func TestSentinelSecretNeverEntersMessagesOrDurableRows(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	id := park(t, jobs, "wr_sentinel", handoffWork())
	const sentinel = "SENTINEL_IDP_SECRET_ac9f"

	helloMsgs, helloRaw := runSync(t, b, hello())

	// A client attempt to smuggle an IdP URL as an extra field on an auth frame
	// fails the strict decode and stores nothing.
	bad := json.RawMessage(`{"protocol":"papio-browser/1","type":"auth_returned","msg_id":"client-msg-9","seq":9,"job_id":"` + id +
		`","payload":{"elapsed_ms":10,"idp_url":"https://idp.example/saml?token=` + sentinel + `"}}`)
	if _, err := b.Sync(context.Background(), []json.RawMessage{bad}); err == nil {
		t.Fatal("smuggled idp_url field must be rejected")
	}

	// Legitimate timing-only auth frames carry no address.
	authMsgs, authRaw := runSync(t, b,
		inFrame(t, protocol.MsgAuthPending, id, map[string]any{"elapsed_ms": 5}),
		inFrame(t, protocol.MsgAuthReturned, id, map[string]any{"elapsed_ms": 900}),
	)

	// Scan every outbound frame across the session.
	var outbound strings.Builder
	for _, raw := range append(append([]json.RawMessage{}, helloRaw...), authRaw...) {
		outbound.Write(raw)
	}
	_ = helloMsgs
	_ = authMsgs
	if strings.Contains(outbound.String(), sentinel) {
		t.Fatal("sentinel leaked into an outbound frame")
	}

	// Scan every durable row that could conceivably hold text.
	db := jobs.S.DB()
	var dump strings.Builder
	for _, q := range []string{
		`SELECT COALESCE(detail_json,'') FROM events`,
		`SELECT COALESCE(detail,'') FROM human_actions`,
		`SELECT COALESCE(kind,'')||COALESCE(status,'') FROM human_actions`,
		`SELECT COALESCE(url_redacted,'')||COALESCE(landing_redacted,'') FROM candidates`,
		`SELECT COALESCE(terminal_reason,'') FROM jobs`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			dump.WriteString(s)
		}
		_ = rows.Close()
	}
	if strings.Contains(dump.String(), sentinel) {
		t.Fatal("sentinel leaked into a durable row")
	}
	// Sanity: the timing events were actually recorded (elapsed only).
	events, _ := jobs.Events(context.Background(), id)
	encoded, _ := json.Marshal(events)
	if !strings.Contains(string(encoded), "auth_returned") || !strings.Contains(string(encoded), "elapsed_ms") {
		t.Fatalf("timing-only auth events missing: %s", encoded)
	}
}

func writeFixturePDF(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := append([]byte("%PDF-1.4\nadopted\n"), make([]byte, 512)...)
	body = append(body, []byte("\n%%EOF")...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadCompleteRejectsTraversalAndAdoptsValidFile(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	id := park(t, jobs, "wr_dl", handoffWork())
	runSync(t, b, hello())

	// Path-separated filenames fail the protocol decode before any adoption.
	for _, bad := range []string{"../evil.pdf", "/etc/passwd.pdf"} {
		frame := inFrame(t, protocol.MsgDownloadComplete, id,
			map[string]any{"download_id": 1, "filename": bad, "size_bytes": 100})
		if _, err := b.Sync(context.Background(), []json.RawMessage{frame}); err == nil {
			t.Fatalf("filename %q must be rejected", bad)
		}
	}

	// A valid download under adoptions/<job>/ is adopted and reaches ready.
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))
	msgs, _ := runSync(t, b, inFrame(t, protocol.MsgDownloadComplete, id,
		map[string]any{"download_id": 7, "filename": "paper.pdf", "size_bytes": 533}))
	if firstOfType(msgs, protocol.MsgAck) == nil {
		t.Fatalf("no ack for download_complete: %v", msgs)
	}
	row, err := jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("adopted job not ready: %+v", row)
	}
	if err := b.svc.Artifacts.Verify(row.ArtifactSHA256); err != nil {
		t.Fatalf("artifact verify: %v", err)
	}
}

func TestDownloadForUnrelatedJobDoesNotAdoptAnotherJobsFile(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	target := park(t, jobs, "wr_target", handoffWork())

	// A different job, also parked, but with no download of its own.
	other := park(t, jobs, "wr_other", handoffWork())
	runSync(t, b, hello())
	// Place target's file: the poll-time scan may legitimately adopt it for
	// TARGET (its own directory) — never for `other`.
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), target, "paper.pdf"))

	// A download_complete correlated to `other` must not adopt target's file: it
	// only ever looks under adoptions/<other>/, which is empty. The miss is
	// non-fatal: the bridge acks, records a deferral, and keeps `other` parked.
	frame := inFrame(t, protocol.MsgDownloadComplete, other,
		map[string]any{"download_id": 3, "filename": "paper.pdf", "size_bytes": 533})
	msgs, _ := runSync(t, b, frame)
	if firstOfType(msgs, protocol.MsgAck) == nil {
		t.Fatalf("expected ack after deferred adoption: %v", msgs)
	}
	oRow, _ := jobs.Get(ctx, other)
	if oRow.State != job.StateAwaitingHuman || oRow.ArtifactSHA256 != "" {
		t.Fatalf("other job must stay parked: %+v", oRow)
	}
	events, _ := jobs.Events(ctx, other)
	deferred := false
	for _, e := range events {
		if e["kind"] == "browser.adoption_deferred" {
			deferred = true
		}
	}
	if !deferred {
		t.Fatal("missing browser.adoption_deferred event")
	}
	tRow, _ := jobs.Get(ctx, target)
	if tRow.State == job.StateReady && tRow.ArtifactSHA256 == "" {
		t.Fatalf("target adopted without artifact: %+v", tRow)
	}
	if tRow.State != job.StateReady && tRow.State != job.StateAwaitingHuman {
		t.Fatalf("target in unexpected state: %+v", tRow)
	}
}

func TestPollScanAdoptsSingleSettledFileAndDefersAmbiguity(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	single := park(t, jobs, "wr_scan_single", handoffWork())
	ambig := park(t, jobs, "wr_scan_ambig", handoffWork())
	partial := park(t, jobs, "wr_scan_partial", handoffWork())
	root := cfg.EffectiveAdoptionRoot()
	writeFixturePDF(t, filepath.Join(root, single, "paper.pdf"))
	if err := os.WriteFile(filepath.Join(root, single, ".DS_Store"), []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFixturePDF(t, filepath.Join(root, ambig, "a.pdf"))
	writeFixturePDF(t, filepath.Join(root, ambig, "b.pdf"))
	writeFixturePDF(t, filepath.Join(root, partial, "c.pdf"))
	if err := os.WriteFile(filepath.Join(root, partial, "c.pdf.crdownload"), []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}

	runSync(t, b, hello())
	if _, err := b.Sync(ctx, nil); err != nil { // poll triggers the scan
		t.Fatal(err)
	}

	sRow, _ := jobs.Get(ctx, single)
	if sRow.State != job.StateReady || sRow.ArtifactSHA256 == "" {
		t.Fatalf("single settled file not adopted: %+v", sRow)
	}
	aRow, _ := jobs.Get(ctx, ambig)
	if aRow.State != job.StateAwaitingHuman {
		t.Fatalf("ambiguous dir must stay with the user: %+v", aRow)
	}
	pRow, _ := jobs.Get(ctx, partial)
	if pRow.State != job.StateAwaitingHuman {
		t.Fatalf("in-progress .crdownload must defer the scan: %+v", pRow)
	}
}

func TestProviderOutcomeMappings(t *testing.T) {
	type expect struct {
		state        string
		actionStatus string // status the openurl_handoff action should end in
		extraAction  string // additional open action kind expected (human_auth/terms)
		terminal     string
	}
	cases := map[string]expect{
		"cancelled":                   {state: job.StateCancelled, actionStatus: "cancelled"},
		"no_entitlement":              {state: job.StateUnavailable, actionStatus: "resolved", terminal: "no_entitlement"},
		"document_delivery_available": {state: job.StateUnavailable, actionStatus: "resolved", terminal: "document_delivery_available"},
		"wrong_work":                  {state: job.StateNeedsReview, actionStatus: "resolved"},
		"ui_changed":                  {state: job.StateNeedsReview, actionStatus: "resolved"},
		"rate_limited":                {state: job.StateRetryWait, actionStatus: "resolved"},
		"human_auth_required":         {state: job.StateAwaitingHuman, actionStatus: "open", extraAction: "human_auth_required"},
		"terms_acceptance_required":   {state: job.StateAwaitingHuman, actionStatus: "open", extraAction: "terms_acceptance_required"},
	}
	for outcome, want := range cases {
		t.Run(outcome, func(t *testing.T) {
			b, jobs, _, _ := newBridge(t)
			ctx := context.Background()
			id := park(t, jobs, "wr_"+outcome, handoffWork())
			runSync(t, b, hello())
			runSync(t, b, inFrame(t, protocol.MsgProviderOutcome, id, map[string]any{"outcome": outcome}))

			row, err := jobs.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != want.state {
				t.Fatalf("state = %s, want %s", row.State, want.state)
			}
			if want.terminal != "" && row.TerminalReason != want.terminal {
				t.Fatalf("terminal reason = %q, want %q", row.TerminalReason, want.terminal)
			}
			if want.state == job.StateRetryWait && row.RetryAt == "" {
				t.Fatal("rate_limited did not schedule a retry_at")
			}

			actions, err := jobs.ListHumanActions(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			var handoffStatus string
			extraOpen := false
			for _, a := range actions {
				if a.Kind == handoffActionKind {
					handoffStatus = a.Status
				}
				if want.extraAction != "" && a.Kind == want.extraAction && a.Status == "open" {
					extraOpen = true
				}
			}
			if handoffStatus != want.actionStatus {
				t.Fatalf("openurl_handoff status = %q, want %q", handoffStatus, want.actionStatus)
			}
			if want.extraAction != "" && !extraOpen {
				t.Fatalf("expected an open %s action", want.extraAction)
			}
		})
	}
}

func TestJobRejectEndsHandoffUnavailable(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_reject", handoffWork())
	runSync(t, b, hello())
	runSync(t, b, inFrame(t, protocol.MsgJobReject, id, map[string]any{}))

	row, _ := jobs.Get(ctx, id)
	if row.State != job.StateUnavailable || row.TerminalReason != "browser_rejected" {
		t.Fatalf("rejected job = %+v", row)
	}
	actions, _ := jobs.ListHumanActions(ctx, false)
	for _, a := range actions {
		if a.Kind == handoffActionKind && a.Status == "open" {
			t.Fatal("handoff action still open after reject")
		}
	}
}

func TestDaemonSideCancelEmitsCancelFrameOnce(t *testing.T) {
	b, jobs, _, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_dcancel", handoffWork())
	runSync(t, b, hello()) // offers the job

	if err := jobs.Cancel(ctx, id, "user request"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := runSync(t, b)
	c := firstOfType(msgs, protocol.MsgCancel)
	if c == nil || c.JobID != id {
		t.Fatalf("expected a cancel frame for %s, got %v", id, msgs)
	}
	// Not repeated on the next poll.
	msgs2, _ := runSync(t, b)
	if firstOfType(msgs2, protocol.MsgCancel) != nil {
		t.Fatal("cancel frame emitted more than once")
	}
}

func TestOpenURLPMIDFallbackAndYear(t *testing.T) {
	got := OpenURL("https://openurl.example.edu/resolve", work.Work{PMID: "123456", Title: "T", Year: 2020})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("rft_id") != "info:pmid/123456" {
		t.Fatalf("rft_id = %q", q.Get("rft_id"))
	}
	if q.Get("rft.date") != "2020" {
		t.Fatalf("rft.date = %q", q.Get("rft.date"))
	}
}

// SweepAdoptions adopts a settled file WITHOUT any hello/extension connection —
// the daemon owns completion; the browser plane is only a delivery hint.
func TestSweepAdoptionsAdoptsWithoutHello(t *testing.T) {
	b, jobs, cfg, _ := newBridge(t)
	ctx := context.Background()
	id := park(t, jobs, "wr_sweep", handoffWork())
	writeFixturePDF(t, filepath.Join(cfg.EffectiveAdoptionRoot(), id, "paper.pdf"))

	// No hello was ever sent; poll() would offer nothing. The sweeper still adopts.
	if err := b.SweepAdoptions(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	row, _ := jobs.Get(ctx, id)
	if row.State != job.StateReady || row.ArtifactSHA256 == "" {
		t.Fatalf("sweeper did not adopt: %+v", row)
	}
}
