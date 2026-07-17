// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package browser implements the daemon side of the Phase 2 ordinary-Chrome
// institutional handoff. The bridge speaks locked papio-browser/1 as a
// pull loop over daemon-owned local IPC: the extension delivers observation
// frames and the bridge returns command frames. Browser messages are strictly
// re-validated here (fail closed) and treated as observations only — they never
// authorize a job transition the core policy would forbid. PDF bytes and secrets
// never cross this boundary; only metadata, timing, and local paths do.
package browser

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/store"
)

const handoffActionKind = "openurl_handoff"

// ErrInvalidFrame marks a client-side protocol violation (a frame that fails
// strict decode, arrives before hello, or is not a legal inbound type). The RPC
// layer maps it to invalid_argument; other Sync errors are internal.
var ErrInvalidFrame = errors.New("invalid browser frame")

// Bridge is the per-daemon-run browser session. hello is tracked in memory: a
// fresh hello resets the offered/cancelled bookkeeping, which is exactly the
// recovery an MV3 service-worker restart needs (the extension reconnects, says
// hello again, and re-receives outstanding offers).
type Bridge struct {
	jobs *job.Store
	svc  *app.Service
	cfg  config.Config

	mu         sync.Mutex
	seq        int64
	helloSeen  bool
	offered    map[string]bool // handoff jobs offered this hello-session
	cancelSent map[string]bool // jobs a daemon-side cancel was already announced for
	now        func() time.Time
}

// NewBridge constructs the bridge. It is cheap and always constructed; whether
// any job is ever offered depends on config (extension_id / openurl base).
func NewBridge(jobs *job.Store, svc *app.Service, cfg config.Config) *Bridge {
	return &Bridge{
		jobs: jobs, svc: svc, cfg: cfg,
		offered: map[string]bool{}, cancelSent: map[string]bool{},
		now: time.Now,
	}
}

// Sync processes a batch of inbound frames (possibly empty for a poll) and
// returns the outbound command frames. Every inbound frame is re-validated with
// protocol.DecodeBrowserMessage; malformed frames fail the whole call closed.
// A valid frame or poll arriving after a daemon restart but before hello gets a
// protocol error frame so the still-connected extension can reconnect and
// establish a fresh hello-session. Every outbound frame is self-validated by
// the same decoder before it leaves.
func (b *Bridge) Sync(ctx context.Context, frames []json.RawMessage) ([]json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var out []json.RawMessage
	for _, raw := range frames {
		msg, err := protocol.DecodeBrowserMessage(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidFrame, err)
		}
		if !b.helloSeen && msg.Type != protocol.MsgHello {
			return b.helloRequired()
		}
		replies, err := b.handle(ctx, msg)
		if err != nil {
			return nil, err
		}
		out = append(out, replies...)
	}
	if !b.helloSeen {
		required, err := b.helloRequired()
		if err != nil {
			return nil, err
		}
		return append(out, required...), nil
	}
	polled, err := b.poll(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, polled...), nil
}

