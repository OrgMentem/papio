// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"papio/internal/job"
	"papio/internal/notify"
	"papio/internal/zotio"
)

// ZoteroDesktop reports and awaits Zotero desktop's presence for imports whose
// route needs its connector. Production passes the zotio client. Nil, or a
// zotio that returns zotio.ErrDesktopPresenceUnsupported, keeps the spaced
// import retries that predate it.
type ZoteroDesktop interface {
	DesktopStatus(context.Context) (zotio.DesktopStatus, error)
	WaitForDesktop(context.Context) (zotio.DesktopStatus, error)
}

// Import outcome statuses and reasons papio records in zotio.auto_import
// events while an import waits for Zotero desktop.
const (
	importStatusWaiting              = "waiting"
	importReasonZoteroNotRunning     = "zotero_not_running"
	importReasonConnectorUnreachable = "zotero_connector_unreachable"
	// Zotero runs past its startup window with a connector that cannot take
	// requests; waiting alone does not end these.
	importReasonZoteroUnresponsive = "zotero_unresponsive"
	importReasonConnectorOff       = "zotero_connector_off"
	// Zotero runs past its startup window and did not answer one check, as
	// a large sync makes it; waiting ends it.
	importReasonZoteroBusy = "zotero_busy"
	// A queued import waited for Zotero desktop, which now accepts connector
	// saves; it waits only for its turn in the paced import pass.
	importStatusQueued      = "queued"
	importReasonZoteroReady = "zotero_ready"
)

// ZoteroNoticePrefix begins the aggregate key of every notice that papers
// wait for Zotero desktop; the rest of the key is the wait reason.
const ZoteroNoticePrefix = "zotero:"

// desktopWaitReason names why an import waits, from the status that set
// presence.
func desktopWaitReason(status zotio.DesktopStatus) string {
	switch {
	case status.Stuck() && status.State == zotio.DesktopStateConnectorOff:
		return importReasonConnectorOff
	case status.Stuck():
		return importReasonZoteroUnresponsive
	case status.State == zotio.DesktopStateBusy:
		return importReasonZoteroBusy
	case status.Running:
		return importReasonConnectorUnreachable
	default:
		return importReasonZoteroNotRunning
	}
}

// desktopStuckRewait is how long the watcher waits before it asks zotio again
// about a Zotero that is open but stuck. zotio answers such a wait at once, and
// nothing on disk announces a recovery, so this bounds the cost to one short
// process per interval until the operator restarts Zotero.
var desktopStuckRewait = 3 * time.Minute

// desktopWaitRetryDelays space the restarts of a waiter that failed rather
// than timed out, so a broken zotio costs one process per delay instead of a
// tight loop. A timeout restarts at once: that is the normal idle cycle.
var desktopWaitRetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, time.Hour}

type desktopPresence int

const (
	desktopUnknown desktopPresence = iota
	desktopReady
	desktopClosed
)

// zoteroDesktopState is the daemon's in-memory view of Zotero desktop. It is
// deliberately not durable: presence is a fact about now, and a restarted
// daemon asks zotio once before its first desktop-route import.
type zoteroDesktopState struct {
	mu       sync.Mutex
	presence desktopPresence
	// lastStatus is the status that set presence, for the notification copy.
	lastStatus zotio.DesktopStatus
	// generation changes whenever presence does. A caller that read presence
	// before a slow step compares it afterwards to learn whether presence
	// moved meanwhile.
	generation uint64
	// notified is the wait reason the current episode has already told the
	// operator about; empty when nothing was sent. Zotero turning ready
	// clears it, and a different reason (closed, then open but not
	// responding) is a new thing to say.
	notified string
	// queued reports that the papers still waiting when presence last became
	// ready are marked queued. Presence turning closed clears it; so does a
	// restart, whose first ready presence then queues the durable waits.
	queued bool
	// probeMu serializes status probes so concurrent imports ask zotio once.
	probeMu sync.Mutex
	// wake tells the watcher that an import waits. It holds at most one
	// signal, so marking many jobs waiting starts one waiter.
	wakeOnce sync.Once
	wake     chan struct{}
}

func (d *zoteroDesktopState) wakeChan() chan struct{} {
	d.wakeOnce.Do(func() { d.wake = make(chan struct{}, 1) })
	return d.wake
}

func (d *zoteroDesktopState) signal() {
	select {
	case d.wakeChan() <- struct{}{}:
	default:
	}
}

func (d *zoteroDesktopState) current() desktopPresence {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.presence
}

