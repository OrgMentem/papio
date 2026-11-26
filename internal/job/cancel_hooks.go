// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import "sync"

// cancelHooks holds the subscribers to a job's move to cancelled. The zero
// value is ready to use.
type cancelHooks struct {
	mu    sync.Mutex
	next  int
	hooks map[int]func(jobID string)
}

// OnCancelled registers hook to run each time this Store commits a job's move
// to cancelled, whichever path made it: Cancel, a dismissed human action that
// cancels its parked job, or a rejected review. The hook runs on the
// cancelling goroutine after the commit, so it must not block. The returned
// function removes it.
//
// It exists so a scheduler in this process can stop the processor that holds
// the cancelled job at once. A cancel committed by another process reaches that
// processor only through its next lease heartbeat, which stays the fallback.
func (js *Store) OnCancelled(hook func(jobID string)) (unsubscribe func()) {
	hooks := &js.cancelHooks
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if hooks.hooks == nil {
		hooks.hooks = make(map[int]func(string))
	}
	id := hooks.next
	hooks.next++
	hooks.hooks[id] = hook
	return func() {
		hooks.mu.Lock()
		defer hooks.mu.Unlock()
		delete(hooks.hooks, id)
	}
}

// notifyCancelled tells every subscriber that jobID has committed as cancelled.
func (js *Store) notifyCancelled(jobID string) {
	hooks := &js.cancelHooks
	hooks.mu.Lock()
	subscribed := make([]func(string), 0, len(hooks.hooks))
	for _, hook := range hooks.hooks {
		subscribed = append(subscribed, hook)
	}
	hooks.mu.Unlock()
	for _, hook := range subscribed {
		hook(jobID)
	}
}
