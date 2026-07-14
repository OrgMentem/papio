// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package nativehost implements papio's Chrome native-messaging host bridge.
//
// The host is a thin, disposable relay: Chrome launches papio-native-host,
// hands it the extension origin as an untrusted argument, and speaks the
// locked papio-browser/1 protocol over stdin/stdout using Chrome's 4-byte
// little-endian length framing. This process owns no durable state. It
// validates the origin, enforces framing and the fail-closed protocol
// invariants (bounded frame size, hello-first, strictly increasing seq), and
// forwards every metadata frame to the daemon's browser.sync RPC over the
// user-only Unix socket. Authoritative policy, hello_ack generation, and
// outbound seq numbering all live in the daemon.
//
// Stdout carries protocol frames only; every diagnostic goes to stderr. PDF
// bytes and secrets never transit this bridge — the protocol decoder in
// internal/protocol structurally forbids them.
package nativehost

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"papio/internal/config"
	"papio/internal/daemon"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/protocol"
)

// pollInterval bounds how long the bridge waits before draining any
// daemon-initiated frames (job_offer, cancel) while stdin is idle.
const pollInterval = 2 * time.Second

// syncMethod is the daemon RPC the bridge forwards browser frames through.
const syncMethod = "browser.sync"

// nativeHostBasename is the executable basename that main.go dispatches into
// native-host mode. A resolved daemon executable must never carry it, or
// autostart would spawn another native host instead of the daemon.
const nativeHostBasename = "papio-native-host"

// resolveDaemonExecutable returns the real papio binary to autostart as the
// daemon, resolving the installed papio-native-host symlink to its target.
func resolveDaemonExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return resolveExecutablePath(exe)
}

// resolveExecutablePath resolves exe through any symlinks (falling back to exe
// when resolution fails) and refuses a path that still dispatches as the native
// host, which would loop autostart back into this mode instead of the daemon.
func resolveExecutablePath(exe string) (string, error) {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	if filepath.Base(resolved) == nativeHostBasename {
		return "", fmt.Errorf("resolved executable %q still dispatches as native host; cannot autostart daemon", resolved)
	}
	return resolved, nil
}

// errFrameTooLarge is returned by readFrame when a length prefix exceeds the
// frame cap. It is reported before any body byte is allocated or read.
var errFrameTooLarge = errors.New("inbound frame exceeds size cap")

// Syncer forwards raw browser frames to the daemon and returns the daemon's
// outbound frames. It exists so the relay loop can be unit-tested without a
// live daemon; production wiring uses ipcSyncer over the Unix socket.
type Syncer interface {
	// Sync sends messages (possibly empty, meaning "poll") to the daemon and
	// returns any frames the daemon wants delivered to the extension.
	Sync(ctx context.Context, messages []json.RawMessage) ([]json.RawMessage, error)
}

// Run is the papio-native-host entrypoint. It loads config, refuses a missing
// or mismatched extension origin, ensures the daemon is running, and relays
// frames between Chrome and the daemon until stdin closes or ctx is cancelled.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Browser.ExtensionID == "" {
		return errors.New("browser bridge disabled: browser.extension_id is not configured")
	}
	if err := validateOrigin(args, cfg.Browser.ExtensionID); err != nil {
		return fmt.Errorf("reject native-messaging origin: %w", err)
	}

	socket := filepath.Join(cfg.DataDir, "papio.sock")
	starter := daemon.NewAutostarter(socket)
	starter.Args = []string{"--config", cfg.Path, "daemon", "--socket", socket}
	// Chrome launches this process through the installed papio-native-host
	// symlink, so os.Executable reports that symlink. Autostart must spawn the
	// real binary under its own basename; otherwise the child re-dispatches into
	// native-host mode (basename == papio-native-host), never starts the daemon,
	// and Ensure times out on a socket that never appears.
	starter.Executable = resolveDaemonExecutable
	if err := starter.Ensure(ctx); err != nil {
		return fmt.Errorf("ensure daemon: %w", err)
	}

	syncer := &ipcSyncer{client: ipc.NewUnixClient(socket)}
	return newBridge(syncer, stdin, stdout, stderr).run(ctx)
}