// set records a probe or wait result.
func (d *zoteroDesktopState) set(status zotio.DesktopStatus) {
	next := desktopClosed
	if status.Ready() {
		next = desktopReady
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if next != d.presence {
		d.generation++
	}
	d.presence = next
	d.lastStatus = status
	if next == desktopReady {
		d.notified = ""
	} else {
		d.queued = false
	}
}

func (d *zoteroDesktopState) forget() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.presence != desktopUnknown {
		d.generation++
	}
	d.presence = desktopUnknown
}

// queueDue reports whether Zotero accepts connector saves while papers may
// still be marked waiting, and the generation to hand to queueDone.
func (d *zoteroDesktopState) queueDue() (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.generation, d.presence == desktopReady && !d.queued
}

// readySince reports whether presence has stayed ready since generation.
func (d *zoteroDesktopState) readySince(generation uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.presence == desktopReady && d.generation == generation
}

// queueDone records that every waiting paper is queued, unless presence
// moved since generation.
func (d *zoteroDesktopState) queueDone(generation uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.generation == generation {
		d.queued = true
	}
}

// importNeedsDesktop reports whether this job's import route must reach
// Zotero desktop and the installed zotio can say whether it runs.
func (s *Service) importNeedsDesktop(row *job.Row) bool {
	return s.ZoteroDesktop != nil && zotio.ImportNeedsDesktop(row.ZotioItemKey, s.Config.Zotio.AttachmentMode)
}

// desktopClosed reports whether Zotero desktop is known not to accept
// connector saves. An unknown presence costs one `zotio desktop status`; a
// known one costs nothing, so a pass over many waiting jobs spawns no process.
// When presence cannot be learned — an older zotio, or a probe that failed —
// it reports false and the import is attempted, which is the behavior before
// presence existed.
func (s *Service) desktopClosed(ctx context.Context) bool {
	if presence := s.desktop.current(); presence != desktopUnknown {
		return presence == desktopClosed
	}
	presence, _ := s.probeDesktop(ctx)
	return presence == desktopClosed
}

// probeDesktop asks zotio once and records the answer. It returns the
// probe's error when presence stays unknown because zotio failed or cannot
// report presence.
func (s *Service) probeDesktop(ctx context.Context) (desktopPresence, error) {
	s.desktop.probeMu.Lock()
	defer s.desktop.probeMu.Unlock()
	if presence := s.desktop.current(); presence != desktopUnknown {
		return presence, nil // a concurrent probe answered while this one queued
	}
	status, err := s.ZoteroDesktop.DesktopStatus(ctx)
	if err != nil {
		if !errors.Is(err, zotio.ErrDesktopPresenceUnsupported) && ctx.Err() == nil {
			log.Printf("papio: asking zotio whether Zotero desktop runs: %v", err)
		}
		return desktopUnknown, err
	}
	s.desktop.set(status)
	return s.desktop.current(), nil
}

// connectorClosedFailure reports whether a failed import found Zotero
// desktop's connector refusing or not answering, which is what a closed
// Zotero produces: zotio's connector save fails with
// zotero_connector_refused, and its connector check says the connector is
// not reachable or the connection was refused.
func connectorClosedFailure(err error, class string) bool {
	if class == zotio.ErrorClassZoteroConnectorRefused {
		return true
	}
	text := strings.ToLower(err.Error())
	if !strings.Contains(text, "connector") && !strings.Contains(text, "zotero desktop") {
		return false
	}
	return strings.Contains(text, "not reachable") || strings.Contains(text, "connection refused") ||
		strings.Contains(text, "not running")
}

// recordImportWaiting records that a job's import waits for Zotero desktop.
// A job already waiting for the same reason gets no second event, so a closed
// Zotero leaves one durable event per job however many passes it spans; a
// changed reason (closed, then open but not responding) records the new one so
// every surface names the current remedy. No attempt is counted:
// importNeedsRetry counts only error outcomes.
func (s *Service) recordImportWaiting(ctx context.Context, row *job.Row) {
	defer s.desktop.signal()
	eventCtx := context.WithoutCancel(ctx)
	s.desktop.mu.Lock()
	reason := desktopWaitReason(s.desktop.lastStatus)
	s.desktop.mu.Unlock()
	if events, err := s.Jobs.Events(eventCtx, row.ID); err == nil {
		if status, previous := latestImportReason(events); status == importStatusWaiting && previous == reason {
			return
		}
	}
	_ = s.Jobs.RecordEvent(eventCtx, row.ID, "zotio.auto_import", map[string]any{
		"status": importStatusWaiting, "reason": reason,
		"parent_key": "", "attachment_key": "",
	})
}

