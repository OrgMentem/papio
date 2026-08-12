// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package cli defines papio's human and agent command surface.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/agentjson"
	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/update"
)

type options struct {
	configPath string
	jsonOutput bool
	out        io.Writer
	errOut     io.Writer

	daemonVersionChecked bool
	updateHintShown      bool

	configLoader   func(string) (config.Config, error)
	newAutostarter func(string) *daemon.Autostarter
	rpcCall        func(context.Context, string, string, any, any) error
}

// NewRoot builds a command tree with no process-global output state.
func NewRoot(out, errOut io.Writer) *cobra.Command {
	return newRoot(&options{out: out, errOut: errOut})
}

// NewInProcessRoot builds a papio command tree whose RPC calls route through
// call instead of the daemon socket, for embedding the CLI command surface in
// the in-process MCP server. It performs no autostart and no daemon version
// handshake: the embedding process already owns the configured services.
func NewInProcessRoot(out, errOut io.Writer, cfg config.Config, call func(context.Context, string, any, any) error) *cobra.Command {
	opt := &options{
		out:                  out,
		errOut:               errOut,
		daemonVersionChecked: true,
		configLoader:         func(string) (config.Config, error) { return cfg, nil },
		newAutostarter: func(socket string) *daemon.Autostarter {
			return &daemon.Autostarter{SocketPath: socket, Ready: func(context.Context, string) error { return nil }}
		},
		rpcCall: func(ctx context.Context, _ string, method string, params, result any) error {
			return call(ctx, method, params, result)
		},
	}
	return newRoot(opt)
}

func newRoot(opt *options) *cobra.Command {
	root := &cobra.Command{
		Use:           "papio",
		Short:         "Legitimate paper-acquisition broker",
		Version:       api.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("papio {{.Version}}\n")
	root.SetOut(opt.out)
	root.SetErr(opt.errOut)
	root.PersistentFlags().StringVar(&opt.configPath, "config", "", "config TOML path")
	root.PersistentFlags().BoolVar(&opt.jsonOutput, "json", false, "emit structured JSON")
	root.AddCommand(
		newInitCommand(opt),
		newConfigCommand(opt),
		newAcquireCommand(opt),
		newGrabsCommand(opt),
		newBatchCommand(opt),
		newSearchCommand(opt),
		newWatchCommand(opt),
		newNotifyCommand(opt),
		newJobsCommand(opt),
		newDeliveryCommand(opt),
		newActivityCommand(opt),
		newFailuresCommand(opt),
		newExportCommand(opt),
		newAdapterCommand(opt),
		newPulseCommand(opt),
		newStatusCommand(opt),
		newBenchCommand(opt),
		newStatsCommand(opt),
		newActionsCommand(opt),
		newBrowserCommand(opt),
		newInboxCommand(opt),
		newArtifactsCommand(opt),
		newBundleCommand(opt),
		newDoctorCommand(opt),
		newZotioCommand(opt),
		newDaemonCommand(opt),
		newNativeHostCommand(opt),
		newMCPCommand(opt),
		newVersionCommand(opt),
	)
	for _, command := range root.Commands() {
		configureCommandGroups(command)
	}
	return root
}

// configureCommandGroups makes every non-runnable command group reject an
// unrecognized verb consistently. Cobra otherwise resolves an unknown child as
// a positional argument to its parent; groups with no RunE then succeed
// silently. Runnable parents (for example `watch digest <id>`, which also
// owns `watch digest clear`) keep their own Args contract untouched.
func configureCommandGroups(command *cobra.Command) {
	children := command.Commands()
	if len(children) == 0 {
		return
	}

	if !command.Runnable() {
		command.Args = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			verbs := make([]string, 0, len(children))
			for _, child := range children {
				if !child.Hidden {
					verbs = append(verbs, child.Name())
				}
			}
			return fmt.Errorf("unknown %s command %q; valid verbs: %s", cmd.Name(), args[0], strings.Join(verbs, ", "))
		}
		// Cobra returns help before validating Args for a non-runnable command.
		// A help-printing RunE keeps the bare invocation informative while the
		// Args validator above reports unknown verbs. The group stays help-only:
		// mcp:help-only excludes this node from the MCP command mirror without
		// hiding its children (mcp:hidden would prune the whole subtree).
		command.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
		if command.Annotations == nil {
			command.Annotations = map[string]string{}
		}
		command.Annotations["mcp:help-only"] = "true"
	}
	for _, child := range children {
		configureCommandGroups(child)
	}
}

func (o *options) loadConfig() (config.Config, error) {
	if o.configLoader != nil {
		return o.configLoader(o.configPath)
	}
	return config.Load(o.configPath)
}

func (o *options) call(ctx context.Context, method string, params, result any) error {
	cfg, err := o.loadConfig()
	if err != nil {
		return err
	}
	socket := filepath.Join(cfg.DataDir, "papio.sock")
	starter := o.autostarter(socket)
	starter.Args = []string{"--config", cfg.Path, "daemon", "--socket", socket}
	ensureResult, err := starter.EnsureWithResult(ctx)
	if err != nil {
		return err
	}
	if ensureResult.Started {
		o.daemonVersionChecked = true
	} else if err := o.warnDaemonVersion(ctx, socket, cfg); err != nil {
		return err
	}
	return o.socketCall(ctx, socket, method, params, result)
}

