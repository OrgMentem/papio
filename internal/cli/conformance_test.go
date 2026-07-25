// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"papio/internal/config"
)

// This file mechanizes the manual `--json` surface sweep that produced
// dev/field-report-acquisition-ux-2026-07-21.md finding L1: an agent parser
// broke because papio's machine-readable surfaces have no single shape.
// internal/agentjson is now the one contract every list command must honor;
// see its doc comment for the reasoning this file does not repeat.
//
// The seam that makes a payload-agnostic sweep possible: an in-process root
// whose RPC stub returns nil without touching result leaves every command
// rendering its Go zero value, so a list command renders an empty list
// without the test needing to know any payload schema.

// nilRPC is the stub every conformance case in this file uses: it never
// populates result, so each command renders whatever its zero value prints.
func nilRPC(context.Context, string, any, any) error { return nil }

// Known blind spot, worth stating plainly: because nilRPC leaves result at its
// zero value, an `omitempty` field on a command's JSON struct is absent here and
// the payload can look conformant while a real invocation emits an extra key.
// `jobs failures` shipped exactly that bug — a `since,omitempty` metadata field
// that only appeared once the daemon populated it. Adding a metadata key to a
// page struct therefore will NOT be caught by this test; keep pages to rows plus
// truncated and put anything else outside the page.

// TestJSONListCommandsUseTheAgentJSONEnvelope is the enforcement half of the
// L1 fix: every pure-list `--json` payload must be a two-key envelope object
// — {"<rowKey>": [...], "truncated": bool} — never a bare top-level array
// (or, as several of these commands emit today, a literal `null`). Row keys
// below follow the names internal/agentjson's own doc comment and the
// existing MCP resource envelopes already use ("jobs", "works", "watches"),
// or the field name the command's own result struct already picked
// ("failures", "entries").
func TestJSONListCommandsUseTheAgentJSONEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		rowKey string
	}{
		{name: "search", args: []string{"search", "conformance probe"}, rowKey: "works"},
		{name: "jobs list", args: []string{"jobs", "list"}, rowKey: "jobs"},
		{name: "jobs failures", args: []string{"jobs", "failures"}, rowKey: "failures"},
		{name: "actions list", args: []string{"actions", "list"}, rowKey: "actions"},
		{name: "actions open --dry-run", args: []string{"actions", "open", "--dry-run"}, rowKey: "urls"},
		{name: "watch list", args: []string{"watch", "list"}, rowKey: "watches"},
		// watch digest also owns a "digest clear" subcommand; ExactArgs(1) on
		// "digest" itself accepts a bare watch id, so no daemon state is needed
		// to reach its --json path.
		{name: "watch digest", args: []string{"watch", "digest", "1"}, rowKey: "entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, nilRPC)
			root.SetArgs(append([]string{"--json"}, tc.args...))
			label := "papio " + strings.Join(tc.args, " ")
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("%s: %v (stderr: %s)", label, err, errOut.String())
			}
			assertEnvelope(t, label, tc.rowKey, out.Bytes())
		})
	}
}

// TestJSONStructuredCommandsStayPlainObjects documents the boundary
// internal/agentjson draws deliberately: `jobs get` and `doctor` already
// return a structured object with room to grow, so they are NOT envelopes.
// Only bare top-level arrays are the L1 defect. This test exists so nobody
// "fixes" these into {"...": ..., "truncated": bool} shapes later.
func TestJSONStructuredCommandsStayPlainObjects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		allowErr bool // doctor legitimately fails readiness against an all-zero ping stub
	}{
		{name: "jobs get", args: []string{"jobs", "get", "job_01"}},
		{name: "doctor", args: []string{"doctor"}, allowErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, nilRPC)
			root.SetArgs(append([]string{"--json"}, tc.args...))
			label := "papio " + strings.Join(tc.args, " ")
			// doctor writes its JSON report before evaluating readiness, so an
			// all-zero daemon ping still leaves a decodable payload behind even
			// though the command itself reports errDoctorFailed.
			if err := root.ExecuteContext(context.Background()); err != nil && !tc.allowErr {
				t.Fatalf("%s: %v (stderr: %s)", label, err, errOut.String())
			}
			var decoded any
			if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
				t.Fatalf("%s --json produced invalid JSON: %v (payload: %s)", label, err, out.Bytes())
			}
			if _, ok := decoded.(map[string]any); !ok {
				t.Fatalf("%s --json decoded as %T, want a plain JSON object — this command is documented to stay outside internal/agentjson.Envelope, not become one", label, decoded)
			}
		})
	}
}

