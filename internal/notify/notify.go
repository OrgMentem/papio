// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package notify delivers best-effort local desktop notifications.
package notify

import (
	"context"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

const notificationTimeout = 5 * time.Second

// ExecFunc runs one bounded argv command. It is injectable so notification
// construction and failure handling are testable without platform commands.
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
	return MacOS{Exec: execCommand}
}

// Send displays message under the fixed papio title. It uses a five-second
// deadline and deliberately ignores execution errors, including systems where
// desktop notifications are unavailable.
func (m MacOS) Send(ctx context.Context, message string) {
	if m.Exec == nil {
		return
	}
	runBounded(ctx, m.Exec, "osascript", "-e", appleScript(message))
}

var linuxMarkupEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)

// Linux sends notifications through notify-send or the freedesktop D-Bus
// notification service.
type Linux struct {
	Exec      ExecFunc
	mechanism linuxMechanism
}

// Send displays message under the fixed papio title. It uses a five-second
// deadline and deliberately ignores execution errors.
func (l Linux) Send(ctx context.Context, message string) {
	if l.Exec == nil {
		return
	}
	message = linuxMarkupEscaper.Replace(message)
	switch l.mechanism {
	case linuxNotifySend:
		runBounded(ctx, l.Exec, "notify-send", "--app-name", "papio", "--", "papio", message)
	case linuxGDBus:
		runBounded(ctx, l.Exec,
			"gdbus", "call", "--session",
			"--dest", "org.freedesktop.Notifications",
			"--object-path", "/org/freedesktop/Notifications",
			"--method", "org.freedesktop.Notifications.Notify", "--",
			"papio", "0", "", "papio", message, "[]", "{}", "5000",
		)
	}
}

// Windows sends notifications through the built-in PowerShell WinRT API.
type Windows struct {
	Exec ExecFunc
}

// Send displays message under the fixed papio title. It uses a five-second
// deadline and deliberately ignores execution errors.
func (w Windows) Send(ctx context.Context, message string) {
	if w.Exec == nil {
		return
	}
	runBounded(ctx, w.Exec, "powershell", "-NoProfile", "-NonInteractive", "-Command", windowsToastScript(message))
}

func runBounded(ctx context.Context, run ExecFunc, name string, args ...string) {
	bounded, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	_ = run(bounded, name, args...)
}

func execCommand(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

func windowsToastScript(message string) string {
	literal := powershellLiteral(message)
	return `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null; ` +
		`[Windows.UI.Notifications.ToastNotification, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null; ` +
		`$xml = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02); ` +
		`$text = $xml.GetElementsByTagName('text'); ` +
		`$text[0].AppendChild($xml.CreateTextNode('papio')) > $null; ` +
		`$text[1].AppendChild($xml.CreateTextNode(` + literal + `)) > $null; ` +
		`$toast = [Windows.UI.Notifications.ToastNotification]::new($xml); ` +
		`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('papio').Show($toast)`
}

func powershellLiteral(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	return `'` + strings.ReplaceAll(value, `'`, `''`) + `'`
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