// notifyImportsWaiting routes one desktop notification per closed-Zotero
// episode, never one per paper or per pass. waiting is the number of papers
// the pass found waiting and since is the oldest waiting event among them.
// The aggregate key and window come from that durable event, so a daemon
// restarted mid-episode coalesces into the notification it already sent
// rather than repeating it.
func (s *Service) notifyImportsWaiting(ctx context.Context, waiting int, since time.Time) {
	if s.Notifier == nil || waiting == 0 || since.IsZero() {
		return
	}
	s.desktop.mu.Lock()
	reason := desktopWaitReason(s.desktop.lastStatus)
	generation := s.desktop.generation
	if s.desktop.presence != desktopClosed || s.desktop.notified == reason {
		s.desktop.mu.Unlock()
		return
	}
	s.desktop.mu.Unlock()

	message := zoteroWaitingMessage(waiting, reason)
	happened := s.Now().UTC()
	intent := notify.Intent{
		EventKind: "zotio.import_waiting", Category: notify.CategorySystemDegraded,
		AggregateKey: ZoteroNoticePrefix + reason, Phase: notify.PhaseEpisode,
		WindowStart: since.UTC(), HappenedAt: happened, Message: message,
		Detail: notify.Event{Kind: "zotio.import_waiting", Message: message},
	}
	if err := s.Notifier.Route(context.WithoutCancel(ctx), intent); err != nil {
		// Nothing durable holds the notice, so the next pass routes it again.
		log.Printf("papio: routing Zotero waiting notification: %v", err)
		return
	}
	// The ledger holds the notice now. Latch it for this episode only: a
	// Zotero that became ready meanwhile ended the episode it belongs to.
	s.desktop.mu.Lock()
	if s.desktop.generation == generation {
		s.desktop.notified = reason
	}
	s.desktop.mu.Unlock()
}

// queueWaitingImports marks the papers still waiting for Zotero desktop as
// queued once Zotero accepts connector saves. One pass imports at most
// maxImportsPerPass papers, so without this the rest kept a durable wait
// that told doctor, status, activity and batch reports to open a Zotero that
// was already open. It runs once each time presence becomes ready, costs no
// zotio process, and spends no attempt. rows is the pass's ready scan.
func (s *Service) queueWaitingImports(ctx context.Context, rows []job.Row) {
	generation, due := s.desktop.queueDue()
	if !due {
		return
	}
	eventCtx := context.WithoutCancel(ctx)
	complete := true
	for i := range rows {
		if ctx.Err() != nil || !s.desktop.readySince(generation) {
			return
		}
		if !rows[i].Policy.AutoImport || !s.claimImport(rows[i].ID) {
			continue // an import that holds the job records its own outcome
		}
		complete = s.queueWaitingImport(eventCtx, rows[i].ID) && complete
		s.releaseImport(rows[i].ID)
	}
	if complete {
		s.desktop.queueDone(generation)
	}
}

// queueWaitingImport records one queued event for a job whose latest import
// outcome is a wait, and reports whether the job needs no further look.
func (s *Service) queueWaitingImport(ctx context.Context, jobID string) bool {
	events, err := s.Jobs.Events(ctx, jobID)
	if err != nil {
		return false
	}
	if status, _ := latestImportReason(events); status != importStatusWaiting {
		return true
	}
	return s.Jobs.RecordEvent(ctx, jobID, "zotio.auto_import", map[string]any{
		"status": importStatusQueued, "reason": importReasonZoteroReady,
		"parent_key": "", "attachment_key": "",
	}) == nil
}

// ZoteroWaitNoticeCurrent reports whether a `zotero:<reason>` notice still
// describes Zotero desktop. The notification drain asks before it delivers,
// because quiet hours can hold the notice past the moment Zotero opens, and
// "Open Zotero" is then wrong. The notice is stale when Zotero accepts
// connector saves, or when no paper still waits for this reason: the papers
// were imported or queued, or the condition changed and the notice for the
// new reason speaks instead.
func (s *Service) ZoteroWaitNoticeCurrent(ctx context.Context, reason string) (bool, error) {
	if s.desktop.current() == desktopReady {
		return false, nil
	}
	rows, err := s.Jobs.List(ctx, job.StateReady, readyImportScanLimit)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if !row.Policy.AutoImport {
			continue
		}
		events, err := s.Jobs.Events(ctx, row.ID)
		if err != nil {
			return false, err
		}
		if status, waitReason := latestImportReason(events); status == importStatusWaiting && waitReason == reason {
			return true, nil
		}
	}
	return false, nil
}

