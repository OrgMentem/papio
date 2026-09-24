//go:build !windows

// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/store"
)

// The environment that turns this test image into upgradingDaemon.
const (
	upgradingDaemonSocketEnv = "PAPIO_TEST_UPGRADING_DAEMON_SOCKET"
	upgradingDaemonDataEnv   = "PAPIO_TEST_UPGRADING_DAEMON_DATA"
	// upgradingDaemonHoldEnv names the phase the daemon stops in:
	// "upgrading" (the default) or "checking".
	upgradingDaemonHoldEnv = "PAPIO_TEST_UPGRADING_DAEMON_HOLD"
)

func TestMain(m *testing.M) {
	if socket := os.Getenv(upgradingDaemonSocketEnv); socket != "" {
		os.Exit(upgradingDaemon(socket, os.Getenv(upgradingDaemonDataEnv)))
	}
	if socket := os.Getenv(legacyDaemonSocketEnv); socket != "" {
		os.Exit(legacyDaemon(socket))
	}
	os.Exit(m.Run())
}

// upgradingDaemon starts as the papio daemon does: it claims the instance,
// then opens a new store under the instance's trace, so it reports its phases
// exactly as a real daemon does. It stops in one phase, the first migration
// or the integrity check, for the parent test: it prints the phase name once
// the phase is recorded, and carries on only when the parent writes a line to
// its stdin. That is store work exactly as slow as the test needs. Then it
// serves the socket until its stdin closes.
func upgradingDaemon(socket, dataDir string) int {
	instance, err := AcquireInstance(dataDir, socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrading daemon:", err)
		return 3
	}
	defer instance.Release()
	hold := os.Getenv(upgradingDaemonHoldEnv)
	if hold == "" {
		hold = "upgrading"
	}
	stdin := bufio.NewReader(os.Stdin)
	trace := instance.StoreTrace()
	parentGone := false
	gate := func(phase string) {
		if phase != hold {
			return
		}
		fmt.Println(phase)
		if _, err := stdin.ReadString('\n'); err != nil {
			parentGone = true
		}
	}
	recordUpgrade, recordCheck := trace.Migrating, trace.Checking
	trace.Migrating = func(from, to int) {
		recordUpgrade(from, to)
		gate("upgrading")
	}
	trace.Checking = func() {
		recordCheck()
		gate("checking")
	}
	db, err := store.Open(store.WithOpenTrace(context.Background(), trace), dataDir)
	if parentGone {
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrading daemon:", err)
		return 1
	}
	defer db.Close()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "upgrading daemon:", err)
		return 1
	}
	defer listener.Close()
	instance.SetPhase(PhaseRunning)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, _ = io.Copy(io.Discard, stdin)
	return 0
}

// upgradeFixture launches upgradingDaemon through Autostarter's seams and
// records every launch. Each launch gets its own stdin gate, which release
// opens, and its own output pipe, which Start reads until the daemon reports
// that it has reached the phase it holds in.
type upgradeFixture struct {
	t       *testing.T
	dir     string
	socket  string
	dataDir string
	image   string
	// hold is the phase launched daemons stop in; empty means "upgrading".
	hold string
	// afterHold, when set, runs in Start once the daemon has reached hold.
	afterHold func()

	mu       sync.Mutex
	launched []*exec.Cmd
	gates    []*os.File
	pending  *os.File // the read end of the newest launch's output
	logs     bytes.Buffer
}

func newUpgradeFixture(t *testing.T) *upgradeFixture {
	t.Helper()
	// A Unix socket path must stay under about 100 bytes, and t.TempDir's
	// macOS path alone can exceed that.
	dir, err := os.MkdirTemp("", "papio-upgrade-")
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &upgradeFixture{
		t:       t,
		dir:     dir,
		socket:  filepath.Join(dir, "papio.sock"),
		dataDir: filepath.Join(dir, "data"),
		image:   image,
	}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, gate := range f.gates {
			_ = gate.Close()
		}
		for _, cmd := range f.launched {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
		_ = os.RemoveAll(dir)
	})
	return f
}

// starter returns an Autostarter that launches upgradingDaemon. Its start
// deadline has passed by the time Start returns: the autostarter that
// terminated a daemon mid-migration and failed with "wait for daemon socket:
// context deadline exceeded" did exactly that at its deadline.
func (f *upgradeFixture) starter(onWait func(Status)) *Autostarter {
	return &Autostarter{
		SocketPath:    f.socket,
		DataDir:       f.dataDir,
		LogPath:       filepath.Join(f.dir, "daemon.log"),
		StartTimeout:  time.Nanosecond,
		RetryInterval: 5 * time.Millisecond,
		OnWait:        onWait,
		Executable:    func() (string, error) { return f.image, nil },
		Command: func(name string, _ ...string) *exec.Cmd {
			cmd := exec.Command(name)
			cmd.Env = append(os.Environ(), upgradingDaemonSocketEnv+"="+f.socket, upgradingDaemonDataEnv+"="+f.dataDir, upgradingDaemonHoldEnv+"="+f.hold)
			f.mu.Lock()
			f.launched = append(f.launched, cmd)
			f.mu.Unlock()
			return cmd
		},
		OpenNull: func() (*os.File, error) {
			read, write, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.gates = append(f.gates, write)
			f.mu.Unlock()
			return read, nil
		},
		OpenLog: func() (*os.File, error) {
			read, write, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.pending = read
			f.mu.Unlock()
			return write, nil
		},
		Start: func(_ context.Context, cmd *exec.Cmd) error {
			f.mu.Lock()
			output := f.pending
			f.mu.Unlock()
			if err := cmd.Start(); err != nil {
				return err
			}
			if err := f.awaitHold(output); err != nil {
				return err
			}
			if f.afterHold != nil {
				f.afterHold()
			}
			return nil
		},
	}
}