// handle dispatches one decoded inbound frame.
func (b *Bridge) handle(ctx context.Context, msg *protocol.BrowserMessage) ([]json.RawMessage, error) {
	if msg.Type == protocol.MsgHello {
		b.helloSeen = true
		b.offered = map[string]bool{}
		b.cancelSent = map[string]bool{}
		ack, err := b.frame(protocol.MsgHelloAck, "", protocol.EmptyPayload{})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{ack}, nil
	}
	if !b.helloSeen {
		return b.helloRequired()
	}

	switch msg.Type {
	case protocol.MsgJobAccept:
		return nil, b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.job_accept", nil)

	case protocol.MsgJobReject:
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.job_reject", nil); err != nil {
			return nil, err
		}
		if fellBack, err := b.fallbackOAHandoff(ctx, msg.JobID, "browser_rejected"); err != nil {
			return nil, err
		} else if fellBack {
			return nil, nil
		}
		if err := b.resolveHandoff(ctx, msg.JobID, "cancelled"); err != nil {
			return nil, err
		}
		return nil, b.leaveHandoff(ctx, msg.JobID, job.StateUnavailable, "browser_rejected")

	case protocol.MsgAuthPending, protocol.MsgAuthReturned:
		return nil, b.recordAuth(ctx, msg)

	case protocol.MsgDownloadStarted:
		return nil, b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.download_started", nil)

	case protocol.MsgDownloadComplete:
		p := msg.Payload.(*protocol.DownloadCompletePayload)
		if err := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.download_complete",
			map[string]any{"filename": p.Filename, "size_bytes": p.SizeBytes}); err != nil {
			return nil, err
		}
		if err := b.adopt(ctx, msg.JobID, p.Filename); err != nil {
			// Environmental failure (file not there yet, Chrome rename race,
			// user saved elsewhere) must not sever the bridge: record it, keep
			// the job parked, and let the poll-time directory scan pick the
			// file up when it appears. Confinement violations land here too —
			// the report is ignored and the job simply stays awaiting_human.
			if evErr := b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.adoption_deferred",
				map[string]any{"filename": p.Filename, "reason": truncate(err.Error(), 200)}); evErr != nil {
				return nil, evErr
			}
		}
		ack, err := b.frame(protocol.MsgAck, msg.JobID, protocol.EmptyPayload{})
		if err != nil {
			return nil, err
		}
		return []json.RawMessage{ack}, nil

	case protocol.MsgProviderOutcome:
		return nil, b.outcome(ctx, msg.JobID, msg.Payload.(*protocol.ProviderOutcomePayload))

	case protocol.MsgCancel:
		// Extension -> daemon: the user closed the broker-owned tab. Treat as a
		// cancelled outcome.
		if err := b.resolveHandoff(ctx, msg.JobID, "cancelled"); err != nil {
			return nil, err
		}
		b.cancelSent[msg.JobID] = true // we initiated nothing to echo back
		return nil, b.jobs.Cancel(ctx, msg.JobID, "browser_cancelled")

	case protocol.MsgError:
		// Only the normalized code is durable; the free-text message is
		// extension-supplied and never persisted.
		return nil, b.jobs.S.AppendEvent(ctx, msg.JobID, "browser.error",
			map[string]any{"code": msg.Payload.(*protocol.ErrorPayload).Code})

	default:
		return nil, fmt.Errorf("%w: unexpected inbound frame type %q", ErrInvalidFrame, msg.Type)
	}
}

// helloRequired tells a still-connected extension that the daemon lost its
// in-memory browser session (for example, after a daemon restart). The existing
// error frame keeps papio-browser/1 unchanged while instructing the extension
// to reconnect and send the mandatory first hello.
func (b *Bridge) helloRequired() ([]json.RawMessage, error) {
	frame, err := b.frame(protocol.MsgError, "", protocol.ErrorPayload{
		Code:    "expected_hello",
		Message: "hello required before browser session can resume",
	})
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{frame}, nil
}

// recordAuth appends a timing-only auth event. The AuthPayload structurally
// cannot carry a URL, host, title, query, or fragment, so an identity-provider
// address cannot enter the event stream through this path.
func (b *Bridge) recordAuth(ctx context.Context, msg *protocol.BrowserMessage) error {
	kind := "browser.auth_pending"
	if msg.Type == protocol.MsgAuthReturned {
		kind = "browser.auth_returned"
	}
	detail := map[string]any{}
	if p := msg.Payload.(*protocol.AuthPayload); p.ElapsedMS != nil {
		detail["elapsed_ms"] = *p.ElapsedMS
	}
	return b.jobs.S.AppendEvent(ctx, msg.JobID, kind, detail)
}

// outcome maps a terminal provider observation onto a policy-legal transition.
func (b *Bridge) outcome(ctx context.Context, jobID string, p *protocol.ProviderOutcomePayload) error {
	switch p.Outcome {
	case "cancelled":
		if err := b.resolveHandoff(ctx, jobID, "cancelled"); err != nil {
			return err
		}
		b.cancelSent[jobID] = true
		return b.jobs.Cancel(ctx, jobID, "browser_cancelled")

	case "no_entitlement", "document_delivery_available":
		if fellBack, err := b.fallbackOAHandoff(ctx, jobID, p.Outcome); err != nil {
			return err
		} else if fellBack {
			return nil
		}
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		return b.leaveHandoff(ctx, jobID, job.StateUnavailable, p.Outcome)

	case "wrong_work", "ui_changed":
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		return b.leaveHandoff(ctx, jobID, job.StateNeedsReview, p.Outcome)

	case "rate_limited":
		if err := b.resolveHandoff(ctx, jobID, "resolved"); err != nil {
			return err
		}
		return b.leaveHandoff(ctx, jobID, job.StateRetryWait, p.Outcome)

	case "human_auth_required", "terms_acceptance_required":
		// Still legitimately in progress: keep the job parked and add the
		// specific human action the extension observed.
		_, err := b.jobs.OpenHumanAction(ctx, jobID, p.Outcome,
			"the provider requires a human step before the download can proceed")
		return err

	default:
		return fmt.Errorf("unknown provider outcome %q", p.Outcome)
	}
}