// zoteroWaitingMessage names the condition, the number of papers, and the one
// action that ends the wait.
func zoteroWaitingMessage(waiting int, reason string) string {
	papers, them := "1 paper is", "it"
	if waiting != 1 {
		papers, them = fmt.Sprintf("%d papers are", waiting), "them"
	}
	switch reason {
	case importReasonZoteroUnresponsive:
		return fmt.Sprintf("Zotero is open but not responding. %s ready to add. Restart Zotero and papio adds %s.", papers, them)
	case importReasonConnectorOff:
		return fmt.Sprintf("Zotero is open but does not accept papers from papio. %s ready to add. In Zotero, turn on Settings > Advanced > \"Allow other applications to communicate with Zotero\".", papers)
	case importReasonZoteroBusy:
		return fmt.Sprintf("Zotero is busy. %s ready to add. papio adds %s when Zotero answers.", papers, them)
	case importReasonConnectorUnreachable:
		return fmt.Sprintf("Zotero is starting. %s ready to add. papio adds %s when Zotero is ready.", papers, them)
	default:
		return fmt.Sprintf("Zotero is closed. %s ready to add. Open Zotero and papio adds %s.", papers, them)
	}
}

// latestImportReason returns the latest durable auto-import status and its
// reason.
func latestImportReason(events []map[string]any) (status, reason string) {
	for _, event := range events {
		if kind, _ := event["kind"].(string); kind != "zotio.auto_import" {
			continue
		}
		detail, _ := event["detail"].(map[string]any)
		status, _ = detail["status"].(string)
		reason, _ = detail["reason"].(string)
	}
	return status, reason
}

// ZoteroDesktopWatcher runs at most one `zotio desktop wait` while any import
// waits for Zotero desktop, and runs the import-retry pass as soon as Zotero
// accepts connector saves. It is idle, with no process, while nothing waits.
type ZoteroDesktopWatcher struct {
	svc *Service
	// failures counts consecutive waiter failures; only Run touches it.
	failures int
}

// ZoteroDesktopWatcher returns the watcher, or nil when presence cannot be
// known. It satisfies daemon.BackgroundRunner without importing that package.
func (s *Service) ZoteroDesktopWatcher() *ZoteroDesktopWatcher {
	if s == nil || s.ZoteroDesktop == nil {
		return nil
	}
	return &ZoteroDesktopWatcher{svc: s}
}

// Run blocks until ctx ends. Every waiter it starts is bound to ctx, so
// shutdown kills it and Run returns only after the process is reaped.
func (w *ZoteroDesktopWatcher) Run(ctx context.Context) {
	if w == nil || w.svc == nil || w.svc.ZoteroDesktop == nil {
		return
	}
	wake := w.svc.desktop.wakeChan()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
		w.waitUntilReady(ctx)
	}
}

// waitUntilReady keeps one waiter running while presence is closed. It
// returns when Zotero is ready, when presence is no longer known closed, or
// when ctx ends.
func (w *ZoteroDesktopWatcher) waitUntilReady(ctx context.Context) {
	s := w.svc
	for ctx.Err() == nil && s.desktop.current() == desktopClosed {
		status, err := s.ZoteroDesktop.WaitForDesktop(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !errors.Is(err, zotio.ErrDesktopPresenceUnsupported) {
				log.Printf("papio: waiting for Zotero desktop: %v", err)
				delay := desktopWaitRetryDelays[min(w.failures, len(desktopWaitRetryDelays)-1)]
				w.failures++
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			// Nothing watches Zotero now, so a "closed" presence would hold
			// the papers even after Zotero opens. Forget it: the next pass
			// asks `zotio desktop status` once, and either imports, or marks
			// the papers waiting again and wakes a new waiter. A zotio
			// downgraded under the daemon then falls back to attempting.
			s.desktop.forget()
			return
		}
		w.failures = 0
		if status.Stuck() {
			// Zotero is open but its connector cannot take requests, and
			// zotio answered at once. Record the new reason and tell the
			// operator (the pass does both, once per reason), then ask
			// again after a pause instead of respawning in a loop.
			s.desktop.set(status)
			if err := s.retryPendingImports(ctx); err != nil && ctx.Err() == nil {
				log.Printf("papio: import-retry pass after Zotero stopped responding: %v", err)
			}
			timer := time.NewTimer(desktopStuckRewait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		if !status.Ready() {
			// zotio's timeout elapsed: record what it saw, so a stuck Zotero
			// the operator has since quit reads as closed again, and wait
			// again.
			s.desktop.set(status)
			continue
		}
		s.desktop.set(status)
		if err := s.retryPendingImports(ctx); err != nil && ctx.Err() == nil {
			log.Printf("papio: import-retry pass after Zotero opened: %v", err)
		}
		return
	}
}
