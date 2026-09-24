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
	"testing"

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
	owner, err := daemon.AcquireInstance(socket)
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
	want := fmt.Sprintf("daemon already upgrading the database at %s (pid %d)", socket, os.Getpid())
	if err == nil || err.Error() != want {
		t.Fatalf("second daemon error = %v, want %q", err, want)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "papio.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the second daemon opened the store: stat papio.db = %v", err)
	}
}
