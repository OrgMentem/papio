// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
)

func closureRoot(cleared bool, calls *[]string) (func(args ...string) (string, error), *bytes.Buffer) {
	var out, errOut bytes.Buffer
	call := func(_ context.Context, method string, params, result any) error {
		raw, _ := json.Marshal(params)
		*calls = append(*calls, method+" "+string(raw))
		*(result.(*api.AcknowledgeClosureResult)) = api.AcknowledgeClosureResult{Source: "openalex", Cleared: cleared}
		return nil
	}
	run := func(args ...string) (string, error) {
		out.Reset()
		root := NewInProcessRoot(&out, &errOut, config.Config{}, call)
		root.SetArgs(append([]string{"config", "acknowledge-closure"}, args...))
		err := root.ExecuteContext(context.Background())
		return out.String(), err
	}
	return run, &errOut
}

func TestAcknowledgeClosureRefusesWithoutConfirm(t *testing.T) {
	var calls []string
	run, _ := closureRoot(true, &calls)
	_, err := run("openalex")
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("err = %v, want a refusal naming --confirm", err)
	}
	if len(calls) != 0 {
		t.Fatalf("daemon was called without --confirm: %v", calls)
	}
}

func TestAcknowledgeClosureCallsDaemonAndReportsWhatCleared(t *testing.T) {
	var calls []string
	run, _ := closureRoot(true, &calls)
	out, err := run("openalex", "--confirm")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != `budget.acknowledge_closure_v1 {"source":"openalex"}` {
		t.Fatalf("calls = %v", calls)
	}
	if !strings.Contains(out, "openalex egress closure acknowledged") {
		t.Fatalf("output = %q", out)
	}
}

func TestAcknowledgeClosureExitsNonzeroWhenNothingWasClosed(t *testing.T) {
	var calls []string
	run, _ := closureRoot(false, &calls)
	_, err := run("openalex", "--confirm")
	if err == nil || !strings.Contains(err.Error(), "not closed") {
		t.Fatalf("err = %v, want a nonzero exit saying the source is not closed", err)
	}
}

// Reopening spending must never be reachable by an agent: the command carries
// its own annotation, so it stays hidden even if its parent group changes.
func TestAcknowledgeClosureIsHiddenFromMCP(t *testing.T) {
	command := newAcknowledgeClosureCommand(&options{})
	if command.Annotations["mcp:hidden"] != "true" {
		t.Fatalf("annotations = %v, want mcp:hidden", command.Annotations)
	}
}
