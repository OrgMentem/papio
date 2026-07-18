// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package cobratree exposes papio's Cobra command tree through MCP so the CLI
// stays the single source of truth for the agent command surface. Instead of a
// hand-maintained typed tool per command (which drifts from the CLI), it
// registers either:
//
//   - the command-orchestration facade (default): papio_command_search +
//     papio_command_run, which collapses the whole tree behind two tools and
//     keeps standing token cost low while leaving every command reachable on
//     demand; or
//   - the per-command mirror (PAPIO_MCP_SURFACE=mirror): one lean MCP tool per
//     mirrorable command, giving hosts native per-command schemas.
//
// Both surfaces execute the mirrored command in-process against a fresh command
// tree whose RPC is routed to the embedding daemon (see cli.NewInProcessRoot),
// inject --json out-of-band for structured output, and share one arg-safety
// guard so validation and schema exposure never diverge.
//
// Commands opt out with the "mcp:hidden" annotation and declare read-only
// intent with "mcp:read-only"; unannotated commands are visible and treated as
// mutating.
package cobratree

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// RootFactory builds a fresh papio command tree that writes to out/errOut.
// cobra.Command state is single-use, so every tool invocation gets its own
// tree. papio commands render to the writers captured at construction (not
// cmd.OutOrStdout), so the factory must thread them through.
type RootFactory func(out, errOut io.Writer) *cobra.Command

// Register adds the CLI command surface to the MCP server. PAPIO_MCP_SURFACE
// selects the shape: default "facade" (papio_command_search + papio_command_run)
// or "mirror" (one tool per command).
func Register(s *server.MCPServer, factory RootFactory) {
	if factory == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("PAPIO_MCP_SURFACE")), "mirror") {
		registerMirror(s, factory)
		return
	}
	registerFacade(s, factory)
}

// --- Facade: papio_command_search + papio_command_run ---

func registerFacade(s *server.MCPServer, factory RootFactory) {
	s.AddTool(mcplib.NewTool("papio_command_search",
		mcplib.WithDescription("Search and inspect papio CLI commands reachable through the command facade. Omit arguments to list every command; pass name for one command's flags and args."),
		mcplib.WithString("query", mcplib.Description("Case-insensitive text matched against command names and summaries.")),
		mcplib.WithString("name", mcplib.Description("Exact space-separated command path to inspect, such as \"zotio apply\".")),
		mcplib.WithReadOnlyHintAnnotation(true),
		mcplib.WithDestructiveHintAnnotation(false),
	), commandSearchHandler(factory))

	s.AddTool(mcplib.NewTool("papio_command_run",
		mcplib.WithDescription("Run one papio CLI command by its space-separated command path. Output is structured JSON. Inspect available flags with papio_command_search."),
		mcplib.WithString("name", mcplib.Required(), mcplib.Description("Exact space-separated command path to run, such as \"zotio apply\".")),
		mcplib.WithObject("flags", mcplib.Description("Command-local flags passed by name, such as {\"confirm-sha256\": \"...\"} or {\"oa-only\": true}."), mcplib.AdditionalProperties(true)),
		mcplib.WithString("args", mcplib.Description("Additional positional arguments only; raw flags are rejected.")),
		mcplib.WithDestructiveHintAnnotation(true),
	), commandRunHandler(factory))
}

type commandSummary struct {
	Name     string `json:"name"`
	Summary  string `json:"summary"`
	ReadOnly bool   `json:"read_only"`
}

type commandDetail struct {
	Name      string        `json:"name"`
	Summary   string        `json:"summary"`
	ReadOnly  bool          `json:"read_only"`
	TakesArgs bool          `json:"takes_args"`
	Flags     []flagDetail  `json:"flags"`
}

type flagDetail struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Default     string `json:"default,omitempty"`
	Description string `json:"description"`
}

func commandSearchHandler(factory RootFactory) server.ToolHandlerFunc {
	return func(_ context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		args := req.GetArguments()
		if name, _ := args["name"].(string); strings.TrimSpace(name) != "" {
			cmd, _, ok := findCommand(factory, name)
			if !ok {
				return mcplib.NewToolResultError("command not found: " + name), nil
			}
			detail := commandDetail{
				Name:      name,
				Summary:   firstLine(descriptionFor(cmd)),
				ReadOnly:  isReadOnly(cmd),
				TakesArgs: commandTakesArgs(cmd),
				Flags:     []flagDetail{},
			}
			visitSafeFlags(cmd, func(flag *pflag.Flag) {
				detail.Flags = append(detail.Flags, flagDetail{
					Name:        flag.Name,
					Type:        flag.Value.Type(),
					Default:     flag.DefValue,
					Description: flag.Usage,
				})
			})
			return jsonResult(detail)
		}

		needle := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", args["query"])))
		if _, ok := args["query"]; !ok {
			needle = ""
		}
		commands := listCommands(factory)
		out := make([]commandSummary, 0, len(commands))
		for _, command := range commands {
			if needle != "" && !strings.Contains(strings.ToLower(command.Name+" "+command.Summary), needle) {
				continue
			}
			out = append(out, command)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return jsonResult(out)
	}
}

func commandRunHandler(factory RootFactory) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		args := req.GetArguments()
		name, _ := args["name"].(string)
		if strings.TrimSpace(name) == "" {
			return mcplib.NewToolResultError("papio_command_run requires name"), nil
		}
		cmd, path, ok := findCommand(factory, name)
		if !ok {
			return mcplib.NewToolResultError("command not found: " + name), nil
		}

		flags := map[string]any{}
		if raw, ok := args["flags"]; ok && raw != nil {
			obj, ok := raw.(map[string]any)
			if !ok {
				return mcplib.NewToolResultError("papio_command_run flags must be an object"), nil
			}
			flags = obj
		}
		argsStr, _ := args["args"].(string)

		if err := validateFlags(flags, safeFlagNames(cmd)); err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		if err := validateArgs(argsStr); err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		return runInProcess(ctx, factory, path, flags, argsStr), nil
	}
}

