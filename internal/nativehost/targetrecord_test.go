// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package nativehost

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTargetRecordRoundTrip(t *testing.T) {
	record := formatTargetRecord(`C:\Users\a b\papio.exe`, "abc123")
	target, digest, err := parseTargetRecord([]byte(record))
	if err != nil || target != `C:\Users\a b\papio.exe` || digest != "abc123" {
		t.Fatalf("parseTargetRecord() = %q, %q, %v", target, digest, err)
	}
}

func TestTargetRecordRejectsUnrecognisedFormats(t *testing.T) {
	for name, record := range map[string]string{
		"legacy path only": "C:\\papio.exe\n",
		"empty":            "",
		"no digest":        "C:\\papio.exe\nhost-sha256=\n",
		"wrong key":        "C:\\papio.exe\nother=abc\n",
		"empty target":     "\nhost-sha256=abc\n",
		"extra line":       "C:\\papio.exe\nhost-sha256=abc\nmore\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseTargetRecord([]byte(record)); !errors.Is(err, errHostPairMismatch) {
				t.Fatalf("parseTargetRecord(%q) error = %v; want errHostPairMismatch", record, err)
			}
		})
	}
}

// TestVerifyHostPairRefusesMixedInstalls pins the crash window: whichever of
// the host image and its record was published last, a pair from different
// installs is refused rather than used to autostart a daemon.
func TestVerifyHostPairRefusesMixedInstalls(t *testing.T) {
	oldRecord := []byte(formatTargetRecord("old-daemon", "old-digest"))
	newRecord := []byte(formatTargetRecord("new-daemon", "new-digest"))

	if target, err := verifyHostPair(oldRecord, "old-digest"); err != nil || target != "old-daemon" {
		t.Fatalf("matching pair = %q, %v; want old-daemon, nil", target, err)
	}
	// New image published, record still from the previous install.
	if target, err := verifyHostPair(oldRecord, "new-digest"); !errors.Is(err, errHostPairMismatch) || target != "old-daemon" {
		t.Fatalf("new image + old record = %q, %v; want mismatch naming the recorded target", target, err)
	}
	// Record published, image still the previous one.
	if _, err := verifyHostPair(newRecord, "old-digest"); !errors.Is(err, errHostPairMismatch) {
		t.Fatalf("old image + new record error = %v; want errHostPairMismatch", err)
	}
	if target, err := verifyHostPair(newRecord, "new-digest"); err != nil || target != "new-daemon" {
		t.Fatalf("completed pair = %q, %v; want new-daemon, nil", target, err)
	}
}

func writeDigested(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := digestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// TestRunningHostDigestIsPinnedAtStart: after the host pins its own image, an
// install or rollback that swaps the file at the installed path must not change
// which image the record is validated against.
func TestRunningHostDigestIsPinnedAtStart(t *testing.T) {
	pinnedImage.Store(nil)
	t.Cleanup(func() { pinnedImage.Store(nil) })
	host := filepath.Join(t.TempDir(), "host")
	oldDigest := writeDigested(t, host, "old image")

	pinHostImage(host)
	newDigest := writeDigested(t, host, "new image") // swap after the host started

	got, err := runningHostDigest(host)
	if err != nil || got != oldDigest {
		t.Fatalf("runningHostDigest() = %q, %v; want the started image %q", got, err, oldDigest)
	}
	// The swapped-in install's record must be refused by the running old host.
	if _, err := verifyHostPair([]byte(formatTargetRecord("new-daemon", newDigest)), got); !errors.Is(err, errHostPairMismatch) {
		t.Fatalf("running old host accepted the new install's record: %v", err)
	}
	// Unpinned callers (status) hash the installed file.
	pinnedImage.Store(nil)
	if got, err := runningHostDigest(host); err != nil || got != newDigest {
		t.Fatalf("unpinned runningHostDigest() = %q, %v; want %q", got, err, newDigest)
	}
}

func TestPinHostImageRecordsUnreadableImage(t *testing.T) {
	pinnedImage.Store(nil)
	t.Cleanup(func() { pinnedImage.Store(nil) })
	missing := filepath.Join(t.TempDir(), "missing")
	pinHostImage(missing)
	if _, err := runningHostDigest(missing); err == nil {
		t.Fatal("runningHostDigest() after a failed pin = nil error, want the hash failure")
	}
}

func TestDigestAndReadFileShared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	digest := writeDigested(t, path, "content")
	if data, err := readFileShared(path); err != nil || string(data) != "content" {
		t.Fatalf("readFileShared() = %q, %v", data, err)
	}
	if again, err := digestFile(path); err != nil || again != digest {
		t.Fatalf("digestFile() = %q, %v; want %q", again, err, digest)
	}
	if _, err := digestFile(path + ".missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("digestFile(missing) error = %v; want os.ErrNotExist", err)
	}
}
