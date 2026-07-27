// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"papio/internal/config"
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
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	seen := map[string]bool{}
	var snippets []string
	for _, pkg := range pkgs {
		ast.Inspect(pkg, func(node ast.Node) bool {
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
