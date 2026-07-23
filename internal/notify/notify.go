// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package notify delivers best-effort local desktop notifications.
package notify

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const notificationTimeout = 5 * time.Second

// ExecFunc runs one bounded argv command. It is injectable so notification
// construction and failure handling are testable without invoking osascript.
type ExecFunc func(context.Context, string, ...string) error

// Sender delivers one notification. Senders must never make the caller's work
// fail: notifications are an optional, best-effort UX affordance.
type Sender interface {
	Send(context.Context, string)
}

// MacOS sends notifications through the platform's osascript executable.
type MacOS struct {
	Exec ExecFunc
}

// NewMacOS constructs the production macOS notification sender.
func NewMacOS() MacOS {
	return MacOS{Exec: func(ctx context.Context, name string, args ...string) error {
		return exec.CommandContext(ctx, name, args...).Run()
	}}
}

// Send displays message under the fixed papio title. It uses a five-second
// deadline and deliberately ignores execution errors, including systems where
// desktop notifications are unavailable.
func (m MacOS) Send(ctx context.Context, message string) {
	if m.Exec == nil {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	_ = m.Exec(bounded, "osascript", "-e", appleScript(message))
}

func appleScript(message string) string {
	return `display notification "` + escapeAppleString(message) + `" with title "papio"`
}

func escapeAppleString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\r", `\r`)
	return strings.ReplaceAll(value, "\n", `\n`)
}

// Coalescer limits each notification class to one delivery per interval. The
// first event is delivered immediately; events accumulated until the next
// window are summarized when that window closes.
type Coalescer struct {
	Sender   Sender
	Now      func() time.Time
	Interval time.Duration

	mu        sync.Mutex
	pending   map[string]int
	last      map[string]time.Time
	scheduled map[string]uint64
	sequence  map[string]uint64
	after     func(time.Duration, func())
}

// NewCoalescer constructs a coalescer with the production sixty-second window.
func NewCoalescer(sender Sender) *Coalescer {
	return &Coalescer{
		Sender: sender, Now: time.Now, Interval: time.Minute,
		pending: make(map[string]int), last: make(map[string]time.Time),
		scheduled: make(map[string]uint64), sequence: make(map[string]uint64),
		after: func(duration time.Duration, callback func()) {
			time.AfterFunc(duration, callback)
		},
	}
}

// HumanAction records a job that needs human attention.
func (c *Coalescer) HumanAction(ctx context.Context) {
	c.notify(ctx, "human_action", func(count int) string {
		base := plural(count, "paper needs your attention", "papers need your attention")
		return base + "; run papio status to see why"
	})
}

// Imported records a job whose automatic Zotio import was applied.
func (c *Coalescer) Imported(ctx context.Context) {
	c.notify(ctx, "imported", func(count int) string {
		return plural(count, "paper imported", "papers imported")
	})
}

func (c *Coalescer) notify(ctx context.Context, kind string, message func(int) string) {
	if c == nil || c.Sender == nil {
		return
	}
	now := c.now()
	interval := c.Interval
	if interval <= 0 {
		interval = time.Minute
	}

	c.mu.Lock()
	if c.pending == nil {
		c.pending = make(map[string]int)
	}
	if c.last == nil {
		c.last = make(map[string]time.Time)
	}
	if c.scheduled == nil {
		c.scheduled = make(map[string]uint64)
	}
	if c.sequence == nil {
		c.sequence = make(map[string]uint64)
	}
	c.pending[kind]++
	last := c.last[kind]
	if !last.IsZero() && now.Sub(last) < interval {
		if _, found := c.scheduled[kind]; !found {
			c.sequence[kind]++
			sequence := c.sequence[kind]
			c.scheduled[kind] = sequence
			after := c.after
			if after == nil {
				after = func(duration time.Duration, callback func()) {
					time.AfterFunc(duration, callback)
				}
			}
			// The timer outlives the request that scheduled it; detach
			// cancellation but keep values so the delayed flush still sends.
			flushCtx := context.WithoutCancel(ctx)
			after(interval-now.Sub(last), func() {
				c.flush(flushCtx, kind, message, sequence)
			})
		}
		c.mu.Unlock()
		return
	}
	count := c.pending[kind]
	c.pending[kind] = 0
	c.last[kind] = now
	delete(c.scheduled, kind)
	c.mu.Unlock()

	c.Sender.Send(ctx, message(count))
}

func (c *Coalescer) flush(ctx context.Context, kind string, message func(int) string, sequence uint64) {
	c.mu.Lock()
	if c.scheduled[kind] != sequence || c.pending[kind] == 0 {
		c.mu.Unlock()
		return
	}
	delete(c.scheduled, kind)
	count := c.pending[kind]
	c.pending[kind] = 0
	c.last[kind] = c.now()
	c.mu.Unlock()

	c.Sender.Send(ctx, message(count))
}

func (c *Coalescer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func plural(count int, singular, plural string) string {
	if count == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(count) + " " + plural
}
