// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/ipc"
	"papio/internal/protocol"
)

func TestPulseCommandRendersStructuredObject(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "work.pulse_v1" {
			t.Fatalf("method = %q", method)
		}
		p := result.(*api.PulseResult)
		complete, total, moving, zero, scheduled := true, int64(2), int64(1), int64(0), int64(1)
		p.Schema = 1
		p.GeneratedAt = "2026-08-12T12:00:00Z"
		p.ProjectionComplete = &complete
		p.NonterminalTotal = &total
		p.InFlight = &moving
		p.Continuing = &zero
		p.Scheduled = &scheduled
		p.WaitingRequired = &zero
		p.Stalled = &zero
		return nil
	})
	root.SetArgs([]string{"--json", "pulse"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"schema":1`) || !strings.Contains(out.String(), `"in_flight":1`) {
		t.Fatalf("output = %s", out.String())
	}
}

func TestPulseCommandOldDaemonIsExplicitlyUnavailable(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, _ any, _ any) error {
		if method != "work.pulse_v1" {
			t.Fatalf("method = %q", method)
		}
		return &ipc.RemoteError{Code: "unknown_method", Message: "unknown method"}
	})
	root.SetArgs([]string{"--json", "pulse"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != `{"available":false}` {
		t.Fatalf("output = %q", got)
	}
}

func TestPulseTextUsesPlanVocabulary(t *testing.T) {
	var out bytes.Buffer
	zero, one, total, complete := int64(0), int64(1), int64(1), true
	s := protocol.WorkPulseResponsePayload{Schema: 1, GeneratedAt: "2026-08-12T12:00:00Z", ProjectionComplete: &complete, NonterminalTotal: &total, InFlight: &zero, Continuing: &zero, Scheduled: &one, WaitingRequired: &zero, Stalled: &zero}
	if err := renderPulseText(&out, s); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "Scheduled") {
		t.Fatalf("output = %q", out.String())
	}
}
