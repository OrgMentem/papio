package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A daemon blocked on a keychain read must not be killed as a hung start.
func TestCredentialsPhaseIsStartupWork(t *testing.T) {
	status := Status{Phase: PhaseCredentials}
	if !status.Phase.storeWork() {
		t.Fatal("credentials phase is not exempt from the start deadline")
	}
	if got := status.Activity(); !strings.Contains(got, "keychain") {
		t.Fatalf("Activity() = %q, want it to name the keychain", got)
	}
	if phaseOrder[PhaseStarting] >= phaseOrder[PhaseCredentials] || phaseOrder[PhaseCredentials] >= phaseOrder[PhaseUpgrading] {
		t.Fatal("credentials phase must sit between starting and upgrading")
	}
}

// failStatusWrites replaces the status writer for one test and restores it
// afterwards. The status file stays removable.
func failStatusWrites(t *testing.T, write func(path string, status Status) error) {
	t.Helper()
	original, originalRemove := writeStatusFile, removeStatusFile
	originalWait, originalRetry := storeWorkStatusWait, storeWorkStatusRetry
	writeStatusFile = write
	storeWorkStatusWait, storeWorkStatusRetry = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		writeStatusFile, removeStatusFile = original, originalRemove
		storeWorkStatusWait, storeWorkStatusRetry = originalWait, originalRetry
	})
}

// A store-work phase the status file misses leaves "starting" for a waiting
// command to act on, so the write is retried until it lands (papio-fd19e264bf0e5382).
func TestInstanceRetriesTheStatusOfStoreWork(t *testing.T) {
	dataDir := t.TempDir()
	socket := filepath.Join(dataDir, "papio.sock")
	instance, err := AcquireInstance(dataDir, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Release)
	var attempts atomic.Int32
	failStatusWrites(t, func(path string, status Status) error {
		if attempts.Add(1) <= 3 {
			return errors.New("status file is busy")
		}
		return writeStatus(path, status)
	})
	instance.SetPhase(PhaseUpgrading)
	if status, held := ReadInstance(dataDir); !held || status.Phase != PhaseUpgrading {
		t.Fatalf("status = %+v held %v after %d attempts; want upgrading once a retry lands", status, held, attempts.Load())
	}
}

// When the status stays unwritable, the stale "starting" must not stay behind
// to be read as proof the daemon has begun no store work.
func TestInstanceClearsAStaleStatusItCannotReplace(t *testing.T) {
	dataDir := t.TempDir()
	socket := filepath.Join(dataDir, "papio.sock")
	instance, err := AcquireInstance(dataDir, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Release)
	if status, held := ReadInstance(dataDir); !held || status.Phase != PhaseStarting {
		t.Fatalf("status = %+v held %v, want the claim recording starting", status, held)
	}
	failStatusWrites(t, func(string, Status) error { return errors.New("status file is not writable") })
	instance.SetPhase(PhaseUpgrading)
	if status, held := ReadInstance(dataDir); !held || status != (Status{}) {
		t.Fatalf("status = %+v held %v; want the claim held with no readable status, not a stale %q", status, held, PhaseStarting)
	}
}

// A daemon that can neither announce store work nor clear the stale status
// must not begin the work: a command that launched it would stop it at the
// start deadline, and the daemon is the only one that can refuse in time
// (papio-fd19e264bf0e5382).
func TestInstanceRefusesStoreWorkItCannotAnnounce(t *testing.T) {
	dataDir := t.TempDir()
	socket := filepath.Join(dataDir, "papio.sock")
	instance, err := AcquireInstance(dataDir, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Release)
	failStatusWrites(t, func(string, Status) error { return errors.New("status file is not writable") })
	removeStatusFile = func(string) error { return errors.New("status file is not removable") }

	err = instance.BeginStoreWork(PhaseCredentials)
	if err == nil || !strings.Contains(err.Error(), "not writable") || !strings.Contains(err.Error(), "not removable") {
		t.Fatalf("BeginStoreWork = %v; want a refusal naming the failed write and the failed removal", err)
	}
	// The refusal leaves the daemon where it was, so a later attempt can
	// still announce the phase once the status file works again.
	writeStatusFile, removeStatusFile = writeStatus, os.Remove
	if err := instance.BeginStoreWork(PhaseCredentials); err != nil {
		t.Fatalf("BeginStoreWork once the status file works = %v", err)
	}
	if status, held := ReadInstance(dataDir); !held || status.Phase != PhaseCredentials {
		t.Fatalf("status = %+v held %v, want credentials", status, held)
	}
	if err := instance.BeginStoreWork(PhaseRunning); err == nil {
		t.Fatal("BeginStoreWork accepted a phase that is not store work")
	}
}