// leaveHandoff transitions a parked handoff job out of awaiting_human. It is
// idempotent: if the job already left awaiting_human, it does nothing.
func (b *Bridge) leaveHandoff(ctx context.Context, jobID, to, reason string) error {
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return err
	}
	if row.State != job.StateAwaitingHuman {
		return nil
	}
	detail := map[string]any{"reason": reason}
	var opts []job.TransitionOpt
	switch to {
	case job.StateUnavailable:
		opts = append(opts, job.WithTerminalReason(reason))
	case job.StateRetryWait:
		opts = append(opts, job.WithRetryAt(b.now().Add(b.actionExpiry())))
	}
	return b.jobs.Transition(ctx, jobID, job.StateAwaitingHuman, to, detail, opts...)
}

// adopt resolves the reported download strictly under the job's adoption
// directory and hands it to the app for validation. The filename has already
// passed protocol validation (no path separators); this adds IsLocal and a
// symlink-resolved prefix guard before app-side confinement.
func (b *Bridge) adopt(ctx context.Context, jobID, filename string) error {
	if !filepath.IsLocal(filename) {
		return fmt.Errorf("adoption filename %q is not a local name", filename)
	}
	root := filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("adoption root unavailable: %w", err)
	}
	full := filepath.Join(realRoot, filename)
	rel, err := filepath.Rel(realRoot, full)
	if err != nil || rel != filename || strings.Contains(rel, "..") {
		return fmt.Errorf("adoption path escapes %s", realRoot)
	}
	return b.svc.AdoptDownload(ctx, jobID, full)
}

// scanAdoptionDir looks for exactly one settled candidate file in a parked
// job's adoption directory. Dotfiles (.DS_Store) are invisible; any
// .crdownload/.download marks an in-progress Chrome write and defers the whole
// scan; more than one visible file is ambiguous and adopts nothing. The
// returned name feeds adopt(), which re-applies full confinement checks.
func (b *Bridge) scanAdoptionDir(_ context.Context, jobID string) (string, bool) {
	dir := filepath.Join(b.cfg.EffectiveAdoptionRoot(), jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false // no directory yet: nothing placed
	}
	var name string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") {
			continue
		}
		if strings.HasSuffix(n, ".crdownload") || strings.HasSuffix(n, ".download") {
			return "", false // Chrome is still writing; wait for the rename
		}
		if !e.Type().IsRegular() {
			continue
		}
		if name != "" {
			return "", false // ambiguous: stays with the user
		}
		name = n
	}
	return name, name != ""
}

// SweepAdoptions adopts any settled file sitting in a parked handoff job's
// adoption directory, independently of whether the extension is connected or
// has said hello. This makes directory adoption self-driving: the daemon owns
// completion, the browser plane is only a delivery hint. It scans the adoption
// root directly rather than the newest-N job list, so a settled download is
// never missed behind a large handoff backlog. It never emits frames or opens
// offers. Safe to call on a timer.
func (b *Bridge) SweepAdoptions(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	root := b.cfg.EffectiveAdoptionRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return err
	}
	handoff := map[string]bool{}
	for _, a := range actions {
		if a.Kind == handoffActionKind {
			handoff[a.JobID] = true
		}
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "rejected" {
			continue
		}
		jobID := e.Name()
		if !handoff[jobID] {
			continue // no open handoff action: not an adoptable handoff job
		}
		row, err := b.jobs.Get(ctx, jobID)
		if err != nil || row == nil || row.State != job.StateAwaitingHuman {
			continue
		}
		name, ok := b.scanAdoptionDir(ctx, jobID)
		if !ok {
			continue
		}
		if err := b.adopt(ctx, jobID, name); err != nil {
			if evErr := b.jobs.S.AppendEvent(ctx, jobID, "browser.adoption_deferred",
				map[string]any{"filename": name, "reason": truncate(err.Error(), 200)}); evErr != nil {
				return evErr
			}
		}
	}
	return nil
}