// validateOrigin enforces that Chrome supplied exactly the configured
// extension origin. The argument is untrusted: it is only compared, never
// trusted to name a different allowed extension. Chrome passes the origin as
// "chrome-extension://<id>/"; a trailing native-window handle (Windows) is
// ignored because it does not carry the scheme.
func validateOrigin(args []string, wantID string) error {
	const scheme = "chrome-extension://"
	for _, arg := range args {
		if !strings.HasPrefix(arg, scheme) {
			continue
		}
		id := strings.TrimPrefix(arg, scheme)
		if slash := strings.IndexByte(id, '/'); slash >= 0 {
			id = id[:slash]
		}
		if id == wantID {
			return nil
		}
		return fmt.Errorf("origin extension id does not match configured extension_id")
	}
	return errors.New("missing chrome-extension origin argument")
}

// ipcSyncer forwards frames to the daemon's browser.sync RPC. Each call opens a
// fresh one-shot connection (ipc.Client semantics); the daemon correlates the
// bridge session server-side.
type ipcSyncer struct {
	client *ipc.Client
}

// syncRequest is the browser.sync request body.
type syncRequest struct {
	Messages []json.RawMessage `json:"messages"`
}

// syncResponse is the browser.sync response body.
type syncResponse struct {
	Outbound []json.RawMessage `json:"outbound"`
}

func (s *ipcSyncer) Sync(ctx context.Context, messages []json.RawMessage) ([]json.RawMessage, error) {
	if messages == nil {
		messages = []json.RawMessage{}
	}
	var resp syncResponse
	if err := s.client.Call(ctx, job.NewID("rpc"), syncMethod, syncRequest{Messages: messages}, &resp); err != nil {
		return nil, err
	}
	return resp.Outbound, nil
}

// bridge is the stateful per-connection relay. lastSeq and seenHello enforce
// the fail-closed inbound invariants; writeMu serializes every stdout frame so
// the read path and the idle-poll goroutine never interleave a frame's bytes.
type bridge struct {
	syncer Syncer
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	writeMu sync.Mutex

	lastSeq   int64
	seenHello bool
}

func newBridge(syncer Syncer, stdin io.Reader, stdout, stderr io.Writer) *bridge {
	return &bridge{syncer: syncer, stdin: stdin, stdout: stdout, stderr: stderr, lastSeq: -1}
}

// run relays frames until stdin closes (Chrome closed the port), ctx is
// cancelled, or a protocol violation forces a non-zero exit. The idle-poll
// goroutine is always cancelled and joined before run returns.
func (b *bridge) run(ctx context.Context) error {
	pollCtx, cancelPoll := context.WithCancel(ctx)
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		b.pollLoop(pollCtx)
	}()
	defer func() {
		cancelPoll()
		pollWG.Wait()
	}()

	frames := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		for {
			frame, err := readFrame(b.stdin)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readErr:
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // Chrome closed the port: clean shutdown.
			}
			if errors.Is(err, errFrameTooLarge) {
				b.sendError("frame_too_large", "inbound frame exceeds size cap")
				return err
			}
			return err
		case frame := <-frames:
			if err := b.handleInbound(ctx, frame); err != nil {
				return err
			}
		}
	}
}

// handleInbound validates one inbound frame, then forwards it to the daemon and
// writes any resulting outbound frames. Every validation failure writes a
// best-effort error frame and returns a non-nil error so the process exits
// non-zero (the connection is considered bad).
func (b *bridge) handleInbound(ctx context.Context, frame []byte) error {
	msg, err := protocol.DecodeBrowserMessage(frame)
	if err != nil {
		_, _ = fmt.Fprintln(b.stderr, "papio-native-host: reject inbound frame:", err)
		b.sendError("invalid_frame", "inbound frame failed strict decode")
		return fmt.Errorf("decode inbound frame: %w", err)
	}

	if !b.seenHello {
		if msg.Type != protocol.MsgHello {
			b.sendError("expected_hello", "first frame must be hello")
			return fmt.Errorf("first frame type %q, want hello", msg.Type)
		}
		b.seenHello = true
	} else if msg.Type == protocol.MsgHello {
		b.sendError("unexpected_hello", "hello already received on this connection")
		return errors.New("duplicate hello frame")
	}

	if msg.Seq <= b.lastSeq {
		b.sendError("seq_regression", "seq must strictly increase")
		return fmt.Errorf("seq %d not greater than %d", msg.Seq, b.lastSeq)
	}
	b.lastSeq = msg.Seq

	outbound, err := b.syncer.Sync(ctx, []json.RawMessage{frame})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		_, _ = fmt.Fprintln(b.stderr, "papio-native-host: browser.sync:", err)
		b.sendError("daemon_unavailable", "daemon rejected or dropped the frame")
		return fmt.Errorf("%s: %w", syncMethod, err)
	}
	return b.writeOutbound(outbound)
}

