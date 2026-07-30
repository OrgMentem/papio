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

// commandKind classifies how a runnable command's `--json` output is shaped.
type commandKind int

const (
	// kindEnvelope commands emit a two-key internal/agentjson.Envelope page.
	kindEnvelope commandKind = iota
	// kindStructured commands emit a single structured JSON record — possibly
	// containing arrays of its own, like `browser sessions`'s denied/takeover
	// counts or `acquire --from-zotio`'s queued/skipped lists — that is not a
	// list-shaped result and carries no `truncated` key. Legitimately exempt
	// from the envelope contract; never force-converted.
	kindStructured
	// kindNone commands have no distinct `--json` output: help-only command
	// groups (configureCommandGroups in root.go gives every non-runnable
	// group with children a RunE that only ever calls cmd.Help(), never
	// JSON) and foreground daemon processes (`papio daemon`, `papio init`,
	// `papio mcp`) that this test must never invoke.
	kindNone
)

// commandClass is one commandClassification entry. rowKey and args matter
// only for kindEnvelope: rowKey is the envelope's row key, and args are the
// extra arguments (after the command path) needed to reach the --json branch
// without daemon state, exactly as the old hardcoded table specified them.
//
// rpcMethods names every daemon method this command can reach. It is the
// other half of ADR-0001's rule — the CLI is the single source of truth for
// capabilities, so no domain fact or transition may exist on the wire without
// Cobra reachability. TestEveryDomainRPCIsReachableFromCLI walks the live
// router against the union of these, which is why an entry that names a
// method the router no longer serves is itself a failure.
type commandClass struct {
	kind       commandKind
	rowKey     string
	args       []string
	rpcMethods []string
}

// commandClassification names how every runnable command in the tree emits
// `--json`, keyed by cmd.CommandPath() ("papio jobs list"). It is the single
// source of truth TestJSONCommandTreeConformsToItsClassification walks the
// live command tree against: a runnable command missing from this map fails
// the test, so a future addition can no longer slip past the envelope
// contract undetected — exactly what happened to `browser sessions` and
// `acquire --from-zotio`, both real commands whose non-envelope JSON no
// prior test classified at all.
var commandClassification = map[string]commandClass{
	"papio init":         {kind: kindNone},
	"papio config":       {kind: kindNone},
	"papio config init":  {kind: kindStructured},
	"papio acquire":      {kind: kindStructured, rpcMethods: []string{"watch.digest_acquire", "zotio.queue", "acquire.submit_v2", "acquire.submit", "jobs.get", "zotio.lookup_works", "library.lookup_works"}},
	"papio batch":        {kind: kindNone},
	"papio batch report": {kind: kindStructured, rpcMethods: []string{"acquire.report"}},
	"papio search":       {kind: kindEnvelope, rowKey: "works", args: []string{"conformance probe"}, rpcMethods: []string{"discovery.search"}},
	"papio watch":        {kind: kindNone},
	"papio watch add":    {kind: kindStructured, rpcMethods: []string{"watch.add"}},
	"papio watch list":   {kind: kindEnvelope, rowKey: "watches", rpcMethods: []string{"watch.list"}},
	// watch digest also owns a "digest clear" subcommand; ExactArgs(1) on
	// "digest" itself accepts a bare watch id, so no daemon state is needed
	// to reach its --json path.
	"papio watch digest":               {kind: kindEnvelope, rowKey: "entries", args: []string{"1"}, rpcMethods: []string{"watch.digest"}},
	"papio watch digest clear":         {kind: kindStructured, rpcMethods: []string{"watch.digest_clear"}},
	"papio watch remove":               {kind: kindStructured, rpcMethods: []string{"watch.remove"}},
	"papio watch run":                  {kind: kindStructured, rpcMethods: []string{"watch.run"}},
	"papio jobs":                       {kind: kindNone},
	"papio jobs list":                  {kind: kindEnvelope, rowKey: "jobs", rpcMethods: []string{"jobs.list_v2", "jobs.list"}},
	"papio jobs get":                   {kind: kindStructured, rpcMethods: []string{"jobs.get"}},
	"papio jobs receipt":               {kind: kindStructured, rpcMethods: []string{"jobs.receipt"}},
	"papio jobs add-component":         {kind: kindEnvelope, rowKey: "components", args: []string{"job_01", "/tmp/papio-conformance-supplement.pdf", "--role", "supplement"}, rpcMethods: []string{"jobs.add_component"}},
	"papio jobs repair-awaiting-human": {kind: kindStructured, rpcMethods: []string{"jobs.repair_awaiting_human"}},
	"papio jobs cancel":                {kind: kindStructured, rpcMethods: []string{"jobs.cancel"}},
	"papio jobs retry":                 {kind: kindStructured, rpcMethods: []string{"jobs.retry"}},
	"papio jobs failures":              {kind: kindEnvelope, rowKey: "failures", rpcMethods: []string{"jobs.failures"}},
	"papio adapter":                    {kind: kindNone},
	"papio adapter diagnose":           {kind: kindStructured, rpcMethods: []string{"jobs.get", "ping"}},
	"papio adapter captures":           {kind: kindEnvelope, rowKey: "captures", rpcMethods: []string{"adapter.captures.list"}},
	"papio adapter captures purge":     {kind: kindStructured, rpcMethods: []string{"adapter.captures.purge"}},
	"papio status":                     {kind: kindStructured, rpcMethods: []string{"zotio.missing_count", "jobs.list", "jobs.get"}},
	"papio stats":                      {kind: kindStructured, rpcMethods: []string{"stats.get"}},
	"papio actions":                    {kind: kindNone},
	"papio actions list":               {kind: kindEnvelope, rowKey: "actions", rpcMethods: []string{"actions.list_v2", "actions.list"}},
	"papio actions resolve":            {kind: kindStructured, rpcMethods: []string{"actions.resolve"}},
	"papio actions open":               {kind: kindEnvelope, rowKey: "urls", args: []string{"--dry-run"}, rpcMethods: []string{"actions.list", "jobs.list_v2", "jobs.list", "actions.open"}},
	"papio browser":                    {kind: kindNone},
	"papio browser sessions":           {kind: kindStructured, rpcMethods: []string{"browser.sessions"}},
	"papio browser use":                {kind: kindStructured, rpcMethods: []string{"browser.sessions", "browser.claim"}},
	"papio inbox":                      {kind: kindStructured, rpcMethods: []string{"triage.snapshot"}},
	"papio inbox counts":               {kind: kindStructured, rpcMethods: []string{"triage.counts"}},
	"papio inbox decide":               {kind: kindStructured, rpcMethods: []string{"triage.decide"}},
	"papio artifacts":                  {kind: kindNone},
	"papio artifacts get":              {kind: kindStructured, rpcMethods: []string{"artifacts.get"}},
	"papio bundle":                     {kind: kindNone},
	"papio bundle export":              {kind: kindStructured, rpcMethods: []string{"bundle.export"}},
	"papio doctor":                     {kind: kindStructured, rpcMethods: []string{"ping", "doctor.run"}},
	"papio zotio":                      {kind: kindNone},
	"papio zotio preflight":            {kind: kindStructured, rpcMethods: []string{"zotio.preflight"}},
	"papio zotio plan":                 {kind: kindStructured, rpcMethods: []string{"zotio.plan"}},
	"papio zotio apply":                {kind: kindStructured, rpcMethods: []string{"zotio.apply"}},
	"papio zotio tags":                 {kind: kindNone},
	"papio zotio tags reconcile":       {kind: kindStructured, rpcMethods: []string{"zotio.tags.reconcile"}},
	"papio daemon":                     {kind: kindNone},
	"papio daemon stop":                {kind: kindStructured, rpcMethods: []string{"daemon.shutdown"}},
	"papio daemon status":              {kind: kindStructured, rpcMethods: []string{"ping"}},
	"papio native-host":                {kind: kindNone},
	"papio native-host install":        {kind: kindStructured},
	"papio native-host uninstall":      {kind: kindStructured},
	"papio native-host status":         {kind: kindStructured},
	"papio mcp":                        {kind: kindNone},
	"papio version":                    {kind: kindStructured},
}

