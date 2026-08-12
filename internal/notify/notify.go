// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package notify delivers best-effort local desktop notifications.
package notify

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// PlatformCapability reports whether this build can invoke papio's local
// desktop notification channel. Delivery remains best effort: macOS provides
// no acknowledgement that Notification Center displayed the message.
func PlatformCapability() (available bool, detail string) {
	if runtime.GOOS != "darwin" {
		return false, "desktop notifications are unavailable on " + runtime.GOOS + " (papio uses macOS osascript)"
	}
	if _, err := exec.LookPath("osascript"); err != nil {
		return false, "desktop notifications are unavailable: macOS osascript is not installed"
	}
	return true, "desktop notifications available via macOS osascript (best effort; no delivery acknowledgement)"
}

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
