// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package notify

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMacOSUsesBoundedEscapedArgvAndSwallowsFailure(t *testing.T) {
	var name string
	var args []string
	var deadline time.Time
	sender := MacOS{Exec: func(ctx context.Context, gotName string, gotArgs ...string) error {
		name = gotName
		args = append([]string(nil), gotArgs...)
		deadline, _ = ctx.Deadline()
		return errors.New("notifications unavailable")
	}}
	before := time.Now()
	sender.Send(context.Background(), "A \"quoted\" path \\ here\nnext line")
	if name != "osascript" {
		t.Fatalf("command = %q", name)
	}
	want := []string{"-e", `display notification "A \"quoted\" path \\ here\nnext line" with title "papio"`}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
	if deadline.IsZero() || deadline.Before(before.Add(4*time.Second)) || deadline.After(before.Add(6*time.Second)) {
		t.Fatalf("deadline = %v, want roughly five seconds after %v", deadline, before)
	}
}

func TestPlatformCapability(t *testing.T) {
	tests := []struct {
		name      string
		goos      string
		available map[string]bool
		wantOK    bool
		want      string
	}{
		{
			name:      "darwin osascript",
			goos:      "darwin",
			available: map[string]bool{"osascript": true},
			wantOK:    true,
			want:      "desktop notifications available via macOS osascript (best effort; no delivery acknowledgement)",
		},
		{
			name:      "linux notify-send",
			goos:      "linux",
			available: map[string]bool{"notify-send": true},
			wantOK:    true,
			want:      "desktop notifications available via notify-send (best effort; no delivery acknowledgement)",
		},
		{
			name:      "linux gdbus fallback",
			goos:      "linux",
			available: map[string]bool{"gdbus": true},
			wantOK:    true,
			want:      "desktop notifications available via gdbus (best effort; no delivery acknowledgement)",
		},
		{
			name: "linux unavailable",
			goos: "linux",
			want: "desktop notifications are unavailable: neither notify-send nor gdbus is installed",
		},
		{
			name:      "windows powershell",
			goos:      "windows",
			available: map[string]bool{"powershell": true},
			wantOK:    true,
			want:      "desktop notifications available via powershell WinRT toast (best effort; no delivery acknowledgement)",
		},
		{
			name: "windows unavailable",
			goos: "windows",
			want: "desktop notifications are unavailable: powershell is not installed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := probePlatform(tt.goos, func(name string) (string, error) {
				if tt.available[name] {
					return "/usr/bin/" + name, nil
				}
				return "", exec.ErrNotFound
			})
			if got.available != tt.wantOK || got.detail != tt.want {
				t.Fatalf("probePlatform() = (%t, %q), want (%t, %q)", got.available, got.detail, tt.wantOK, tt.want)
			}
		})
	}
}

func TestLinuxSenderUsesSelectedArgv(t *testing.T) {
	const message = "-paper is ready"
	tests := []struct {
		name      string
		available map[string]bool
		want      []string
	}{
		{
			name:      "notify-send",
			available: map[string]bool{"notify-send": true, "gdbus": true},
			want:      []string{"notify-send", "--app-name", "papio", "--", "papio", message},
		},
		{
			name:      "gdbus fallback",
			available: map[string]bool{"gdbus": true},
			want: []string{
				"gdbus", "call", "--session",
				"--dest", "org.freedesktop.Notifications",
				"--object-path", "/org/freedesktop/Notifications",
				"--method", "org.freedesktop.Notifications.Notify",
				"papio", "0", "", "papio", message, "[]", "{}", "5000",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var argv []string
			sender, ok := newPlatformSender(
				"linux",
				func(name string) (string, error) {
					if tt.available[name] {
						return "/usr/bin/" + name, nil
					}
					return "", exec.ErrNotFound
				},
				func(_ context.Context, name string, args ...string) error {
					argv = append([]string{name}, args...)
					return errors.New("notifications unavailable")
				},
			)
			if !ok {
				t.Fatal("newPlatformSender() unavailable")
			}
			sender.Send(context.Background(), message)
			if !reflect.DeepEqual(argv, tt.want) {
				t.Fatalf("argv = %#v, want %#v", argv, tt.want)
			}
		})
	}
}

func TestLinuxUnavailableDoesNotExecute(t *testing.T) {
	execCalls := 0
	sender, ok := newPlatformSender(
		"linux",
		func(string) (string, error) { return "", exec.ErrNotFound },
		func(context.Context, string, ...string) error {
			execCalls++
			return nil
		},
	)
	if ok || sender != nil {
		t.Fatalf("newPlatformSender() = (%T, %t), want (nil, false)", sender, ok)
	}
	if execCalls != 0 {
		t.Fatalf("exec calls = %d, want 0", execCalls)
	}
}

func TestWindowsSenderUsesEscapedLiteral(t *testing.T) {
	const message = "Paper '; Remove-Item\nnext\r\tline\x00"
	var argv []string
	sender, ok := newPlatformSender(
		"windows",
		func(name string) (string, error) {
			if name == "powershell" {
				return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, nil
			}
			return "", exec.ErrNotFound
		},
		func(_ context.Context, name string, args ...string) error {
			argv = append([]string{name}, args...)
			return nil
		},
	)
	if !ok {
		t.Fatal("newPlatformSender() unavailable")
	}
	sender.Send(context.Background(), message)
	if len(argv) != 5 {
		t.Fatalf("argv = %#v, want powershell plus four arguments", argv)
	}
	if want := []string{"powershell", "-NoProfile", "-NonInteractive", "-Command"}; !reflect.DeepEqual(argv[:4], want) {
		t.Fatalf("argv prefix = %#v, want %#v", argv[:4], want)
	}
	const literalCall = `$xml.CreateTextNode('Paper ''; Remove-Itemnextline')`
	if !strings.Contains(argv[4], literalCall) {
		t.Fatalf("script = %q, want literal call %q", argv[4], literalCall)
	}
	withoutLiteral := strings.Replace(argv[4], literalCall, "", 1)
	if strings.Contains(withoutLiteral, "Remove-Item") {
		t.Fatalf("script contains injected command outside the quoted literal: %q", argv[4])
	}
}

func TestLinuxSenderCancelsExecutionAfterFiveSeconds(t *testing.T) {
	var execErr error
	sender := Linux{
		mechanism: linuxNotifySend,
		Exec: func(ctx context.Context, _ string, _ ...string) error {
			<-ctx.Done()
			execErr = ctx.Err()
			return execErr
		},
	}
	before := time.Now()
	sender.Send(context.Background(), "paper is ready")
	elapsed := time.Since(before)
	if !errors.Is(execErr, context.DeadlineExceeded) {
		t.Fatalf("exec error = %v, want context deadline exceeded", execErr)
	}
	if elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("elapsed = %v, want roughly five seconds", elapsed)
	}
}

// Coalescing is durable and is tested through Router and its ledger; no
// process-local Coalescer remains.
