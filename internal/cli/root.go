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

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/ipc"
	"papio/internal/job"
)

type options struct {
	configPath string
	jsonOutput bool
	out        io.Writer
	errOut     io.Writer
}

// NewRoot builds a command tree with no process-global output state.
func NewRoot(out, errOut io.Writer) *cobra.Command {
	opt := &options{out: out, errOut: errOut}
	root := &cobra.Command{
		Use:           "papio",
		Short:         "Legitimate paper-acquisition broker",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(out)
	root.SetErr(errOut)
	root.PersistentFlags().StringVar(&opt.configPath, "config", "", "config TOML path")
	root.PersistentFlags().BoolVar(&opt.jsonOutput, "json", false, "emit structured JSON")
	root.AddCommand(
		newConfigCommand(opt),
		newAcquireCommand(opt),
		newJobsCommand(opt),
		newActionsCommand(opt),
		newArtifactsCommand(opt),
		newBundleCommand(opt),
		newDoctorCommand(opt),
		newDaemonCommand(opt),
		newNativeHostCommand(opt),
		newVersionCommand(opt),
	)
	return root
}

func (o *options) loadConfig() (config.Config, error) {
	return config.Load(o.configPath)
}

func (o *options) call(ctx context.Context, method string, params, result any) error {
	cfg, err := o.loadConfig()
	if err != nil {
		return err
	}
	socket := filepath.Join(cfg.DataDir, "papio.sock")
	starter := daemon.NewAutostarter(socket)
	starter.Args = []string{"--config", cfg.Path, "daemon", "--socket", socket}
	if err := starter.Ensure(ctx); err != nil {
		return err
	}
	return callSocket(ctx, socket, method, params, result)
}

func (o *options) callExisting(ctx context.Context, method string, params, result any) error {
	cfg, err := o.loadConfig()
	if err != nil {
		return err
	}
	return callSocket(ctx, filepath.Join(cfg.DataDir, "papio.sock"), method, params, result)
}

func callSocket(ctx context.Context, socket, method string, params, result any) error {
	client := ipc.NewUnixClient(socket)
	return client.Call(ctx, job.NewID("rpc"), method, params, result)
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
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return opt.printResult(map[string]string{"version": api.Version}, "papio %s", api.Version)
		},
	}
}

func newConfigCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "config", Short: "Manage papio configuration"}
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