// awaitHold reads a launched daemon's output until it reports the phase it
// holds in, then keeps draining it for the failure report.
func (f *upgradeFixture) awaitHold(output *os.File) error {
	hold := f.hold
	if hold == "" {
		hold = "upgrading"
	}
	lines := bufio.NewReader(output)
	for {
		line, err := lines.ReadString('\n')
		f.mu.Lock()
		f.logs.WriteString(line)
		f.mu.Unlock()
		if strings.TrimSpace(line) == hold {
			break
		}
		if err != nil {
			return fmt.Errorf("upgrading daemon exited before it reached %s: %w", hold, err)
		}
	}
	go func() {
		defer output.Close()
		rest, _ := io.ReadAll(lines)
		f.mu.Lock()
		f.logs.Write(rest)
		f.mu.Unlock()
	}()
	return nil
}

// release lets every launched daemon apply its migrations.
func (f *upgradeFixture) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, gate := range f.gates {
		_, _ = gate.WriteString("go\n")
	}
}

func (f *upgradeFixture) launches() []*exec.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*exec.Cmd(nil), f.launched...)
}

func (f *upgradeFixture) output() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs.String()
}

// TestConcurrentStartersWaitForOneUpgradingDaemon reproduces an upgrade whose
// migration outlasts the start deadline while several commands and browser
// native hosts try to start the daemon at once. Every one of them must wait
// for the one daemon that is migrating: none may terminate it at a deadline,
// none may start a second daemon, and all must succeed once it serves.
func TestConcurrentStartersWaitForOneUpgradingDaemon(t *testing.T) {
	f := newUpgradeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	const starters = 4
	notices := make(chan Status, 4*starters)
	results := make(chan error, starters)
	for range starters {
		go func() {
			_, err := f.starter(func(status Status) { notices <- status }).EnsureWithResult(ctx)
			results <- err
		}()
	}
	for waiting := 0; waiting < starters; {
		select {
		case status := <-notices:
			if status.Phase != PhaseUpgrading {
				t.Fatalf("a starter waited on %+v, want the daemon upgrading", status)
			}
			waiting++
		case err := <-results:
			t.Fatalf("a starter returned while the daemon was still migrating: %v\ndaemon output:\n%s", err, f.output())
		}
	}
	f.release()
	for range starters {
		if err := <-results; err != nil {
			t.Fatalf("starter after the migration: %v\ndaemon output:\n%s", err, f.output())
		}
	}
	launched := f.launches()
	if len(launched) != 1 {
		t.Fatalf("launched %d daemons, want exactly one", len(launched))
	}
	if isProcessGone(t, launched[0]) {
		t.Fatalf("the daemon that migrated is gone; no start deadline may stop it\ndaemon output:\n%s", f.output())
	}
	// The daemon serves before it records PhaseRunning, so only its claim on
	// the socket is certain here.
	if status, held := ReadInstance(f.dataDir); !held || status.PID != launched[0].Process.Pid {
		t.Fatalf("instance = %+v held %v, want it held by the one launched daemon", status, held)
	}
}

// TestStarterGivesUpOnALongUpgradeWithoutStoppingIt pins the bound on the
// wait: a command stops waiting for a migration after MaxWait and says so,
// but the migration goes on, because stopping it would only make the next
// start do it again from the beginning.
func TestStarterGivesUpOnALongUpgradeWithoutStoppingIt(t *testing.T) {
	f := newUpgradeFixture(t)
	starter := f.starter(nil)
	starter.MaxWait = time.Nanosecond
	_, err := starter.EnsureWithResult(context.Background())
	if err == nil || !strings.Contains(err.Error(), "still creating the database") || !strings.Contains(err.Error(), "keeps running") {
		t.Fatalf("EnsureWithResult error = %v, want the daemon still creating the database and left running", err)
	}
	launched := f.launches()
	if len(launched) != 1 || isProcessGone(t, launched[0]) {
		t.Fatalf("launched %d daemons, and the migrating one must still be running\ndaemon output:\n%s", len(launched), f.output())
	}
	if status, held := ReadInstance(f.dataDir); !held || status.Phase != PhaseUpgrading {
		t.Fatalf("instance = %+v held %v, want the daemon still upgrading", status, held)
	}
}

