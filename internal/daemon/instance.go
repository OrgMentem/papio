// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"papio/internal/store"
)

// Phase is how far a daemon process has got between its start and its exit.
// The daemon records it in its data directory, so that a command waiting for
// the socket can tell a daemon that is busy from one that is hung, and can
// tell the person why it waits.
type Phase string

const (
	// PhaseStarting covers the steps from process start to the socket that
	// are not store work.
	PhaseStarting Phase = "starting"
	// PhaseUpgrading means the daemon is applying store migrations. One
	// migration can rewrite every row of a table and run for minutes.
	PhaseUpgrading Phase = "upgrading"
	// PhaseChecking means the daemon is running the store's startup
	// integrity check, which reads the whole database file.
	PhaseChecking Phase = "checking"
	// PhaseRunning means the daemon's socket is serving.
	PhaseRunning Phase = "running"
	// PhaseStopping means the daemon has closed its socket and is finishing
	// its work before it exits.
	PhaseStopping Phase = "stopping"
)

// phaseOrder is the one direction a daemon moves through its phases.
var phaseOrder = map[Phase]int{
	PhaseStarting:  0,
	PhaseUpgrading: 1,
	PhaseChecking:  2,
	PhaseRunning:   3,
	PhaseStopping:  4,
}

// storeWork reports whether the phase is store work that runs as long as the
// database needs. No start deadline may interrupt it: a migration that is
// stopped part way is rolled back, and the next start begins it again.
func (p Phase) storeWork() bool { return p == PhaseUpgrading || p == PhaseChecking }

// Status is what a daemon process records about itself while it holds its
// instance.
type Status struct {
	PID   int   `json:"pid"`
	Phase Phase `json:"phase"`
	// FromSchema is the schema version an upgrade started from. Zero during
	// PhaseUpgrading means the daemon is creating a new database.
	FromSchema int `json:"from_schema,omitempty"`
	// Socket is the socket this daemon serves, or will serve.
	Socket string `json:"socket,omitempty"`
}

// Activity names what the daemon is doing, for a person.
func (s Status) Activity() string {
	switch s.Phase {
	case PhaseUpgrading:
		if s.FromSchema == 0 {
			return "creating the database"
		}
		return "upgrading the database"
	case PhaseChecking:
		return "checking the database"
	case PhaseStopping:
		return "stopping"
	case PhaseRunning:
		return "running"
	default:
		return "starting"
	}
}

// Waiting says, for a person, what a command waits for while the daemon has
// this status.
func (s Status) Waiting() string {
	switch s.Phase {
	case PhaseUpgrading, PhaseChecking:
		return s.Activity() + "; this can take a minute"
	case PhaseStopping:
		return "waiting for the previous daemon to stop"
	default:
		return "waiting for the daemon: " + s.Activity()
	}
}

// ErrInstanceHeld means another live papio daemon process owns the data
// directory: it is starting, upgrading the database, serving, or stopping.
var ErrInstanceHeld = errors.New("another papio daemon process owns this data directory")

// A command that probes the instance lock holds it only for the probe, so
// AcquireInstance retries long enough to outlast one. A daemon holds the lock
// for its whole life.
const (
	instanceAcquireWait  = 500 * time.Millisecond
	instanceAcquireRetry = 5 * time.Millisecond
)

// Instance is a daemon process's claim on its data directory, and so on the
// database in it, held from before it opens the store until it exits. Only one
// process can hold it, so only one daemon migrates a database, a daemon
// started with another --socket cannot open a database that one already
// serves, and a second daemon never runs beside one that is still stopping.
// The lock file lives in the directory itself, so every spelling of the
// directory's path, through a symlink or not, reaches the same file. The
// operating system releases the lock when the process exits, however it
// exits, so a crash leaves no stale claim.
type Instance struct {
	mu         sync.Mutex
	lock       *os.File
	statusPath string
	status     Status
	released   bool
}

func instanceLockPath(dataDir string) string   { return filepath.Join(dataDir, "papio.daemon.lock") }
func instanceStatusPath(dataDir string) string { return filepath.Join(dataDir, "papio.daemon.json") }

