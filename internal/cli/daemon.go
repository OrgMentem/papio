// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/bootstrap"
	"papio/internal/daemon"
	"papio/internal/ipc"
	"papio/internal/store"
)

func newDaemonCommand(opt *options) *cobra.Command {
	var socket string
	command := &cobra.Command{
		Use:         "daemon",
		Short:       "Run or control the local acquisition daemon",
		Annotations: map[string]string{"mcp:hidden": "true"},
		Args:        cobra.NoArgs,
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
			// The data directory is claimed before the store is opened and held
			// until the process exits, so exactly one daemon migrates and writes
			// the database, whatever socket it serves, and none starts beside one
			// that is still stopping.
			instance, err := daemon.AcquireInstance(cfg.DataDir, socket)
			if errors.Is(err, daemon.ErrInstanceHeld) {
				return daemonInstanceHeld(cfg.DataDir)
			}
			if err != nil {
				return err
			}
			// Deferred first, so it runs last: the claim outlives the store.
			defer instance.Release()
			// Bootstrap reads credentials from the OS keychain before it opens
			// the store; that read can wait on an access prompt, so say so.
			instance.SetPhase(daemon.PhaseCredentials)
			system, err := bootstrap.NewWithVersion(store.WithOpenTrace(cmd.Context(), instance.StoreTrace()), cfg, api.Version)
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
			// Shutdown starts when runCtx ends; from then until the process
			// exits, a command waits for this daemon instead of starting one.
			stopping := context.AfterFunc(runCtx, func() { instance.SetPhase(daemon.PhaseStopping) })
			defer stopping()
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
			instance.SetPhase(daemon.PhaseRunning)
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
			stopSocket := socket
			if stopSocket == "" {
				cfg, err := opt.loadConfig()
				if err != nil {
					return err
				}
				stopSocket = filepath.Join(cfg.DataDir, "papio.sock")
			}
			var result map[string]bool
			shutdown := func(ctx context.Context) error { return callExisting(ctx, "daemon.shutdown", &result) }
			// Returning only once the process has exited is what keeps the next
			// command from starting a new daemon beside the old one, which still
			// writes the database after its socket is gone.
			if err := daemon.StopDaemon(cmd.Context(), stopSocket, shutdown, func(pid int) {
				if opt.errOut != nil {
					_, _ = fmt.Fprintf(opt.errOut, "papio: waiting for the daemon (pid %d) to finish stopping\n", pid)
				}
			}); err != nil {
				down, unreachable := daemonDownState(opt, socket, err)
				switch {
				case !unreachable:
					return err
				case down.held:
					return fmt.Errorf("%w; run 'papio daemon stop' again when it finishes", down)
				}
				// Nothing to stop. A deploy script that stops the daemon before
				// it replaces the binary must not fail on a machine where the
				// daemon is already down.
				return opt.printResult(map[string]bool{"stopping": false}, "Daemon not running")
			}
			return opt.printResult(result, "Daemon stopped")
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Check the running daemon without autostarting one",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result daemonPingResult
			if err := callExisting(cmd.Context(), "ping", &result); err != nil {
				if down, unreachable := daemonDownState(opt, socket, err); unreachable {
					return down
				}
				return err
			}
			return opt.printResult(result, "Daemon %s (%s)", result.Status, result.Version)
		},
	}
	command.AddCommand(stop, status)
	return command
}

// daemonDown says why a daemon control command could not reach the daemon's
// socket: no daemon runs, or one holds the data directory and has not opened
// its socket yet, for example while it migrates the database.
type daemonDown struct {
	socket string
	status daemon.Status
	held   bool
}

// Is makes errors.Is(err, ipc.ErrDaemonUnreachable) true: --json reports
// daemon_unreachable for a daemon control command that found no daemon.
func (d daemonDown) Is(target error) bool { return target == ipc.ErrDaemonUnreachable }

func (d daemonDown) Error() string {
	if !d.held {
		return fmt.Sprintf("daemon is not running (nothing is listening on %s)", d.socket)
	}
	who := ""
	if d.status.PID > 0 {
		who = fmt.Sprintf(" (pid %d)", d.status.PID)
	}
	return fmt.Sprintf("daemon is %s%s; it is not accepting commands yet", d.status.Activity(), who)
}

// daemonDownState classifies err from a call to the daemon's socket. It
// reports false for any failure but a failed dial, which still says the daemon
// answered or that the caller gave up. socketFlag is the --socket override, or
// empty for the configured data directory's socket.
func daemonDownState(opt *options, socketFlag string, err error) (daemonDown, bool) {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return daemonDown{}, false
	}
	// Only a missing socket or a refused connection says no daemon listens.
	// EACCES and the like mean a daemon may run that this process cannot reach.
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
		return daemonDown{}, false
	}
	cfg, loadErr := opt.loadConfig()
	if loadErr != nil {
		return daemonDown{}, false
	}
	down := daemonDown{socket: socketFlag}
	if down.socket == "" {
		down.socket = filepath.Join(cfg.DataDir, "papio.sock")
	}
	down.status, down.held = daemon.ReadInstance(cfg.DataDir)
	// An instance that serves another socket says nothing about this one.
	if down.held && down.status.Socket != "" && down.status.Socket != down.socket {
		down.held = false
	}
	return down, true
}

// daemonInstanceHeld explains a daemon process that exits because another
// daemon process owns its data directory. Autostart waits for that process
// instead of launching a second one, so only a race or a manual start gets
// here, and the database is untouched: this process never opened it.
func daemonInstanceHeld(dataDir string) error {
	status, _ := daemon.ReadInstance(dataDir)
	if status.PID == 0 {
		return fmt.Errorf("daemon already starting for %s", dataDir)
	}
	return fmt.Errorf("daemon already %s for %s (pid %d, socket %s)", status.Activity(), dataDir, status.PID, status.Socket)
}