func (o *options) callExisting(ctx context.Context, method string, params, result any) error {
	cfg, err := o.loadConfig()
	if err != nil {
		return err
	}
	socket := filepath.Join(cfg.DataDir, "papio.sock")
	if err := o.warnDaemonVersion(ctx, socket, cfg); err != nil {
		return err
	}
	return o.socketCall(ctx, socket, method, params, result)
}

func (o *options) autostarter(socket string) *daemon.Autostarter {
	if o.newAutostarter != nil {
		return o.newAutostarter(socket)
	}
	return daemon.NewAutostarter(socket)
}

func (o *options) socketCall(ctx context.Context, socket, method string, params, result any) error {
	if o.rpcCall != nil {
		return o.rpcCall(ctx, socket, method, params, result)
	}
	return callSocket(ctx, socket, method, params, result)
}

func callSocket(ctx context.Context, socket, method string, params, result any) error {
	client := ipc.NewSocketClient(socket)
	return client.Call(ctx, job.NewID("rpc"), method, params, result)
}

type daemonPingResult struct {
	Status                 string `json:"status"`
	Version                string `json:"version"`
	ExtensionConnected     bool   `json:"extension_connected"`
	ExtensionVersion       string `json:"extension_version,omitempty"`
	PendingBrowserSessions int    `json:"pending_browser_sessions,omitempty"`
	BrowserSessionDenied   int    `json:"browser_session_denied,omitempty"`
	UpdateAvailable        bool   `json:"update_available"`
	LatestVersion          string `json:"latest_version,omitempty"`
	ZotioUpdateAvailable   bool   `json:"zotio_update_available"`
	ZotioLatestVersion     string `json:"zotio_latest_version,omitempty"`
}

func (o *options) warnDaemonVersion(ctx context.Context, socket string, cfg config.Config) error {
	if o.daemonVersionChecked {
		return nil
	}
	var status daemonPingResult
	if err := o.socketCall(ctx, socket, "ping", struct{}{}, &status); err != nil {
		return err
	}
	o.daemonVersionChecked = true
	if err := o.warnAvailableUpdate(cfg, status); err != nil {
		return err
	}
	if status.Version == "" || api.Version == "" || status.Version == api.Version {
		return nil
	}
	if o.errOut == nil {
		return nil
	}
	_, err := fmt.Fprintf(o.errOut, "papio: daemon is running %s but this CLI is %s — run 'papio daemon stop'; the next command starts the matching daemon\n", status.Version, api.Version)
	return err
}

func (o *options) warnAvailableUpdate(cfg config.Config, status daemonPingResult) error {
	if o.updateHintShown || !cfg.Updates.Check || o.errOut == nil {
		return nil
	}
	updates := make([]string, 0, 2)
	if status.UpdateAvailable && status.LatestVersion != "" {
		updates = append(updates, fmt.Sprintf("papio %s (you have %s)", status.LatestVersion, api.Version))
	}
	zotio := update.NewZotio(cfg.DataDir)
	if info, installed := zotio.CachedState(); info != nil {
		if installed != "" && update.IsNewer(info.LatestVersion, installed) {
			updates = append(updates, fmt.Sprintf("zotio %s (you have %s)", info.LatestVersion, installed))
		}
	}
	if len(updates) == 0 || !update.New(cfg.DataDir).TryMarkNagged(time.Now()) {
		return nil
	}
	o.updateHintShown = true
	_, err := fmt.Fprintf(o.errOut, "papio: updates available: %s — run 'papio doctor' for details\n", strings.Join(updates, ", "))
	return err
}

// printJSON writes one machine-readable payload.
//
// It escapes DEL (0x7f) and the C1 block (U+0080-U+009F) as \uXXXX rather
// than emitting them raw. encoding/json escapes only bytes below 0x20 plus
// quote and backslash, so an attacker-influenced string that reaches --json —
// a browser-reported download filename, a third-party bibliographic title —
// could otherwise carry U+009B or U+009D, which a UTF-8 terminal decodes as
// the CSI and OSC introducers, into the terminal of anyone who eyeballs or
// pipes this output (papio-9007c692bea6c968).
//
// This is deliberately NOT store.StripTerminalControls, which the
// human-readable rows use: --json is the authoritative machine-readable form
// and must not lose a byte of a filename a consumer needs to find on disk.
// A \uXXXX escape is lossless — every conformant JSON parser decodes \u009b
// back to U+009B — so the value survives exactly while the bytes on the wire
// stay terminal-safe.
//
// Encoding buffers rather than streams so the escape can scan the result.
// Every payload here is already bounded by the agentjson caps.
func (o *options) printJSON(value any) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	_, err := o.out.Write(escapeJSONTerminalControls(buf.Bytes()))
	return err
}

const hexDigits = "0123456789abcdef"