// AcquireInstance claims dataDir for this process, which will serve
// socketPath, in PhaseStarting. It returns ErrInstanceHeld when another daemon
// process holds the claim.
func AcquireInstance(dataDir, socketPath string) (*Instance, error) {
	// #nosec G703 -- dataDir is this daemon's own data directory from its
	// config; the instance files live in it with the same owner.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create daemon data directory: %w", err)
	}
	// #nosec G703 -- same provenance as the directory above.
	file, err := os.OpenFile(instanceLockPath(dataDir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon instance lock: %w", err)
	}
	deadline := time.Now().Add(instanceAcquireWait)
	for {
		locked, err := tryLockFile(file)
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("lock daemon instance: %w", err)
		}
		if locked {
			break
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, ErrInstanceHeld
		}
		time.Sleep(instanceAcquireRetry)
	}
	instance := &Instance{
		lock:       file,
		statusPath: instanceStatusPath(dataDir),
		status:     Status{PID: os.Getpid(), Phase: PhaseStarting, Socket: socketPath},
	}
	instance.record()
	return instance, nil
}

// SetPhase records that the daemon has reached phase. A phase at or before
// the recorded one is ignored, because a daemon only moves forward: a store
// opened again while the daemon serves must not report it as checking.
func (i *Instance) SetPhase(phase Phase) {
	i.advance(Status{Phase: phase})
}

// StoreTrace reports the slow steps of the daemon's store open as its phase.
func (i *Instance) StoreTrace() *store.OpenTrace {
	return &store.OpenTrace{
		Migrating: func(from, _ int) { i.advance(Status{Phase: PhaseUpgrading, FromSchema: from}) },
		Checking:  func() { i.SetPhase(PhaseChecking) },
	}
}

// Release gives up the claim and removes the status. A daemon calls it after
// it has closed its store.
func (i *Instance) Release() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.released {
		return
	}
	i.released = true
	// #nosec G703 -- statusPath is in the data directory AcquireInstance was
	// given, as the lock file is.
	_ = os.Remove(i.statusPath)
	_ = unlockFile(i.lock)
	_ = i.lock.Close()
}

func (i *Instance) advance(next Status) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.released || phaseOrder[next.Phase] <= phaseOrder[i.status.Phase] {
		return
	}
	next.PID, next.Socket = i.status.PID, i.status.Socket
	i.status = next
	i.record()
}

// record writes the status file. The status only tells waiting commands what
// they wait for, and the lock is the claim, so a failed write is logged and
// the daemon carries on.
func (i *Instance) record() {
	if err := writeStatus(i.statusPath, i.status); err != nil {
		// #nosec G706 -- the phase is one of this package's constants and the
		// error names this daemon's own status file.
		log.Printf("papio: recording daemon phase %s: %v", i.status.Phase, err)
	}
}

// writeStatus replaces the status file whole, so a reader never sees half of
// one. Windows refuses to replace a file while a reader has it open, and a
// reader holds it only for one read, so the rename is retried briefly.
func writeStatus(path string, status Status) error {
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	staging := path + ".new"
	// #nosec G703 -- path is this daemon's own status file in its data
	// directory (see AcquireInstance); staging only adds a fixed suffix to it.
	if err := os.WriteFile(staging, data, 0o600); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		// #nosec G703 -- the same two paths as the write above.
		err = os.Rename(staging, path)
		if err == nil || attempt == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		// #nosec G703 -- the staging file this function wrote.
		_ = os.Remove(staging)
	}
	return err
}

// ReadInstance reports whether a live daemon process holds dataDir's
// instance, and the status it last recorded. When no daemon holds it, the
// probe holds the lock for the moment it takes, which AcquireInstance
// tolerates. An unreadable status is returned as the zero Status.
func ReadInstance(dataDir string) (Status, bool) {
	file, err := os.OpenFile(instanceLockPath(dataDir), os.O_RDWR, 0)
	if err != nil {
		return Status{}, false
	}
	defer file.Close()
	locked, err := tryLockFile(file)
	if err != nil {
		return Status{}, false
	}
	if locked {
		_ = unlockFile(file)
		return Status{}, false
	}
	var status Status
	if data, err := os.ReadFile(instanceStatusPath(dataDir)); err == nil {
		if json.Unmarshal(data, &status) != nil {
			status = Status{}
		}
	}
	return status, true
}