// SweepTerminalAdoptions removes the per-job adoption landing directory of any
// terminal job. A ready job's PDF has already been promoted into the immutable
// artifact store (Zotero imports from there, never from the landing copy), and
// cancelled/failed/unavailable jobs never produced anything the user needs from
// this directory, so the landing bytes are pure disk growth. Non-terminal jobs
// are load-bearing and never swept: awaiting_human handoffs may still receive a
// download, and needs_review inspection files are referenced by open actions.
// The rejected/ sibling directory, which deliberately preserves files a human
// must re-supply, is left untouched. Best-effort, idempotent, and safe on a
// timer.
func (b *Bridge) SweepTerminalAdoptions(ctx context.Context) error {
	root := b.cfg.EffectiveAdoptionRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "rejected" {
			continue
		}
		row, err := b.jobs.Get(ctx, e.Name())
		if err != nil || row == nil {
			continue // unknown or unreadable dir: leave it for a human
		}
		if !job.Terminal(row.State) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
	return nil
}

// RunSweeper calls SweepAdoptions and SweepTerminalAdoptions on an interval
// until ctx is cancelled.
// Cancellation is a normal shutdown and returns nil. Per-job adoption failures
// are recorded as durable browser.adoption_deferred events inside
// SweepAdoptions; a transient store-level sweep error is retried on the next
// tick rather than returned, because this goroutine is unsupervised and a dead
// sweeper silently strands every subsequently downloaded PDF.
func (b *Bridge) RunSweeper(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := b.SweepAdoptions(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// Best-effort, idempotent scan: a transient store error (DB
				// busy, a momentary read failure) must NOT kill the only
				// directory-adoption loop. A dead sweeper silently strands
				// every PDF that lands afterward until a daemon restart, and
				// this goroutine is unsupervised, so its death is invisible.
				// Retry next tick; a genuinely fatal store failure also breaks
				// the supervised server and scheduler loops.
			}
			if err := b.SweepTerminalAdoptions(ctx); err != nil && ctx.Err() != nil {
				return nil
			}
		}
	}
}

// poll offers outstanding handoff jobs (once per hello-session) and announces a
// daemon-side cancel for any offered job the user has since cancelled.
func (b *Bridge) poll(ctx context.Context) ([]json.RawMessage, error) {
	awaiting, err := b.jobs.List(ctx, job.StateAwaitingHuman, 200)
	if err != nil {
		return nil, err
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return nil, err
	}
	handoff := map[string]job.HumanAction{}
	for _, a := range actions {
		if a.Kind == handoffActionKind {
			handoff[a.JobID] = a
		}
	}
	present := map[string]bool{}
	var out []json.RawMessage
	for i := range awaiting {
		row := awaiting[i]
		present[row.ID] = true
		action, ok := handoff[row.ID]
		if !ok {
			continue
		}
		// Directory-scan adoption: a file the user (or a steered Chrome
		// download) placed in the job's adoption directory is the strongest
		// job-scoped gesture available. Exactly one settled regular file
		// adopts; zero or several (or an in-progress .crdownload) waits —
		// ambiguity stays with the user, per the fail-closed rule.
		if name, ok := b.scanAdoptionDir(ctx, row.ID); ok {
			if err := b.adopt(ctx, row.ID, name); err != nil {
				if evErr := b.jobs.S.AppendEvent(ctx, row.ID, "browser.adoption_deferred",
					map[string]any{"filename": name, "reason": truncate(err.Error(), 200)}); evErr != nil {
					return nil, evErr
				}
			} else {
				continue // adopted; the job has left awaiting_human
			}
		}
		if b.offered[row.ID] {
			continue
		}
		offer, err := b.offer(row, action.Detail)
		if err != nil {
			return nil, err
		}
		out = append(out, offer)
		b.offered[row.ID] = true
	}
	// Announce a cancel for any offered job that left awaiting_human because it
	// was cancelled daemon-side (e.g. `papio jobs cancel`).
	for id := range b.offered {
		if present[id] || b.cancelSent[id] {
			continue
		}
		row, err := b.jobs.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if row.State != job.StateCancelled {
			continue
		}
		frame, err := b.frame(protocol.MsgCancel, id, protocol.EmptyPayload{})
		if err != nil {
			return nil, err
		}
		out = append(out, frame)
		b.cancelSent[id] = true
	}
	return out, nil
}