// escapeJSONTerminalControls rewrites DEL and the C1 block in already-encoded
// JSON to their \uXXXX escapes, returning the input unchanged (and
// unallocated) when neither appears.
//
// Scanning encoded bytes rather than the values before encoding is safe
// because JSON's structural syntax is entirely printable ASCII: DEL and the
// two-byte UTF-8 C1 sequences can only occur inside a string literal, which
// is the one place a \uXXXX escape means the same thing.
func escapeJSONTerminalControls(encoded []byte) []byte {
	if !hasTerminalControlBytes(encoded) {
		return encoded
	}
	out := make([]byte, 0, len(encoded)+16)
	// Classic form on purpose: the C1 branch advances i past the trailing
	// continuation byte, which a range loop would reassign back.
	for i := 0; i < len(encoded); i++ {
		b := encoded[i]
		if b == 0x7f {
			out = append(out, `\u007f`...)
			continue
		}
		if isC1Lead(encoded, i) {
			// UTF-8 two-byte form: 0xc2 contributes the leading 0x80.
			cp := 0x80 | (encoded[i+1] & 0x3f)
			out = append(out, `\u00`...)
			out = append(out, hexDigits[cp>>4], hexDigits[cp&0x0f])
			i++
			continue
		}
		out = append(out, b)
	}
	return out
}

func hasTerminalControlBytes(encoded []byte) bool {
	for i, b := range encoded {
		if b == 0x7f || isC1Lead(encoded, i) {
			return true
		}
	}
	return false
}

// isC1Lead reports whether encoded[i:] begins the UTF-8 encoding of a C1
// control (U+0080-U+009F).
func isC1Lead(encoded []byte, i int) bool {
	return encoded[i] == 0xc2 && i+1 < len(encoded) && encoded[i+1] >= 0x80 && encoded[i+1] <= 0x9f
}

// printPage emits rows through the shared agentjson envelope so every list
// command produces the same two-key shape: {"<key>": [...], "truncated": bool}.
// A standalone generic function rather than a method: Go does not allow type
// parameters on methods, and agentjson.Envelope's row type must stay generic
// so a non-slice value can never reach it by mistake.
func printPage[T any](o *options, key string, rows []T, truncated bool) error {
	return o.printJSON(agentjson.Envelope(key, rows, truncated))
}

// effectiveLimit reproduces a daemon store's own clamp client-side
// (job.EffectiveListLimit, watch.Store.Digest), so a CLI command can pass the
// very value the daemon will actually use to both the RPC call and
// agentjson.Capped. Comparing a returned row count against the raw --limit
// flag instead is how `truncated` lies whenever --limit is out of range.
//
// It must stay in step with the stores: this is a fourth copy of that clamp,
// and when the stores stopped resetting an over-large limit to the default,
// leaving this one behind kept `--limit 600` returning 100 rows even though
// the daemon would have served 500. The unit test passed; only running the
// command showed it.
func effectiveLimit(requested, max, def int) int {
	if requested <= 0 {
		return def
	}
	if requested > max {
		return max
	}
	return requested
}

// effectiveLimitFloored is effectiveLimit for the daemon stores that floor a
// negative limit at 1 instead of resetting it to def (job.Store.Failures,
// discovery.normalizeParams): only an exact zero resets to def.
func effectiveLimitFloored(requested, max, def int) int {
	switch {
	case requested == 0:
		return def
	case requested < 0:
		return 1
	case requested > max:
		return max
	default:
		return requested
	}
}

func (o *options) printResult(value any, prose string, args ...any) error {
	if o.jsonOutput {
		return o.printJSON(value)
	}
	_, err := fmt.Fprintf(o.out, prose+"\n", args...)
	return err
}

func newVersionCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:         "version",
		Short:       "Print version information",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return opt.printResult(map[string]string{"version": api.Version}, "papio %s", api.Version)
		},
	}
}

func newConfigCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "config", Short: "Manage papio configuration", Annotations: map[string]string{"mcp:hidden": "true"}}
	var mode, email, dataDir string
	var force bool
	initCommand := &cobra.Command{
		Use:   "init",
		Short: "Write explicit first-run configuration",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			path := opt.configPath
			if path == "" {
				path = filepath.Join(config.Dir(), "config.toml")
			}
			if !force {
				if _, err := os.Lstat(path); err == nil {
					return fmt.Errorf("config already exists at %s (use --force to replace it)", path)
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			cfg := config.Default()
			cfg.AccessMode = mode
			cfg.Email = strings.TrimSpace(email)
			if dataDir != "" {
				cfg.DataDir = dataDir
			}
			if err := config.Save(cfg, path); err != nil {
				return err
			}
			return opt.printResult(map[string]string{"config_path": path, "access_mode": mode}, "Wrote %s (access_mode=%s)", path, mode)
		},
	}
	initCommand.Flags().StringVar(&mode, "access-mode", "", "required: conservative, assisted, or delegated")
	initCommand.Flags().StringVar(&email, "email", "", "contact email for polite APIs")
	initCommand.Flags().StringVar(&dataDir, "data-dir", "", "artifact and database directory")
	initCommand.Flags().BoolVar(&force, "force", false, "replace an existing config")
	_ = initCommand.MarkFlagRequired("access-mode")
	command.AddCommand(initCommand)
	return command
}
