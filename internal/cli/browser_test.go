// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
			// The RunE decodes into an anonymous struct; round-trip through
			// JSON instead of type-asserting a private shape.
			encoded, err := json.Marshal(map[string]any{"claimed": true, "session_id": claimed})
			if err != nil {
				return err
			}
			return json.Unmarshal(encoded, result)
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

func TestBrowserReloadAttributesObservedHolder(t *testing.T) {
	holderBefore := browser.SessionSummary{ID: "holder-old", Holder: true, ExtensionVersion: "0.15.0"}
	siblingBefore := browser.SessionSummary{ID: "sibling-old", ExtensionVersion: "0.15.0"}
	holderAfter := browser.SessionSummary{ID: "holder-new", Holder: true, ExtensionVersion: "0.15.0"}
	siblingAfter := browser.SessionSummary{ID: "sibling-old", ExtensionVersion: "0.15.0"}

	initialSessionsErr := errors.New("initial sessions unavailable")
	reloadVersionErr := errors.New("browser extension v0.14.0 does not support dev_reload (needs v0.15.0)")
	observationErr := errors.New("observed sessions unavailable")

	tests := []struct {
		name            string
		args            []string
		before          []browser.SessionSummary
		after           []browser.SessionSummary
		initialErr      error
		reloadErr       error
		observationErr  error
		cancelContext   bool
		wantErr         error
		wantErrContains string
		wantOutContains []string
	}{
		{
			name:            "attributed success",
			before:          []browser.SessionSummary{holderBefore, siblingBefore},
			after:           []browser.SessionSummary{holderAfter, siblingAfter},
			wantOutContains: []string{"holder-old", "holder-new"},
		},
		{
			name:            "already-known sibling theft",
			before:          []browser.SessionSummary{holderBefore, siblingBefore},
			after:           []browser.SessionSummary{{ID: "sibling-old", Holder: true}, {ID: "holder-new"}},
			wantErrContains: "already-connected browser",
		},
		{
			name:            "new holder is ambiguous when sibling disappeared",
			before:          []browser.SessionSummary{holderBefore, siblingBefore},
			after:           []browser.SessionSummary{{ID: "sibling-new", Holder: true}},
			wantErrContains: "cannot be attributed",
		},
		{
			name:            "zero timeout reports dispatch without observation",
			args:            []string{"--timeout=0"},
			before:          []browser.SessionSummary{holderBefore},
			wantOutContains: []string{"reload-123", "holder-old", "not waiting"},
		},
		{
			name:            "no holder",
			before:          []browser.SessionSummary{{ID: "sibling-old"}},
			wantErrContains: "no browser session holds",
		},
		{
			name:       "initial session observation failure",
			before:     []browser.SessionSummary{holderBefore},
			initialErr: initialSessionsErr,
			wantErr:    initialSessionsErr,
		},
		{
			name:            "unsupported extension reload failure",
			before:          []browser.SessionSummary{holderBefore},
			reloadErr:       reloadVersionErr,
			wantErr:         reloadVersionErr,
			wantErrContains: "needs v0.15.0",
		},
		{
			name:           "reconnect observation failure",
			before:         []browser.SessionSummary{holderBefore},
			observationErr: observationErr,
			wantErr:        observationErr,
		},
		{
			name:          "cancelled wait",
			before:        []browser.SessionSummary{holderBefore},
			cancelContext: true,
			wantErr:       context.Canceled,
		},
		{
			name:            "no replacement holder before timeout",
			args:            []string{"--timeout=1ms"},
			before:          []browser.SessionSummary{holderBefore},
			after:           nil,
			wantErrContains: "did not reconnect",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			sessionReads := 0
			root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
				switch method {
				case "browser.sessions":
					sessionReads++
					if sessionReads == 1 {
						if tc.initialErr != nil {
							return tc.initialErr
						}
						*result.(*browserSessionsResult) = browserSessionsResult{Sessions: tc.before}
						return nil
					}
					if tc.observationErr != nil {
						return tc.observationErr
					}
					*result.(*browserSessionsResult) = browserSessionsResult{Sessions: tc.after}
					return nil
				case "browser.dev_reload":
					if tc.reloadErr != nil {
						return tc.reloadErr
					}
					encoded, err := json.Marshal(map[string]string{
						"session_id": "holder-old",
						"reload_id":  "reload-123",
					})
					if err != nil {
						return err
					}
					return json.Unmarshal(encoded, result)
				default:
					t.Fatalf("unexpected method %q", method)
					return nil
				}
			})
			args := []string{"browser", "reload", "--timeout=1s"}
			if tc.args != nil {
				args = append([]string{"browser", "reload"}, tc.args...)
			}
			root.SetArgs(args)

			ctx := context.Background()
			if tc.cancelContext {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			err := root.ExecuteContext(ctx)

			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErrContains != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrContains)) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErrContains)
			}
			if tc.wantErr == nil && tc.wantErrContains == "" && err != nil {
				t.Fatalf("browser reload: %v (stderr: %s)", err, stderr.String())
			}
			for _, substring := range tc.wantOutContains {
				if !strings.Contains(stdout.String(), substring) {
					t.Fatalf("stdout = %q, want substring %q", stdout.String(), substring)
				}
			}
		})
	}
}

type browserReloadErrorWriter struct {
	err error
}

func (w browserReloadErrorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestBrowserReloadPropagatesOutputFailure(t *testing.T) {
	outputErr := errors.New("reload output unavailable")
	for _, tc := range []struct {
		name  string
		args  []string
		after []browser.SessionSummary
	}{
		{
			name: "dispatch without waiting",
			args: []string{"browser", "reload", "--timeout=0"},
		},
		{
			name:  "attributed reconnect",
			args:  []string{"browser", "reload", "--timeout=1s"},
			after: []browser.SessionSummary{{ID: "holder-new", Holder: true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessionReads := 0
			var stderr bytes.Buffer
			root := NewInProcessRoot(browserReloadErrorWriter{err: outputErr}, &stderr, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
				switch method {
				case "browser.sessions":
					sessionReads++
					sessions := []browser.SessionSummary{{ID: "holder-old", Holder: true}}
					if sessionReads > 1 {
						sessions = tc.after
					}
					*result.(*browserSessionsResult) = browserSessionsResult{Sessions: sessions}
					return nil
				case "browser.dev_reload":
					encoded, err := json.Marshal(map[string]string{
						"session_id": "holder-old",
						"reload_id":  "reload-123",
					})
					if err != nil {
						return err
					}
					return json.Unmarshal(encoded, result)
				default:
					t.Fatalf("unexpected method %q", method)
					return nil
				}
			})
			root.SetArgs(tc.args)
			if err := root.Execute(); !errors.Is(err, outputErr) {
				t.Fatalf("error = %v, want %v", err, outputErr)
			}
		})
	}
}
