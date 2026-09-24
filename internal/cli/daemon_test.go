// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/daemon"
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