// --- Mirror: one tool per command ---

func registerMirror(s *server.MCPServer, factory RootFactory) {
	root := buildRoot(factory)
	if root == nil {
		return
	}
	walk(root, nil, func(cmd *cobra.Command, path []string) {
		if !isMirrorable(cmd) {
			return
		}
		toolName := "papio_" + strings.ReplaceAll(strings.Join(path, "_"), "-", "_")
		opts := []mcplib.ToolOption{
			mcplib.WithDescription(firstLine(descriptionFor(cmd))),
			mcplib.WithReadOnlyHintAnnotation(isReadOnly(cmd)),
			mcplib.WithDestructiveHintAnnotation(!isReadOnly(cmd)),
		}
		visitSafeFlags(cmd, func(flag *pflag.Flag) {
			opts = append(opts, toolOptionForFlag(flag))
		})
		if commandTakesArgs(cmd) {
			opts = append(opts, mcplib.WithString("args", mcplib.Description("Positional arguments only; raw flags are rejected.")))
		}
		allowed := safeFlagNames(cmd)
		commandPath := append([]string{}, path...)
		s.AddTool(mcplib.NewTool(toolName, opts...), mirrorHandler(factory, commandPath, allowed))
	})
}

func mirrorHandler(factory RootFactory, path []string, allowed map[string]struct{}) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		args := req.GetArguments()
		flags := map[string]any{}
		for key, value := range args {
			if key == "args" {
				continue
			}
			flags[key] = value
		}
		argsStr, _ := args["args"].(string)
		if err := validateFlags(flags, allowed); err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		if err := validateArgs(argsStr); err != nil {
			return mcplib.NewToolResultError(err.Error()), nil
		}
		return runInProcess(ctx, factory, path, flags, argsStr), nil
	}
}

func toolOptionForFlag(flag *pflag.Flag) mcplib.ToolOption {
	desc := mcplib.Description(flag.Usage)
	switch flag.Value.Type() {
	case "bool":
		return mcplib.WithBoolean(flag.Name, desc)
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64":
		return mcplib.WithNumber(flag.Name, desc)
	default:
		return mcplib.WithString(flag.Name, desc)
	}
}

// --- In-process execution ---

// runInProcess builds a fresh command tree, injects --json for structured
// output, appends validated flags and positional args, and executes the command
// with its output captured. Each call gets its own tree and the RPC router it
// routes to is concurrency-safe, so no cross-call locking is needed.
func runInProcess(ctx context.Context, factory RootFactory, path []string, flags map[string]any, argsStr string) *mcplib.CallToolResult {
	var buf bytes.Buffer
	root := factory(&buf, &buf)
	if root == nil {
		return mcplib.NewToolResultError("failed to build command tree")
	}
	finalArgs := append([]string{}, path...)
	finalArgs = append(finalArgs, "--json")
	finalArgs = append(finalArgs, cliArgsFromMCP(flags)...)
	if strings.TrimSpace(argsStr) != "" {
		finalArgs = append(finalArgs, splitShellArgs(argsStr)...)
	}
	root.SetArgs(finalArgs)
	if err := root.ExecuteContext(ctx); err != nil {
		if out := strings.TrimSpace(buf.String()); out != "" {
			return mcplib.NewToolResultError(out + "\n" + err.Error())
		}
		return mcplib.NewToolResultError(err.Error())
	}
	return mcplib.NewToolResultText(buf.String())
}