// TestStarterWaitsForAStoppingDaemonInsteadOfStartingASecond covers the
// deploy sequence `papio daemon stop` then any command: the old daemon has
// closed its socket but is still finishing its work in the store. A second
// daemon started beside it would migrate while the old one still writes, so
// the command waits for the old process to exit and then starts one.
func TestStarterWaitsForAStoppingDaemonInsteadOfStartingASecond(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "papio.sock")
	previous, err := AcquireInstance(filepath.Dir(socket), socket)
	if err != nil {
		t.Fatal(err)
	}
	previous.SetPhase(PhaseRunning)
	previous.SetPhase(PhaseStopping)

	var mu sync.Mutex
	starts := 0
	notices := make(chan Status, 4)
	starter := &Autostarter{
		SocketPath:    socket,
		RetryInterval: time.Millisecond,
		Executable:    func() (string, error) { return "/test/papio", nil },
		Start: func(context.Context, *exec.Cmd) error {
			mu.Lock()
			defer mu.Unlock()
			starts++
			return nil
		},
		Ready: func(context.Context, string) error {
			mu.Lock()
			defer mu.Unlock()
			if starts == 0 {
				return errors.New("not ready")
			}
			return nil
		},
		OnWait: func(status Status) { notices <- status },
	}
	type ensured struct {
		result EnsureResult
		err    error
	}
	done := make(chan ensured, 1)
	go func() {
		result, err := starter.EnsureWithResult(context.Background())
		done <- ensured{result, err}
	}()
	select {
	case status := <-notices:
		if status.Phase != PhaseStopping || status.Waiting() != "waiting for the previous daemon to stop" {
			t.Fatalf("waited on %+v (%q), want the previous daemon stopping", status, status.Waiting())
		}
	case got := <-done:
		t.Fatalf("EnsureWithResult = %+v, %v while the previous daemon was still stopping; it must wait for it", got.result, got.err)
	}
	mu.Lock()
	early := starts
	mu.Unlock()
	if early != 0 {
		t.Fatalf("started %d daemons beside one that is still stopping", early)
	}
	previous.Release()
	got := <-done
	if got.err != nil || !got.result.Started {
		t.Fatalf("EnsureWithResult = %+v, %v; want a daemon started once the previous one exited", got.result, got.err)
	}
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
}

// TestCancelledStarterLeavesAnUnobservedUpgradeRunning covers a caller that
// gives up (Ctrl-C) after the daemon it launched has recorded that it is
// upgrading but before the wait loop has read that phase. Terminating the
// daemon then would roll the migration back for the next start to repeat, so
// the phase is read again before any signal.
func TestCancelledStarterLeavesAnUnobservedUpgradeRunning(t *testing.T) {
	f := newUpgradeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.afterHold = cancel
	starter := f.starter(nil)
	// Only the cancellation may end this wait.
	starter.StartTimeout = time.Hour
	_, err := starter.EnsureWithResult(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureWithResult error = %v, want context.Canceled", err)
	}
	launched := f.launches()
	if len(launched) != 1 || isProcessGone(t, launched[0]) {
		t.Fatalf("launched %d daemons, and the upgrading one must still be running\ndaemon output:\n%s", len(launched), f.output())
	}
	if status, held := ReadInstance(f.dataDir); !held || status.Phase != PhaseUpgrading {
		t.Fatalf("instance = %+v held %v, want the daemon still upgrading", status, held)
	}
}

// TestStarterSaysWhenItWaitsForTheIntegrityCheck pins the notice for the
// other long store step: the integrity check reads the whole database file,
// so a command waiting on it says so instead of sitting silent.
func TestStarterSaysWhenItWaitsForTheIntegrityCheck(t *testing.T) {
	f := newUpgradeFixture(t)
	f.hold = "checking"
	notices := make(chan Status, 4)
	starter := f.starter(func(status Status) { notices <- status })
	// The check is announced once it outlasts checkingNotice; any wait does.
	starter.checkingNotice = time.Nanosecond
	// Bounds only a failing run, which would otherwise wait ten minutes.
	starter.MaxWait = 5 * time.Second
	done := make(chan error, 1)
	go func() {
		_, err := starter.EnsureWithResult(context.Background())
		done <- err
	}()
	for checking := false; !checking; {
		select {
		case status := <-notices:
			if status.Phase != PhaseChecking {
				continue
			}
			if got, want := status.Waiting(), "checking the database; this can take a minute"; got != want {
				t.Fatalf("checking notice = %q, want %q", got, want)
			}
			checking = true
		case err := <-done:
			t.Fatalf("EnsureWithResult returned %v without saying it waited for the integrity check\ndaemon output:\n%s", err, f.output())
		}
	}
	f.release()
	if err := <-done; err != nil {
		t.Fatalf("EnsureWithResult after the check: %v\ndaemon output:\n%s", err, f.output())
	}
}