// offer builds a job_offer for one parked handoff job. OA browser handoffs
// reuse the frozen OpenURL field with the candidate's public URL; institutional
// handoffs still construct the regular OpenURL resolver link.
func (b *Bridge) offer(row job.Row, handoffDetail string) (json.RawMessage, error) {
	base, _ := b.cfg.OpenURLBaseFor(row.Policy.Resolver)
	offerURL := OpenURL(base, row.Work)
	if oaURL, ok := app.OABrowserHandoffURL(handoffDetail); ok {
		offerURL = oaURL
	}
	hosts := []string{}
	if h := resolverHost(offerURL); h != "" {
		hosts = append(hosts, h)
	}
	hosts = append(hosts, verifiedProviderHosts...)
	expected := &protocol.JobOfferExpected{DOI: row.Work.DOI, Title: truncate(row.Work.Title, 500)}
	if expected.DOI == "" && expected.Title == "" {
		expected = nil
	}
	payload := protocol.JobOfferPayload{
		OpenURL:       offerURL,
		ProviderHosts: hosts,
		Expected:      expected,
		AccessMode:    b.cfg.AccessMode,
		ExpiresAt:     b.now().Add(b.actionExpiry()).UTC().Format(time.RFC3339),
	}
	return b.frame(protocol.MsgJobOffer, row.ID, payload)
}

// fallbackOAHandoff replaces the one-time OA browser offer with the ordinary
// institutional resolver offer while keeping the job parked. The action's
// detail is the durable offer discriminator, so a restart cannot re-open the
// OA URL and alternate forever.
func (b *Bridge) fallbackOAHandoff(ctx context.Context, jobID, failure string) (bool, error) {
	row, err := b.jobs.Get(ctx, jobID)
	if err != nil {
		return false, err
	}
	if base, ok := b.cfg.OpenURLBaseFor(row.Policy.Resolver); !ok || base == "" {
		return false, nil
	}
	actions, err := b.jobs.ListHumanActions(ctx, true)
	if err != nil {
		return false, err
	}
	for _, action := range actions {
		if action.JobID != jobID || action.Kind != handoffActionKind {
			continue
		}
		if _, ok := app.OABrowserHandoffURL(action.Detail); !ok {
			return false, nil
		}
		if _, err := b.jobs.OpenHumanAction(ctx, jobID, handoffActionKind, app.InstitutionalOpenURLHandoffDetail); err != nil {
			return false, err
		}
		if err := b.jobs.RecordEvent(ctx, jobID, "browser.oa_handoff_fallback", map[string]any{"reason": failure}); err != nil {
			return false, err
		}
		delete(b.offered, jobID)
		return true, nil
	}
	return false, nil
}

// resolveHandoff closes the open openurl_handoff action for a job with the given
// terminal status ("resolved" or "cancelled").
func (b *Bridge) resolveHandoff(ctx context.Context, jobID, status string) error {
	_, err := b.jobs.S.DB().ExecContext(ctx,
		`UPDATE human_actions SET status = ?, resolved_at = ?
		 WHERE job_id = ? AND kind = ? AND status = 'open'`,
		status, store.Now(), jobID, handoffActionKind)
	return err
}

// frame encodes one outbound envelope with a fresh msg_id and a monotonic seq,
// then self-validates it through the strict decoder so a malformed command can
// never leave the daemon.
func (b *Bridge) frame(msgType, jobID string, payload any) (json.RawMessage, error) {
	b.seq++
	env := map[string]any{
		"protocol": protocol.BrowserProtocolVersion,
		"type":     msgType,
		"msg_id":   newMsgID(),
		"seq":      b.seq,
		"payload":  payload,
	}
	if jobID != "" {
		env["job_id"] = jobID
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	if _, err := protocol.DecodeBrowserMessage(raw); err != nil {
		return nil, fmt.Errorf("outbound %s failed self-validation: %w", msgType, err)
	}
	return raw, nil
}

func (b *Bridge) actionExpiry() time.Duration {
	secs := b.cfg.Browser.ActionExpirySeconds
	if secs <= 0 {
		secs = 1800
	}
	return time.Duration(secs) * time.Second
}

// newMsgID returns a random identifier matching ^[A-Za-z0-9_-]{8,64}$.
func newMsgID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