// cliArgsFromMCP converts an MCP flag object to Cobra CLI flag tokens with
// deterministic ordering.
func cliArgsFromMCP(flags map[string]any) []string {
	keys := make([]string, 0, len(flags))
	for key := range flags {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var out []string
	for _, key := range keys {
		switch value := flags[key].(type) {
		case bool:
			if value {
				out = append(out, "--"+key)
			} else {
				out = append(out, "--"+key+"=false")
			}
		case float64:
			out = append(out, "--"+key, strconv.FormatFloat(value, 'f', -1, 64))
		case string:
			if value != "" {
				out = append(out, "--"+key, value)
			}
		case []any:
			for _, item := range value {
				out = append(out, "--"+key, fmt.Sprintf("%v", item))
			}
		default:
			if value != nil {
				out = append(out, "--"+key, fmt.Sprintf("%v", value))
			}
		}
	}
	return out
}

// splitShellArgs whitespace-splits with double- and single-quoted token
// preservation and backslash escapes.
func splitShellArgs(s string) []string {
	var tokens []string
	var cur []rune
	inSingle := false
	inDouble := false
	escaped := false
	hasToken := false

	for _, r := range s {
		switch {
		case escaped:
			cur = append(cur, r)
			hasToken = true
			escaped = false
		case r == '\\' && !inSingle:
			escaped = true
			hasToken = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
			hasToken = true
		case r == '"' && !inSingle:
			inDouble = !inDouble
			hasToken = true
		case (r == ' ' || r == '\t') && !inSingle && !inDouble:
			if hasToken {
				tokens = append(tokens, string(cur))
				cur = cur[:0]
				hasToken = false
			}
		default:
			cur = append(cur, r)
			hasToken = true
		}
	}
	if escaped {
		cur = append(cur, '\\')
	}
	if hasToken {
		tokens = append(tokens, string(cur))
	}
	return tokens
}

// --- Validation ---

// validateFlags rejects flag keys the command does not expose (inherited
// globals like --config/--json are never in the allowlist and so are rejected).
func validateFlags(flags map[string]any, allowed map[string]struct{}) error {
	for name := range flags {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("command does not expose --%s", name)
		}
	}
	return nil
}

// validateArgs rejects raw flag tokens smuggled through the positional args
// string.
func validateArgs(argsStr string) error {
	for _, token := range splitShellArgs(argsStr) {
		if strings.HasPrefix(token, "-") {
			return fmt.Errorf("args accepts positional arguments only; raw flag %q is not allowed", token)
		}
	}
	return nil
}

// --- Walking & classification ---

func buildRoot(factory RootFactory) *cobra.Command {
	if factory == nil {
		return nil
	}
	return factory(io.Discard, io.Discard)
}

// walk visits every visible descendant command depth-first, skipping hidden
// subtrees entirely so a hidden parent (config, daemon, native-host) hides its
// children too.
func walk(cmd *cobra.Command, path []string, visit func(*cobra.Command, []string)) {
	for _, sub := range cmd.Commands() {
		if isHidden(sub) {
			continue
		}
		subPath := append(append([]string{}, path...), sub.Name())
		visit(sub, subPath)
		walk(sub, subPath, visit)
	}
}

func listCommands(factory RootFactory) []commandSummary {
	root := buildRoot(factory)
	if root == nil {
		return nil
	}
	var out []commandSummary
	walk(root, nil, func(cmd *cobra.Command, path []string) {
		if !isMirrorable(cmd) {
			return
		}
		out = append(out, commandSummary{
			Name:     strings.Join(path, " "),
			Summary:  firstLine(descriptionFor(cmd)),
			ReadOnly: isReadOnly(cmd),
		})
	})
	return out
}

func findCommand(factory RootFactory, name string) (*cobra.Command, []string, bool) {
	root := buildRoot(factory)
	if root == nil {
		return nil, nil, false
	}
	var found *cobra.Command
	var foundPath []string
	walk(root, nil, func(cmd *cobra.Command, path []string) {
		if found != nil || !isMirrorable(cmd) || strings.Join(path, " ") != name {
			return
		}
		found = cmd
		foundPath = append([]string{}, path...)
	})
	return found, foundPath, found != nil
}

func isHidden(cmd *cobra.Command) bool {
	return cmd.Hidden || cmd.Annotations["mcp:hidden"] == "true"
}

func isReadOnly(cmd *cobra.Command) bool {
	return cmd.Annotations["mcp:read-only"] == "true"
}

func isMirrorable(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Runnable() && !isHidden(cmd)
}

func commandTakesArgs(cmd *cobra.Command) bool {
	return cmd.Use != "" && strings.ContainsAny(cmd.Use, " ")
}

// visitSafeFlags emits command-local (non-inherited) flags only. Inherited
// persistent flags (--config, --json) are host/out-of-band concerns and never
// exposed as tool parameters.
func visitSafeFlags(cmd *cobra.Command, visit func(*pflag.Flag)) {
	cmd.NonInheritedFlags().VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden || flag.Deprecated != "" {
			return
		}
		visit(flag)
	})
}

func safeFlagNames(cmd *cobra.Command) map[string]struct{} {
	names := map[string]struct{}{}
	visitSafeFlags(cmd, func(flag *pflag.Flag) {
		names[flag.Name] = struct{}{}
	})
	return names
}

func descriptionFor(cmd *cobra.Command) string {
	if cmd.Long != "" {
		return cmd.Long
	}
	return cmd.Short
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

func jsonResult(value any) (*mcplib.CallToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	return mcplib.NewToolResultText(string(data)), nil
}
