// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/spf13/cobra"
)

// testFactory mimics papio's tree: commands render to the writer captured at
// construction (not cmd.OutOrStdout), a persistent --json flag, a read-only
// command with a local flag, a failing command, a hidden subtree, and a
// runnable leaf under a non-runnable parent.
func testFactory() RootFactory {
	return func(out, errOut io.Writer) *cobra.Command {
		var jsonOut bool
		root := &cobra.Command{Use: "papio", SilenceUsage: true, SilenceErrors: true}
		root.SetOut(out)
		root.SetErr(errOut)
		root.PersistentFlags().BoolVar(&jsonOut, "json", false, "emit JSON")

		var limit int
		visible := &cobra.Command{
			Use:         "visible",
			Short:       "Visible read-only command",
			Annotations: map[string]string{"mcp:read-only": "true"},
			RunE: func(_ *cobra.Command, args []string) error {
				fmt.Fprintf(out, "json=%v limit=%d args=%v", jsonOut, limit, args)
				return nil
			},
		}
		visible.Flags().IntVar(&limit, "limit", 0, "maximum results")

		fail := &cobra.Command{
			Use:   "fail",
			Short: "Failing command",
			RunE: func(_ *cobra.Command, _ []string) error {
				fmt.Fprint(out, "partial output")
				return errors.New("boom")
			},
		}

		hidden := &cobra.Command{
			Use:         "hidden",
			Short:       "Hidden command",
			Annotations: map[string]string{"mcp:hidden": "true"},
		}
		hidden.AddCommand(&cobra.Command{
			Use:   "secret",
			Short: "Secret subcommand",
			RunE:  func(_ *cobra.Command, _ []string) error { return nil },
		})

		parent := &cobra.Command{Use: "parent", Short: "Parent group"}
		child := &cobra.Command{
			Use:   "child <name>",
			Short: "Child leaf",
			RunE: func(_ *cobra.Command, args []string) error {
				fmt.Fprintf(out, "child args=%v", args)
				return nil
			},
		}
		parent.AddCommand(child)

		root.AddCommand(visible, fail, hidden, parent)
		return root
	}
}

func resultText(r *mcplib.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(mcplib.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestListCommandsExcludesHiddenSubtree(t *testing.T) {
	got := listCommands(testFactory())
	names := map[string]bool{}
	readOnly := map[string]bool{}
	for _, c := range got {
		names[c.Name] = true
		readOnly[c.Name] = c.ReadOnly
	}
	for _, want := range []string{"visible", "fail", "parent child"} {
		if !names[want] {
			t.Errorf("listCommands missing %q; got %v", want, names)
		}
	}
	for _, unwant := range []string{"hidden", "hidden secret", "parent"} {
		if names[unwant] {
			t.Errorf("listCommands should not include %q", unwant)
		}
	}
	if !readOnly["visible"] {
		t.Errorf("visible should be read-only")
	}
	if readOnly["fail"] {
		t.Errorf("fail should not be read-only")
	}
}

func TestFindCommand(t *testing.T) {
	factory := testFactory()
	if _, path, ok := findCommand(factory, "parent child"); !ok || !reflect.DeepEqual(path, []string{"parent", "child"}) {
		t.Fatalf("findCommand(parent child) = %v ok=%v", path, ok)
	}
	if _, _, ok := findCommand(factory, "hidden secret"); ok {
		t.Errorf("findCommand should not resolve hidden subtree")
	}
	if _, _, ok := findCommand(factory, "parent"); ok {
		t.Errorf("findCommand should not resolve a non-runnable parent")
	}
	if _, _, ok := findCommand(factory, "nope"); ok {
		t.Errorf("findCommand should not resolve unknown command")
	}
}

func TestRunInProcessInjectsJSONAndFlags(t *testing.T) {
	res := runInProcess(context.Background(), testFactory(), []string{"visible"}, map[string]any{"limit": float64(5)}, "")
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if got := resultText(res); !strings.Contains(got, "json=true limit=5 args=[]") {
		t.Fatalf("output = %q", got)
	}
}

func TestRunInProcessPositionalArgs(t *testing.T) {
	res := runInProcess(context.Background(), testFactory(), []string{"parent", "child"}, map[string]any{}, "alpha")
	if res.IsError {
		t.Fatalf("unexpected error: %s", resultText(res))
	}
	if got := resultText(res); !strings.Contains(got, "child args=[alpha]") {
		t.Fatalf("output = %q", got)
	}
}

func TestRunInProcessErrorIncludesOutput(t *testing.T) {
	res := runInProcess(context.Background(), testFactory(), []string{"fail"}, map[string]any{}, "")
	if !res.IsError {
		t.Fatalf("expected error result")
	}
	got := resultText(res)
	if !strings.Contains(got, "partial output") || !strings.Contains(got, "boom") {
		t.Fatalf("error output = %q", got)
	}
}

func TestValidateFlags(t *testing.T) {
	allowed := map[string]struct{}{"limit": {}}
	if err := validateFlags(map[string]any{"limit": 1}, allowed); err != nil {
		t.Errorf("allowed flag rejected: %v", err)
	}
	if err := validateFlags(map[string]any{"bogus": 1}, allowed); err == nil {
		t.Errorf("unknown flag accepted")
	}
	if err := validateFlags(map[string]any{"json": true}, allowed); err == nil {
		t.Errorf("inherited --json should not be exposed")
	}
}

func TestValidateArgs(t *testing.T) {
	if err := validateArgs("alpha beta"); err != nil {
		t.Errorf("positional args rejected: %v", err)
	}
	if err := validateArgs("alpha --secret"); err == nil {
		t.Errorf("raw flag token accepted in args")
	}
}

func TestCliArgsFromMCP(t *testing.T) {
	got := cliArgsFromMCP(map[string]any{
		"a": true,
		"b": false,
		"c": "x",
		"d": float64(2),
		"e": []any{"p", "q"},
		"f": "",
	})
	want := []string{"--a", "--b=false", "--c", "x", "--d", "2", "--e", "p", "--e", "q"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cliArgsFromMCP = %v, want %v", got, want)
	}
}

func TestSplitShellArgs(t *testing.T) {
	got := splitShellArgs(`a "b c" 'd e' f\ g`)
	want := []string{"a", "b c", "d e", "f g"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitShellArgs = %v, want %v", got, want)
	}
}

func TestSafeFlagNamesExcludesInherited(t *testing.T) {
	cmd, _, ok := findCommand(testFactory(), "visible")
	if !ok {
		t.Fatal("visible not found")
	}
	names := safeFlagNames(cmd)
	if _, ok := names["limit"]; !ok {
		t.Errorf("safeFlagNames missing local flag limit: %v", names)
	}
	if _, ok := names["json"]; ok {
		t.Errorf("safeFlagNames leaked inherited flag json")
	}
}
