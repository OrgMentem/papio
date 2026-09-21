// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//go:build windows

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsConfigPublicationSharingDenialPreservesPriorFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := credentialConfig()
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Real sharing denial at rename: readers remain allowed, replacement does
	// not. This exercises the publication error rather than caller cancellation.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			windows.CloseHandle(handle)
		}
	}()
	cfg.Email = "replacement@example.test"
	if err := SaveIfUnchanged(cfg, path, snapshot); err == nil {
		t.Fatal("publication ignored a live non-delete-sharing handle")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed publication changed prior file")
	}
	assertNoConfigTemporaryFiles(t, filepath.Dir(path))
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	closed = true
	if err := SaveIfUnchanged(cfg, path, snapshot); err != nil {
		t.Fatalf("retry after handle closes: %v", err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Email != cfg.Email {
		t.Fatalf("replacement did not publish: %v", err)
	}
}