// TestCommandTreeHygiene walks the full cobra tree and asserts every command
// is reachable and self-describing: a non-empty Short with no trailing
// period (the convention every existing Short in this repo already follows),
// and either subcommands or a RunE — a command with neither is invisible
// dead weight nobody can invoke.
func TestCommandTreeHygiene(t *testing.T) {
	root := NewInProcessRoot(&bytes.Buffer{}, &bytes.Buffer{}, config.Config{}, nilRPC)
	// Force cobra's built-in help/completion commands into existence so the
	// walk below can name and skip them explicitly, regardless of whether
	// Execute has run yet.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, cmd := range parent.Commands() {
			if cmd.Name() == "help" || cmd.Name() == "completion" {
				continue
			}
			path := cmd.CommandPath()
			short := strings.TrimSpace(cmd.Short)
			if short == "" {
				t.Errorf("%s: Short is empty — every command needs a one-line description for --help and the MCP tool listing", path)
			} else if strings.HasSuffix(short, ".") {
				t.Errorf("%s: Short %q ends with a period; no Short in this command tree does — keep the convention consistent", path, cmd.Short)
			}
			hasChildren := len(cmd.Commands()) > 0
			runnable := cmd.RunE != nil || cmd.Run != nil
			if !hasChildren && !runnable {
				t.Errorf("%s: has neither subcommands nor a RunE — it is unreachable dead weight in the command tree", path)
			}
			walk(cmd)
		}
	}
	walk(root)
}

// TestUnknownCommandsFailWithAnActionableMessage covers field-report finding
// L2 ("papio jobs show silently does nothing" — the real verb is "jobs get")
// and guards against finding H1's bare "papio: exit status 1" resurfacing on
// the command-routing path: an unrecognized token must produce an error that
// names the token, not an opaque process exit code.
func TestUnknownCommandsFailWithAnActionableMessage(t *testing.T) {
	bareExitStatus := regexp.MustCompile(`^(papio: )?exit status \d+$`)
	for _, tc := range []struct {
		name     string
		args     []string
		mustName string
	}{
		{name: "unknown top-level command", args: []string{"frobnicate"}, mustName: "frobnicate"},
		{name: "unknown subcommand", args: []string{"jobs", "show"}, mustName: "show"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(context.Context, string, any, any) error {
				t.Fatal("an unresolved command must never reach the daemon")
				return nil
			})
			root.SetArgs(tc.args)
			label := "papio " + strings.Join(tc.args, " ")
			err := root.ExecuteContext(context.Background())
			if err == nil {
				t.Fatalf("%s succeeded, want an error naming the unresolved token", label)
			}
			if !strings.Contains(err.Error(), tc.mustName) {
				t.Fatalf("%s error = %q, want it to name %q", label, err, tc.mustName)
			}
			if bareExitStatus.MatchString(err.Error()) {
				t.Fatalf("%s error = %q — a bare exit-status message with no context (field-report finding H1)", label, err)
			}
		})
	}
}

// assertEnvelope fails t with a message that teaches the internal/agentjson
// contract on any mismatch: parses as a JSON object (not an array — the L1
// defect — and not null), has exactly two keys, one "truncated" bool and one
// rowKey holding a non-null JSON array.
func assertEnvelope(t *testing.T, label, rowKey string, payload []byte) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("%s --json produced invalid JSON: %v (payload: %s)", label, err, payload)
	}
	switch v := decoded.(type) {
	case nil:
		t.Fatalf("%s --json payload is JSON null, want an envelope object {%q: [...], \"truncated\": bool} — see internal/agentjson.Envelope", label, rowKey)
	case []any:
		t.Fatalf("%s --json payload is a bare top-level array with %d row(s) — this is exactly the L1 field-report defect: an agent parser has nowhere to put \"was this truncated\" and no room to add a field later. Wrap the rows with internal/agentjson.Envelope(%q, rows, truncated) instead", label, len(v), rowKey)
	case map[string]any:
		assertEnvelopeObject(t, label, rowKey, v)
	default:
		t.Fatalf("%s --json payload decoded as %T, want a JSON object — see internal/agentjson.Envelope", label, decoded)
	}
}

func assertEnvelopeObject(t *testing.T, label, rowKey string, obj map[string]any) {
	t.Helper()
	if len(obj) != 2 {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("%s --json payload has %d key(s) %v, want exactly 2: %q and \"truncated\" — internal/agentjson.Envelope is the single source of truth for this shape", label, len(obj), keys, rowKey)
	}
	truncatedRaw, ok := obj["truncated"]
	if !ok {
		t.Fatalf(`%s --json payload has no "truncated" key — every agentjson.Envelope page reports it, even when false`, label)
	}
	if _, ok := truncatedRaw.(bool); !ok {
		t.Fatalf("%s --json \"truncated\" = %#v (%T), want a bool", label, truncatedRaw, truncatedRaw)
	}
	rows, hasRowKey := obj[rowKey]
	if !hasRowKey {
		other := make([]string, 0, 1)
		for k := range obj {
			if k != "truncated" {
				other = append(other, k)
			}
		}
		t.Fatalf("%s --json payload has no %q key (row key drifted); got %v instead — agentjson.Envelope keys rows by the collection name matching the command", label, rowKey, other)
	}
	if rows == nil {
		t.Fatalf("%s --json %q is JSON null, want [] — agentjson.Envelope/Truncate normalize a nil slice to an empty array", label, rowKey)
	}
	if _, ok := rows.([]any); !ok {
		t.Fatalf("%s --json %q = %#v (%T), want a JSON array", label, rowKey, rows, rows)
	}
}
