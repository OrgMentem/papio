// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const presenceRegistry = `{"meta":{"resource_type":"capabilities"},"results":[
	{"path":"desktop status","operation":"read"},{"path":"desktop wait","operation":"read"}]}`

func TestDecodeDesktopStatusRequiresRunning(t *testing.T) {
	status, err := decodeDesktopStatus([]byte(`{"running":true,"connector_reachable":false,"state":"starting","evidence":"profile_lock","profiles":[{"path":"/p","lock":"held"}]}`))
	if err != nil || !status.Running || status.Ready() || status.State != "starting" {
		t.Fatalf("starting desktop = %+v, %v; want running and not ready", status, err)
	}
	if _, err := decodeDesktopStatus([]byte(`{"connector_reachable":true}`)); err == nil {
		t.Fatal("a status without running decoded; an unrelated document must not read as a presence answer")
	}
}

// An older zotio lacks the presence commands. papio asks its registry once
// and then answers from the cache, so a closed-Zotero check costs no process;
// a later preflight that sees an upgraded registry turns presence on.
func TestDesktopStatusUnsupportedByOlderZotioIsCachedUntilPreflightSeesUpgrade(t *testing.T) {
	registry := `{"results":[{"path":"sync","operation":"sync"}]}`
	var calls []string
	client := &Client{Executable: "zotio", Exec: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch strings.Join(args, " ") {
		case "capabilities":
			return []byte(registry), nil
		case "desktop status --agent":
			return []byte(`{"running":false,"connector_reachable":false,"state":"stopped","evidence":"none"}`), nil
		}
		return nil, errors.New("unexpected " + strings.Join(args, " "))
	}}
	for range 3 {
		if _, err := client.DesktopStatus(context.Background()); !errors.Is(err, ErrDesktopPresenceUnsupported) {
			t.Fatalf("older zotio status err = %v, want ErrDesktopPresenceUnsupported", err)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("zotio ran %v, want one registry read", calls)
	}
	client.recordDesktopPresence(map[string]Capability{"desktop status": {}, "desktop wait": {}})
	status, err := client.DesktopStatus(context.Background())
	if err != nil || status.Running {
		t.Fatalf("upgraded zotio status = %+v, %v", status, err)
	}
}

func TestPreflightRecordsDesktopPresenceWithoutRequiringIt(t *testing.T) {
	client := &Client{Executable: "zotio", Exec: func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "version" {
			return []byte("zotio 0.27.0\n"), nil
		}
		return []byte(`{"results":[
			{"path":"items missing-pdf","operation":"read"},{"path":"items get","operation":"read"},
			{"path":"attachments add","operation":"write","write_target":"web_api"},
			{"path":"import scan","operation":"read"},{"path":"import resolve","operation":"read"},
			{"path":"import apply","operation":"write"},{"path":"items add-to-collection","operation":"write"},
			{"path":"sync","operation":"sync"}]}`), nil
	}}
	if _, err := client.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight without presence commands = %v, want pass (they are optional)", err)
	}
	if client.desktopPresence.Load() != desktopPresenceUnsupported {
		t.Fatal("preflight did not record that this zotio lacks presence")
	}
}

func TestWaitForDesktopOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		stdout    string
		err       error
		wantReady bool
		wantErr   string
	}{
		"ready":         {stdout: `{"running":true,"connector_reachable":true,"state":"ready","outcome":"ready","waited_ms":1200}`, wantReady: true},
		"timeout":       {stdout: `{"running":false,"connector_reachable":false,"state":"stopped","outcome":"timeout"}`, err: errors.New("exit status 14")},
		"unresponsive":  {stdout: `{"running":true,"connector_reachable":false,"state":"unresponsive","outcome":"unresponsive"}`, err: errors.New("exit status 15")},
		"connector off": {stdout: `{"running":true,"connector_reachable":false,"state":"connector_off","outcome":"connector_off"}`, err: errors.New("exit status 15")},
		"no profile":    {stdout: `{"running":false,"connector_reachable":false,"state":"stopped","outcome":"no_profile"}`, err: errors.New("exit status 9"), wantErr: "no_profile"},
		"signalled":     {err: errors.New("exit status 1"), wantErr: "exit status 1"},
	} {
		t.Run(name, func(t *testing.T) {
			var waitArgs string
			client := &Client{Executable: "zotio", Exec: func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "capabilities" {
					return []byte(presenceRegistry), nil
				}
				waitArgs = strings.Join(args, " ")
				return []byte(tc.stdout), tc.err
			}}
			status, err := client.WaitForDesktop(context.Background())
			if waitArgs != "desktop wait --agent --watch-stdin --timeout 1h0m0s" {
				t.Fatalf("waiter argv = %q", waitArgs)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || status.Ready() != tc.wantReady {
				t.Fatalf("status = %+v, %v; want ready=%v", status, err, tc.wantReady)
			}
		})
	}
}
