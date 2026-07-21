// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"papio/internal/browser"
	"papio/internal/config"
)

func TestBrowserUseLatestPicksNewestPendingSession(t *testing.T) {
	var claimed string
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		switch method {
		case "browser.sessions":
			*result.(*browserSessionsResult) = browserSessionsResult{Sessions: []browser.SessionSummary{
				{ID: "aaaa1111aaaa1111", Holder: true, ExtensionVersion: "0.3.1", LastSyncAt: "2026-07-21T12:00:05Z"},
				// Non-holder sessions arrive newest-sync first, per Bridge.Sessions.
				{ID: "bbbb2222bbbb2222", Holder: false, ExtensionVersion: "0.4.0", LastSyncAt: "2026-07-21T12:00:04Z"},
				{ID: "cccc3333cccc3333", Holder: false, ExtensionVersion: "0.2.0", LastSyncAt: "2026-07-21T11:00:00Z"},
			}}
			return nil
		case "browser.claim":
			claimed = params.(map[string]string)["session_id"]
			*result.(*map[string]any) = map[string]any{"claimed": true}
			return nil
		default:
			t.Fatalf("unexpected method %q", method)
			return nil
		}
	})
	root.SetArgs([]string{"browser", "use", "--latest"})
	if err := root.Execute(); err != nil {
		t.Fatalf("browser use --latest: %v (%s)", err, stderr.String())
	}
	if claimed != "bbbb2222bbbb2222" {
		t.Fatalf("claimed = %q, want the newest pending session", claimed)
	}
}

func TestBrowserUseRequiresExactlyOneSelector(t *testing.T) {
	for _, args := range [][]string{
		{"browser", "use"},
		{"browser", "use", "abc123", "--latest"},
	} {
		var stdout, stderr bytes.Buffer
		root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, _ any, _ any) error {
			return errors.New("no RPC expected for " + method)
		})
		root.SetArgs(args)
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("args %v: err = %v, want selector error", args, err)
		}
	}
}

func TestBrowserUseLatestWithoutPendingErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "browser.sessions" {
			t.Fatalf("unexpected method %q", method)
		}
		*result.(*browserSessionsResult) = browserSessionsResult{Sessions: []browser.SessionSummary{
			{ID: "aaaa1111aaaa1111", Holder: true, ExtensionVersion: "0.4.0"},
		}}
		return nil
	})
	root.SetArgs([]string{"browser", "use", "--latest"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "no other browser session") {
		t.Fatalf("err = %v, want no-other-session error", err)
	}
}
