// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"errors"
	"fmt"
	"log"
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
)

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
	// notified is set once the waiting notification for the current closed
	// episode has been routed, and cleared when Zotero is ready again.
	notified bool
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
	d.presence = next
	d.lastStatus = status
	if next == desktopReady {
		d.notified = false
	}
}

func (d *zoteroDesktopState) forget() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.presence = desktopUnknown
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
	return s.probeDesktop(ctx) == desktopClosed
}

// probeDesktop asks zotio once and records the answer.
func (s *Service) probeDesktop(ctx context.Context) desktopPresence {
	s.desktop.probeMu.Lock()
	defer s.desktop.probeMu.Unlock()
	if presence := s.desktop.current(); presence != desktopUnknown {
		return presence // a concurrent probe answered while this one queued
	}
	status, err := s.ZoteroDesktop.DesktopStatus(ctx)
	if err != nil {
		if !errors.Is(err, zotio.ErrDesktopPresenceUnsupported) && ctx.Err() == nil {
			log.Printf("papio: asking zotio whether Zotero desktop runs: %v", err)
		}
		return desktopUnknown
	}
	s.desktop.set(status)
	return s.desktop.current()
}

// recordImportWaiting records that a job's import waits for Zotero desktop.
// A job already waiting gets no second event, so a closed Zotero leaves one
// durable event per job however many passes it spans. No attempt is counted:
// importNeedsRetry counts only error outcomes.
func (s *Service) recordImportWaiting(ctx context.Context, row *job.Row) {
	defer s.desktop.signal()
	eventCtx := context.WithoutCancel(ctx)
	if events, err := s.Jobs.Events(eventCtx, row.ID); err == nil {
		if status, _, _ := settledImport(events); status == importStatusWaiting {
			return
		}
	}
	reason := importReasonZoteroNotRunning
	s.desktop.mu.Lock()
	if s.desktop.lastStatus.Running {
		reason = importReasonConnectorUnreachable
	}
	s.desktop.mu.Unlock()
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
	if s.desktop.presence != desktopClosed || s.desktop.notified {
		s.desktop.mu.Unlock()
		return
	}
	s.desktop.notified = true
	running := s.desktop.lastStatus.Running
	s.desktop.mu.Unlock()

	message := zoteroWaitingMessage(waiting, running)
	happened := s.Now().UTC()
	intent := notify.Intent{
		EventKind: "zotio.import_waiting", Category: notify.CategorySystemDegraded,
		AggregateKey: "zotero:desktop_closed", Phase: notify.PhaseEpisode,
		WindowStart: since.UTC(), HappenedAt: happened, Message: message,
		Detail: notify.Event{Kind: "zotio.import_waiting", Message: message},
	}
	if err := s.Notifier.Route(context.WithoutCancel(ctx), intent); err != nil {
		log.Printf("papio: routing Zotero waiting notification: %v", err)
	}
}

func zoteroWaitingMessage(waiting int, running bool) string {
	state := "Zotero is closed."
	if running {
		state = "Zotero is not accepting papers yet."
	}
	if waiting == 1 {
		return state + " 1 paper is ready to add. Open Zotero and papio adds it."
	}
	return fmt.Sprintf("%s %d papers are ready to add. Open Zotero and papio adds them.", state, waiting)
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
		if !status.Ready() {
			continue // zotio's timeout elapsed: restart the wait
		}
		s.desktop.set(status)
		if err := s.retryPendingImports(ctx); err != nil && ctx.Err() == nil {
			log.Printf("papio: import-retry pass after Zotero opened: %v", err)
		}
		return
	}
}
