//go:build !windows

// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package daemon

import (
	"bufio"
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
)

// legacyDaemonSocketEnv turns this test image into legacyDaemon.
const legacyDaemonSocketEnv = "PAPIO_TEST_LEGACY_DAEMON_SOCKET"

// legacyDaemon is a daemon from before the instance claim: it serves its
// socket and takes no instance lock. The first line on its stdin shuts it
// down as daemon.shutdown does, closing and removing its socket at once; it
// then goes on "draining" its work until a second line lets it exit.
func legacyDaemon(socket string) int {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "legacy daemon:", err)
		return 1
	}
	// Like the real server, a connection stays open until the client has
	// sent its request and gone, so a client can read the peer's pid.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			}()
		}
	}()
	fmt.Println("listening")
	stdin := bufio.NewReader(os.Stdin)
	if _, err := stdin.ReadString('\n'); err != nil {
		return 2
	}
	_ = listener.Close()
	fmt.Println("stopping")
	_, _ = stdin.ReadString('\n')
	return 0
}

// TestStopWaitsForALegacyDaemonToExit covers the first deploy of a binary
// with the instance claim: the running daemon is older and holds no claim,
// and it closes its socket before it finishes its work in the store. Stop
// must wait for that process to exit, and no command may start a new daemon
// on the same store until it has.
func TestStopWaitsForALegacyDaemonToExit(t *testing.T) {
	dir, err := os.MkdirTemp("", "papio-stop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "papio.sock")
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	legacy := exec.Command(image)
	legacy.Env = append(os.Environ(), legacyDaemonSocketEnv+"="+socket)
	control, err := legacy.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := legacy.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Start(); err != nil {
		t.Fatal(err)
	}
	// This test is the legacy daemon's parent, so it reaps it: an unreaped
	// child would stay visible to the kernel as a zombie.
	exited := make(chan struct{})
	go func() {
		_ = legacy.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = legacy.Process.Kill()
		<-exited
	})
	lines := bufio.NewReader(output)
	expect := func(want string) error {
		line, err := lines.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading the legacy daemon, want %q: %w", want, err)
		}
		if strings.TrimSpace(line) != want {
			return fmt.Errorf("legacy daemon said %q, want %q", line, want)
		}
		return nil
	}
	if err := expect("listening"); err != nil {
		t.Fatal(err)
	}

	waiting := make(chan int, 1)
	stopped := make(chan error, 1)
	go func() {
		stopped <- StopDaemon(context.Background(), socket, func(context.Context) error {
			if _, err := control.Write([]byte("stop\n")); err != nil {
				return err
			}
			return expect("stopping")
		}, func(pid int) { waiting <- pid })
	}()
	select {
	case pid := <-waiting:
		if pid != legacy.Process.Pid {
			t.Fatalf("StopDaemon waited for pid %d, want the daemon's pid %d", pid, legacy.Process.Pid)
		}
	case err := <-stopped:
		t.Fatalf("StopDaemon returned %v while the daemon was still finishing its work", err)
	}

	var mu sync.Mutex
	starts, probes := 0, 0
	startedEarly := false
	probing := make(chan struct{})
	starter := &Autostarter{
		SocketPath:    socket,
		RetryInterval: time.Millisecond,
		Executable:    func() (string, error) { return "/test/papio", nil },
		Start: func(context.Context, *exec.Cmd) error {
			mu.Lock()
			defer mu.Unlock()
			select {
			case <-exited:
			default:
				startedEarly = true
			}
			starts++
			return nil
		},
		Ready: func(context.Context, string) error {
			mu.Lock()
			defer mu.Unlock()
			if starts > 0 {
				return nil
			}
			if probes++; probes == 3 {
				close(probing)
			}
			return errors.New("not ready")
		},
	}
	ensured := make(chan error, 1)
	go func() {
		_, err := starter.EnsureWithResult(context.Background())
		ensured <- err
	}()
	// Three probes mean the starter is past its first check and retrying.
	<-probing
	if _, err := control.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if err := <-ensured; err != nil {
		t.Fatalf("EnsureWithResult: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if startedEarly || starts != 1 {
		t.Fatalf("starts = %d, started while the old daemon was still running = %v; want one start after it exited", starts, startedEarly)
	}
}
