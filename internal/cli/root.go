// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package cli defines papio's human and agent command surface.
package cli

import (
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
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(opt.out)
	root.SetErr(opt.errOut)
	root.PersistentFlags().StringVar(&opt.configPath, "config", "", "config TOML path")
	root.PersistentFlags().BoolVar(&opt.jsonOutput, "json", false, "emit structured JSON")
	root.AddCommand(
		newInitCommand(opt),
		newConfigCommand(opt),
		newAcquireCommand(opt),
		newBatchCommand(opt),
		newSearchCommand(opt),
		newWatchCommand(opt),
		newJobsCommand(opt),
		newStatusCommand(opt),
		newActionsCommand(opt),
		newArtifactsCommand(opt),
		newBundleCommand(opt),
		newDoctorCommand(opt),
		newZotioCommand(opt),
		newDaemonCommand(opt),
		newNativeHostCommand(opt),
		newMCPCommand(opt),
		newVersionCommand(opt),
	)
	return root
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
	client := ipc.NewUnixClient(socket)
	return client.Call(ctx, job.NewID("rpc"), method, params, result)
}

type daemonPingResult struct {
	Status               string `json:"status"`
	Version              string `json:"version"`
	ExtensionConnected   bool   `json:"extension_connected"`
	ExtensionVersion     string `json:"extension_version,omitempty"`
	UpdateAvailable      bool   `json:"update_available"`
	LatestVersion        string `json:"latest_version,omitempty"`
	ZotioUpdateAvailable bool   `json:"zotio_update_available"`
	ZotioLatestVersion   string `json:"zotio_latest_version,omitempty"`
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

func (o *options) printJSON(value any) error {
	encoder := json.NewEncoder(o.out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
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
	initCommand.Flags().StringVar(&mode, "access-mode", "", "required: conservative, assisted, or maximal")
	initCommand.Flags().StringVar(&email, "email", "", "contact email for polite APIs")
	initCommand.Flags().StringVar(&dataDir, "data-dir", "", "artifact and database directory")
	initCommand.Flags().BoolVar(&force, "force", false, "replace an existing config")
	_ = initCommand.MarkFlagRequired("access-mode")
	command.AddCommand(initCommand)
	return command
}
