// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package incident

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// This is the native before/after oracle for the former post-publication
// directory.Sync ERROR_ACCESS_DENIED. The first call must actually succeed.
func TestWindowsIncidentKeyFirstPublicationAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyName)
	var staged []byte
	var stagedID windowsIncidentFileID
	var stagedDACL string
	incidentKeyPublicationHook = func(point string) error {
		if point != "closed" {
			return nil
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("key published before staged file was closed: %v", err)
		}
		paths, err := filepath.Glob(filepath.Join(dir, "."+KeyName+".tmp-*"))
		if err != nil || len(paths) != 1 {
			return errors.New("missing exclusive staging file")
		}
		staged, err = os.ReadFile(paths[0])
		if err != nil {
			return err
		}
		if len(staged) != KeySize {
			return errors.New("incomplete staging file")
		}
		stagedID, err = readWindowsIncidentFileID(paths[0])
		if err != nil {
			return err
		}
		sd, err := windows.GetNamedSecurityInfo(paths[0], windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		stagedDACL = sd.String()
		return nil
	}
	t.Cleanup(func() { incidentKeyPublicationHook = nil })
	key, err := LoadOrCreateKey(dir)
	incidentKeyPublicationHook = nil
	if err != nil {
		t.Fatalf("first native publication failed: %v", err)
	}
	if len(key) != KeySize || !bytes.Equal(key, staged) {
		t.Fatal("publication did not preserve complete staged bytes")
	}
	publishedID, err := readWindowsIncidentFileID(path)
	if err != nil {
		t.Fatalf("reading published file identity: %v", err)
	}
	if publishedID != stagedID {
		t.Fatalf("publication replaced the staged file identity: staged=%+v published=%+v", stagedID, publishedID)
	}
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || stagedDACL == "" || before.String() != stagedDACL {
		t.Fatalf("publication changed inherited DACL: %v", err)
	}
	loaded, err := LoadOrCreateKey(dir)
	if err != nil || !bytes.Equal(loaded, key) {
		t.Fatalf("reload did not preserve key: %v", err)
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || after.String() != before.String() {
		t.Fatalf("reload changed DACL: %v", err)
	}
}

type windowsIncidentFileID struct {
	volumeSerialNumber uint32
	fileIndexHigh      uint32
	fileIndexLow       uint32
}

// Capture identity while the pathname exists. os.Stat followed by os.SameFile
// resolves Windows identity lazily, after publication has removed the staging name.
func readWindowsIncidentFileID(path string) (windowsIncidentFileID, error) {
	file, err := os.Open(path)
	if err != nil {
		return windowsIncidentFileID{}, err
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info)
	if err = errors.Join(err, file.Close()); err != nil {
		return windowsIncidentFileID{}, err
	}
	return windowsIncidentFileID{
		volumeSerialNumber: info.VolumeSerialNumber,
		fileIndexHigh:      info.FileIndexHigh,
		fileIndexLow:       info.FileIndexLow,
	}, nil
}

func TestWindowsIncidentKeyReadOnlyArtifactStaysReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyName)
	key := bytes.Repeat([]byte{'k'}, KeySize)
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	loaded, err := LoadOrCreateKey(dir)
	if err != nil || !bytes.Equal(loaded, key) {
		t.Fatalf("loading read-only complete key: %v", err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil || attrs&windows.FILE_ATTRIBUTE_READONLY == 0 {
		t.Fatalf("loading key made it writable: attributes=%x err=%v", attrs, err)
	}
}