// TestJSONCommandTreeConformsToItsClassification is the enforcement half of
// the L1 fix, and — unlike the fixed table it replaces — a genuine walk of
// the live command tree: every runnable command (leaf or not; `watch digest`
// is both) must appear in commandClassification, and every kindEnvelope
// command must emit the two-key internal/agentjson.Envelope shape —
// {"<rowKey>": [...], "truncated": bool} — never a bare top-level array (or,
// as several of these commands emitted before internal/agentjson existed, a
// literal `null`).
func TestJSONCommandTreeConformsToItsClassification(t *testing.T) {
	root := NewInProcessRoot(&bytes.Buffer{}, &bytes.Buffer{}, config.Config{}, nilRPC)
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	var walk func(parent *cobra.Command, args []string)
	walk = func(parent *cobra.Command, args []string) {
		for _, cmd := range parent.Commands() {
			if cmd.Name() == "help" || cmd.Name() == "completion" {
				continue
			}
			path := cmd.CommandPath()
			childArgs := append(append([]string{}, args...), cmd.Name())
			if cmd.RunE != nil || cmd.Run != nil {
				class, ok := commandClassification[path]
				if !ok {
					t.Errorf("%s: runnable command has no commandClassification entry — classify it kindEnvelope, kindStructured, or kindNone", path)
				} else if class.kind == kindEnvelope {
					t.Run(path, func(t *testing.T) {
						assertJSONEnvelopeCommand(t, path, childArgs, class)
					})
				}
			}
			walk(cmd, childArgs)
		}
	}
	walk(root, nil)
}

// assertJSONEnvelopeCommand invokes one kindEnvelope command against a fresh
// nilRPC-backed root and asserts its --json payload is a conformant
// internal/agentjson.Envelope page.
func assertJSONEnvelopeCommand(t *testing.T, path string, args []string, class commandClass) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, nilRPC)
	root.SetArgs(append([]string{"--json"}, append(args, class.args...)...))
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("%s: %v (stderr: %s)", path, err, errOut.String())
	}
	assertEnvelope(t, path, class.rowKey, out.Bytes())
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
