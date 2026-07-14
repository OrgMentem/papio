// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/bootstrap"
	"papio/internal/ipc"
)

func newDaemonCommand(opt *options) *cobra.Command {
	var socket string
	command := &cobra.Command{
		Use:   "daemon",
		Short: "Run or control the local acquisition daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (runErr error) {
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			if socket == "" {
				socket = filepath.Join(cfg.DataDir, "papio.sock")
			}
			probeCtx, cancelProbe := context.WithTimeout(cmd.Context(), 25*time.Millisecond)
			probeErr := ipc.WaitForSocket(probeCtx, socket, 5*time.Millisecond)
			cancelProbe()
			if probeErr == nil {
				return fmt.Errorf("daemon already running at %s", socket)
			}
			system, err := bootstrap.New(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() {
				if closeErr := system.Close(); closeErr != nil && runErr == nil {
					runErr = closeErr
				}
			}()
			runCtx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			server := &ipc.Server{SocketPath: socket, Handler: api.RouterWithShutdown(system, cancel)}
			serverDone := make(chan error, 1)
			go func() { serverDone <- server.Serve(runCtx) }()
			readyCtx, cancelReady := context.WithTimeout(runCtx, 2*time.Second)
			readyErr := ipc.WaitForSocket(readyCtx, socket, 10*time.Millisecond)
			cancelReady()
			if readyErr != nil {
				cancel()
				if serverErr := <-serverDone; serverErr != nil {
					return serverErr
				}
				return readyErr
			}
			select {
			case serverErr := <-serverDone:
				cancel()
				return serverErr
			default:
			}
			schedulerDone := make(chan error, 1)
			go func() { schedulerDone <- system.Scheduler.Run(runCtx) }()
			sweeperDone := make(chan error, 1)
			go func() { sweeperDone <- system.Browser.RunSweeper(runCtx, 2*time.Second) }()
			var serverErr, schedulerErr error
			select {
			case serverErr = <-serverDone:
				cancel()
				schedulerErr = <-schedulerDone
				<-sweeperDone
			case schedulerErr = <-schedulerDone:
				cancel()
				serverErr = <-serverDone
				<-sweeperDone
			case <-cmd.Context().Done():
				cancel()
				serverErr = <-serverDone
				schedulerErr = <-schedulerDone
				<-sweeperDone
			}
			if cmd.Context().Err() != nil {
				if errors.Is(serverErr, context.Canceled) {
					serverErr = nil
				}
				if errors.Is(schedulerErr, context.Canceled) {
					schedulerErr = nil
				}
			}
			if serverErr != nil {
				return serverErr
			}
			return schedulerErr
		},
	}
	command.PersistentFlags().StringVar(&socket, "socket", "", "Unix socket path")
	callExisting := func(ctx context.Context, method string, result any) error {
		if socket == "" {
			return opt.callExisting(ctx, method, struct{}{}, result)
		}
		return callSocket(ctx, socket, method, struct{}{}, result)
	}
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon without autostarting one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result map[string]bool
			if err := callExisting(cmd.Context(), "daemon.shutdown", &result); err != nil {
				return err
			}
			return opt.printResult(result, "Daemon stopping")
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Check the running daemon without autostarting one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result map[string]string
			if err := callExisting(cmd.Context(), "ping", &result); err != nil {
				return err
			}
			return opt.printResult(result, "Daemon %s (%s)", result["status"], result["version"])
		},
	}
	command.AddCommand(stop, status)
	return command
}
