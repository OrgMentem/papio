// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/ipc"
)

// TestDaemonLeavesTheStoreToTheDaemonThatOwnsItsSocket pins the single
// migrator. A daemon process that finds another daemon process holding its
// socket, here one that is upgrading the database, exits with a plain message
// and never opens the store, so two daemons never migrate one database.
func TestDaemonLeavesTheStoreToTheDaemonThatOwnsItsSocket(t *testing.T) {
	dataDir := t.TempDir()
	socket := filepath.Join(dataDir, "papio.sock")
	owner, err := daemon.AcquireInstance(dataDir, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	owner.StoreTrace().Migrating(55, 56)

	var out, errOut bytes.Buffer
	root := newRoot(&options{
		out:          &out,
		errOut:       &errOut,
		configLoader: func(string) (config.Config, error) { return config.Config{DataDir: dataDir}, nil },
	})
	root.SetArgs([]string{"daemon", "--socket", socket})
	err = root.ExecuteContext(context.Background())
	want := fmt.Sprintf("daemon already upgrading the database for %s (pid %d, socket %s)", dataDir, os.Getpid(), socket)
	if err == nil || err.Error() != want {
		t.Fatalf("second daemon error = %v, want %q", err, want)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "papio.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the second daemon opened the store: stat papio.db = %v", err)
	}
}

// TestDaemonOnAnotherSocketLeavesTheStoreToItsOwner pins that the claim
// belongs to the database, not to the socket: a daemon started with its own
// --socket for a data directory another daemon already owns must not open
// that database, or two daemons migrate and write one store at once.
func TestDaemonOnAnotherSocketLeavesTheStoreToItsOwner(t *testing.T) {
	dataDir := t.TempDir()
	owner, err := daemon.AcquireInstance(dataDir, filepath.Join(dataDir, "papio.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	owner.StoreTrace().Migrating(55, 56)

	other := filepath.Join(t.TempDir(), "other.sock")
	var out, errOut bytes.Buffer
	root := newRoot(&options{
		out:          &out,
		errOut:       &errOut,
		configLoader: func(string) (config.Config, error) { return config.Config{DataDir: dataDir}, nil },
	})
	root.SetArgs([]string{"daemon", "--socket", other})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = root.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("daemon already upgrading the database for %s (pid %d", dataDir, os.Getpid())) {
		t.Fatalf("daemon on another socket: error = %v, want it refused because pid %d owns %s", err, os.Getpid(), dataDir)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "papio.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the daemon on another socket opened the store: stat papio.db = %v", err)
	}
}

// shortSocketDir returns a directory whose socket path fits sun_path, which
// t.TempDir does not for a descriptively named test on macOS.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "papio-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func daemonControlRoot(dataDir string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	root := newRoot(&options{
		out:          &out,
		errOut:       &errOut,
		configLoader: func(string) (config.Config, error) { return config.Config{DataDir: dataDir}, nil },
	})
	return root, &out, &errOut
}