// pollLoop drains daemon-initiated frames while stdin is idle. Sync errors are
// transient (the daemon may be restarting) and never terminate the bridge; a
// stdout write failure does, because the port is gone.
func (b *bridge) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			outbound, err := b.syncer.Sync(ctx, nil)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				_, _ = fmt.Fprintln(b.stderr, "papio-native-host: poll browser.sync:", err)
				continue
			}
			if err := b.writeOutbound(outbound); err != nil {
				_, _ = fmt.Fprintln(b.stderr, "papio-native-host: poll write:", err)
				return
			}
		}
	}
}

func (b *bridge) writeOutbound(frames []json.RawMessage) error {
	for _, frame := range frames {
		if err := b.writeFrame(frame); err != nil {
			return err
		}
	}
	return nil
}

// writeFrame emits one length-prefixed frame to stdout under writeMu. It
// enforces the frame cap on the way out too; an oversized daemon frame is a
// daemon bug and fails the connection rather than being truncated.
func (b *bridge) writeFrame(data []byte) error {
	if len(data) > protocol.MaxBrowserMessageBytes {
		return fmt.Errorf("outbound frame %d bytes exceeds cap %d", len(data), protocol.MaxBrowserMessageBytes)
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := b.stdout.Write(header[:]); err != nil {
		return err
	}
	_, err := b.stdout.Write(data)
	return err
}

// hostErrorFrame is a host-originated protocol error. It carries no job_id
// (error is not job-scoped) and a fresh seq of 0 because the connection is
// terminating; the daemon owns seq numbering for the normal outbound stream.
type hostErrorFrame struct {
	Protocol string                `json:"protocol"`
	Type     string                `json:"type"`
	MsgID    string                `json:"msg_id"`
	Seq      int64                 `json:"seq"`
	Payload  protocol.ErrorPayload `json:"payload"`
}

// sendError writes a best-effort protocol error frame. Failures are logged to
// stderr and swallowed: the caller is already returning a fatal error.
func (b *bridge) sendError(code, message string) {
	frame := hostErrorFrame{
		Protocol: protocol.BrowserProtocolVersion,
		Type:     protocol.MsgError,
		MsgID:    newMsgID(),
		Seq:      0,
		Payload:  protocol.ErrorPayload{Code: code, Message: message},
	}
	data, err := json.Marshal(frame)
	if err != nil {
		_, _ = fmt.Fprintln(b.stderr, "papio-native-host: encode error frame:", err)
		return
	}
	if err := b.writeFrame(data); err != nil {
		_, _ = fmt.Fprintln(b.stderr, "papio-native-host: write error frame:", err)
	}
}

// newMsgID returns a msg_id matching protocol.msgIDRE (^[A-Za-z0-9_-]{8,64}$).
func newMsgID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "hosterror000000" // valid-length fallback; crypto/rand never fails in practice.
	}
	return "h" + hex.EncodeToString(b[:])
}

// readFrame reads one length-prefixed frame from r. The size cap is enforced on
// the length prefix BEFORE any body byte is allocated or read, so an oversized
// or hostile length never drives an allocation. A clean EOF at a frame boundary
// is surfaced as io.EOF.
func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(header[:])
	if n > protocol.MaxBrowserMessageBytes {
		return nil, errFrameTooLarge
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
