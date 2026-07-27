// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/errcat"
	"papio/internal/job"
	"papio/internal/work"
)

// internal/errcat is the one catalog `papio status`, `papio acquire --wait`,
// and the MCP status tool all read their "here is what to do next" line from,
// so a typo there is the single highest-blast-radius string in the product:
// it is the sentence a stuck user actually follows. Three of them shipped
// `papio actions --open` — a flag that does not exist, so the recommended
// recovery for every parked browser handoff exited 1 with "unknown flag".
// Prose alone cannot be tested, but the commands quoted inside it can: this
// walks every backticked `papio …` snippet in the errcat package's string
// literals and resolves it against the live cobra tree.
var papioCommandInBackticks = regexp.MustCompile("`(papio [^`]+)`")

func TestErrcatGuidanceQuotesOnlyRealCommands(t *testing.T) {
	root := NewInProcessRoot(&bytes.Buffer{}, &bytes.Buffer{}, config.Config{}, nilRPC)
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	snippets := backtickedPapioCommands(t, "../errcat")
	if len(snippets) == 0 {
		t.Fatal("no backticked papio commands found in internal/errcat — the scan broke, not the catalog")
	}
	for _, snippet := range snippets {
		t.Run(snippet, func(t *testing.T) {
			if err := resolveGuidanceCommand(root, snippet); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// backtickedPapioCommands returns every `papio …` snippet appearing inside a
// string literal in the package at dir. Comments are skipped deliberately:
// doc comments name commands illustratively ("the acquire --wait twin") and
// are not what a user is told to type.
func backtickedPapioCommands(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	var snippets []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, match := range papioCommandInBackticks.FindAllStringSubmatch(value, -1) {
				if snippet := strings.TrimSpace(match[1]); !seen[snippet] {
					seen[snippet] = true
					snippets = append(snippets, snippet)
				}
			}
			return true
		})
	}
	return snippets
}

// resolveGuidanceCommand returns an error unless snippet names a command in
// the tree and every flag it passes exists on that command. Placeholder
// operands (`<action-id>`) are ignored: only the verb path and the flags are
// contractual.
func resolveGuidanceCommand(root *cobra.Command, snippet string) error {
	tokens := strings.Fields(snippet)[1:] // drop the "papio" prefix
	cmd, rest, err := root.Find(tokens)
	if err != nil {
		return fmt.Errorf("%q does not resolve to a command: %w", snippet, err)
	}
	if cmd == root {
		return fmt.Errorf("%q resolves to the bare root — guidance must name a subcommand", snippet)
	}
	for _, arg := range rest {
		if !strings.HasPrefix(arg, "-") {
			continue // a positional operand or placeholder
		}
		name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
		if cmd.Flags().Lookup(name) == nil && cmd.InheritedFlags().Lookup(name) == nil {
			return fmt.Errorf("%q passes --%s, which %s does not define", snippet, name, cmd.CommandPath())
		}
	}
	return nil
}

// TestGuidanceResolverRejectsTheFlagThatShipped guards the guard: the scan
// above only earns its keep if it would have failed on the exact string the
// catalog carried. `papio actions --open` parses as the `actions` group plus
// an undefined flag, which is why it exited 1 instead of printing help.
func TestGuidanceResolverRejectsTheFlagThatShipped(t *testing.T) {
	root := NewInProcessRoot(&bytes.Buffer{}, &bytes.Buffer{}, config.Config{}, nilRPC)
	if err := resolveGuidanceCommand(root, "papio actions --open"); err == nil {
		t.Fatal("`papio actions --open` accepted, so the catalog scan cannot catch the bug it exists for")
	}
}

// TestActionGuidanceCommandsApplyToEveryActionKind exercises the kinds the
// daemon can actually record. A resolvable cobra path is not enough: an
// `actions open` recommendation must produce a browser target for that action.
func TestActionGuidanceCommandsApplyToEveryActionKind(t *testing.T) {
	row := job.Row{Work: work.Work{DOI: "10.1000/action-guidance"}}
	baseFor := func(string) (string, bool) {
		return "https://resolver.example.test/openurl", true
	}
	kinds := []string{
		"openurl_handoff",
		"manual_download",
		"verify_identity",
		"human_auth_required",
		"terms_acceptance_required",
		"openurl_available",
	}
	for _, kind := range kinds {
		for _, requiresAuth := range []bool{false, true} {
			authName := "false"
			if requiresAuth {
				authName = "true"
			}
			t.Run(kind+"/requires_auth="+authName, func(t *testing.T) {
				action := job.HumanAction{Kind: kind, RequiresAuth: requiresAuth}
				next := app.HumanActionNextStepFor(action)
				hintAction := action
				hintAction.BlockedBy = "paywall"
				if got, want := strings.Contains(accessHint(hintAction), "papio actions open"), next.RequiresInstitutionalLogin && next.Command == "papio actions open"; got != want {
					t.Fatalf("access hint names `papio actions open` = %t, want %t", got, want)
				}
				switch command := next.Command; command {
				case "":
				case "papio actions open":
					if _, ok := actionURL(action, row, baseFor); !ok {
						t.Fatalf("%q cannot act on %s with requires_auth=%t", command, kind, requiresAuth)
					}
				default:
					t.Fatalf("no applicability check for next-step command %q", command)
				}
			})
		}
	}
}

// TestCurrentErrcatGuidanceCommandsApplyToActions exercises the emitted
// state/reason branches as well as the source literals. A replacement action
// can make a once-valid handoff command inapplicable after the job parks.
func TestCurrentErrcatGuidanceCommandsApplyToActions(t *testing.T) {
	root := NewInProcessRoot(&bytes.Buffer{}, &bytes.Buffer{}, config.Config{}, nilRPC)
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	row := job.Row{Work: work.Work{DOI: "10.1000/current-guidance"}}
	baseFor := func(string) (string, bool) {
		return "https://resolver.example.test/openurl", true
	}
	cases := []struct {
		name     string
		state    string
		reason   string
		actions  []job.HumanAction
		action   job.HumanAction
		wantOpen bool
	}{
		{
			name:     "historical institutional handoff",
			state:    job.StateAwaitingHuman,
			reason:   "institutional_handoff",
			action:   job.HumanAction{Kind: "openurl_handoff", RequiresAuth: true},
			wantOpen: true,
		},
		{
			name:     "historical open access handoff",
			state:    job.StateAwaitingHuman,
			reason:   "open_access_browser_handoff",
			action:   job.HumanAction{Kind: "openurl_handoff"},
			wantOpen: true,
		},
		{
			name:   "manual replacement of login-required park",
			state:  job.StateAwaitingHuman,
			reason: "login_required",
			actions: []job.HumanAction{{
				ID: 228, Kind: "manual_download", Status: "open", RequiresAuth: true, BlockedBy: "landing_page",
			}},
			action:   job.HumanAction{Kind: "manual_download", RequiresAuth: true},
			wantOpen: false,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			exp := errcat.ExplainWithOpenAction(test.state, test.reason, "", "", test.actions, config.Config{})
			opens := strings.Contains(exp.Guidance, "`papio actions open`")
			if opens != test.wantOpen {
				t.Fatalf("guidance names `papio actions open` = %t, want %t: %q", opens, test.wantOpen, exp.Guidance)
			}
			for _, match := range papioCommandInBackticks.FindAllStringSubmatch(exp.Guidance, -1) {
				snippet := match[1]
				if err := resolveGuidanceCommand(root, snippet); err != nil {
					t.Fatal(err)
				}
				if snippet != "papio actions open" {
					continue
				}
				if _, ok := actionURL(test.action, row, baseFor); !ok {
					t.Fatalf("%q cannot act on current %s action", snippet, test.action.Kind)
				}
			}
		})
	}
}