// TestDaemonStatusNamesWhyTheSocketIsUnreachable pins what an operator reads
// while no daemon runs and while one holds the data directory but has not
// opened its socket: a raw dial error looks the same in both cases.
func TestDaemonStatusNamesWhyTheSocketIsUnreachable(t *testing.T) {
	t.Run("no daemon", func(t *testing.T) {
		dataDir := shortSocketDir(t)
		root, _, _ := daemonControlRoot(dataDir)
		root.SetArgs([]string{"daemon", "status"})
		err := root.ExecuteContext(context.Background())
		want := fmt.Sprintf("daemon is not running (nothing is listening on %s)", filepath.Join(dataDir, "papio.sock"))
		if err == nil || err.Error() != want {
			t.Fatalf("daemon status error = %v, want %q", err, want)
		}
	})
	t.Run("daemon upgrading the database", func(t *testing.T) {
		dataDir := shortSocketDir(t)
		owner, err := daemon.AcquireInstance(dataDir, filepath.Join(dataDir, "papio.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Release()
		owner.StoreTrace().Migrating(55, 56)
		root, _, _ := daemonControlRoot(dataDir)
		root.SetArgs([]string{"daemon", "status"})
		err = root.ExecuteContext(context.Background())
		want := fmt.Sprintf("daemon is upgrading the database (pid %d); it is not accepting commands yet", os.Getpid())
		if err == nil || err.Error() != want {
			t.Fatalf("daemon status error = %v, want %q", err, want)
		}
	})
}

// TestDaemonStopReportsAnAlreadyStoppedDaemonAsSuccess pins that a deploy
// script can run daemon stop on a machine where the daemon is already down,
// and that a daemon still upgrading its database is a failure that says so.
func TestDaemonStopReportsAnAlreadyStoppedDaemonAsSuccess(t *testing.T) {
	t.Run("no daemon", func(t *testing.T) {
		root, out, _ := daemonControlRoot(shortSocketDir(t))
		root.SetArgs([]string{"daemon", "stop"})
		if err := root.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("daemon stop with no daemon: %v", err)
		}
		if got := out.String(); got != "Daemon not running\n" {
			t.Fatalf("daemon stop output = %q, want %q", got, "Daemon not running\n")
		}
	})
	t.Run("daemon upgrading the database", func(t *testing.T) {
		dataDir := shortSocketDir(t)
		owner, err := daemon.AcquireInstance(dataDir, filepath.Join(dataDir, "papio.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Release()
		owner.StoreTrace().Migrating(55, 56)
		root, _, _ := daemonControlRoot(dataDir)
		root.SetArgs([]string{"daemon", "stop"})
		err = root.ExecuteContext(context.Background())
		want := fmt.Sprintf("daemon is upgrading the database (pid %d); it is not accepting commands yet; run 'papio daemon stop' again when it finishes", os.Getpid())
		if err == nil || err.Error() != want {
			t.Fatalf("daemon stop error = %v, want %q", err, want)
		}
	})
}

// TestDaemonControlSurfacesUnreachableSocketErrors pins that a dial error that
// is not "no socket" or "refused" (here EACCES) is an error, not an absent daemon.
func TestDaemonControlSurfacesUnreachableSocketErrors(t *testing.T) {
	dataDir := shortSocketDir(t)
	socket := filepath.Join(dataDir, "papio.sock")
	// A directory's POSIX mode cannot deny access to a Windows named pipe.
	// Inject each transport's dial-error shape at the existing RPC boundary.
	for _, dialErr := range []error{
		&net.OpError{Op: "dial", Net: "unix", Err: fs.ErrPermission},
		&os.PathError{Op: "open", Path: socket, Err: fs.ErrPermission},
	} {
		for _, sub := range []string{"stop", "status"} {
			var out, errOut bytes.Buffer
			want := ipc.Unreachable(fmt.Errorf("dial ipc daemon: %w", dialErr))
			root := newRoot(&options{
				out:          &out,
				errOut:       &errOut,
				configLoader: func(string) (config.Config, error) { return config.Config{DataDir: dataDir}, nil },
				rpcCall: func(context.Context, string, string, any, any) error {
					return want
				},
			})
			root.SetArgs([]string{"daemon", sub, "--socket", socket})
			err := root.ExecuteContext(context.Background())
			if !errors.Is(err, want) || !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("daemon %s error = %v, output = %q; want the original permission error %v", sub, err, out.String(), want)
			}
		}
	}
}

func TestDaemonStopSkipsBlockedVersionHandshake(t *testing.T) {
	dataDir := shortSocketDir(t)
	var out, errOut bytes.Buffer
	root := newRoot(&options{
		out:          &out,
		errOut:       &errOut,
		configLoader: func(string) (config.Config, error) { return config.Config{DataDir: dataDir}, nil },
		rpcCall: func(ctx context.Context, _, method string, _, result any) error {
			switch method {
			case "ping":
				<-ctx.Done()
				return ctx.Err()
			case "daemon.shutdown":
				*result.(*map[string]bool) = map[string]bool{"stopping": true}
				return nil
			default:
				return fmt.Errorf("unexpected method %q", method)
			}
		},
	})
	root.SetArgs([]string{"daemon", "stop"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("daemon stop waited for the blocked version ping: %v", err)
	}
	if out.String() != "Daemon stopped\n" {
		t.Fatalf("daemon stop output = %q, want a successful shutdown", out.String())
	}
}

func TestDaemonStatusTimesOutWithLogRemediation(t *testing.T) {
	dataDir := shortSocketDir(t)
	socket := filepath.Join(dataDir, "papio.sock")
	var out, errOut bytes.Buffer
	root := newRoot(&options{
		out:          &out,
		errOut:       &errOut,
		configLoader: func(string) (config.Config, error) { return config.Config{DataDir: dataDir}, nil },
		rpcCall: func(ctx context.Context, _, _ string, _, _ any) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
				return errors.New("the command did not bound its diagnostic call")
			}
		},
	})
	root.SetArgs([]string{"daemon", "status", "--socket", socket})
	err := root.ExecuteContext(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), filepath.Join(dataDir, "daemon.log")) ||
		!strings.Contains(err.Error(), "SIGQUIT") {
		t.Fatalf("stalled daemon status error = %v, want a bounded timeout with log and dump guidance", err)
	}
}
