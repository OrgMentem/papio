// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package nativehost implements papio's browser native-messaging host bridge.
//
// The host is a thin, disposable relay: a browser launches papio-native-host,
// hands it its extension identity as an untrusted argument, and speaks the
// locked papio-browser/1 protocol over stdin/stdout using the browser's 4-byte
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

// InvokedAsHost reports whether argv0 names the native-messaging host
// executable a browser launched. Browsers start a fixed-name file
// (papio-native-host, or papio-native-host.exe on Windows); dispatch keys off
// that basename, ignoring any executable extension.
func InvokedAsHost(argv0 string) bool {
	base := filepath.Base(argv0)
	return strings.TrimSuffix(base, filepath.Ext(base)) == nativeHostBasename
}

// resolveExecutablePath resolves exe through any symlinks (falling back to exe
// when resolution fails) and refuses a path that still dispatches as the native
// host, which would loop autostart back into this mode instead of the daemon.
func resolveExecutablePath(exe string) (string, error) {
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	if InvokedAsHost(resolved) {
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
// or mismatched browser extension identity, ensures the daemon is running, and
// relays frames between the browser and the daemon until stdin closes or ctx is cancelled.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Browser.ExtensionID == "" && cfg.Browser.FirefoxExtensionID == "" {
		return errors.New("browser bridge disabled: browser.extension_id and browser.firefox_extension_id are not configured")
	}
	if err := validateOrigin(args, cfg.Browser.ExtensionID, cfg.Browser.FirefoxExtensionID); err != nil {
		return fmt.Errorf("reject native-messaging origin: %w", err)
	}

	socket := filepath.Join(cfg.DataDir, "papio.sock")
	starter := daemon.NewAutostarter(socket)
	starter.Args = []string{"--config", cfg.Path, "daemon", "--socket", socket}
	// A browser launches this process through the installed host executable
	// (a symlink on Unix, a copy on Windows), so os.Executable reports that
	// host path. Autostart must spawn the real papio binary under its own
	// basename; otherwise the child re-dispatches into native-host mode
	// (basename == papio-native-host), never starts the daemon, and Ensure
	// times out on a socket that never appears. resolveDaemonExecutable is
	// platform-specific: it follows the symlink on Unix and reads the recorded
	// target on Windows.
	starter.Executable = resolveDaemonExecutable
	if err := starter.Ensure(ctx); err != nil {
		return fmt.Errorf("ensure daemon: %w", err)
	}

	syncer := &ipcSyncer{client: ipc.NewSocketClient(socket)}
	return newBridge(syncer, stdin, stdout, stderr).run(ctx)
}

// validateOrigin accepts only configured browser invocation identities. Chrome
// passes exactly "chrome-extension://<id>/"; Firefox passes its configured
// Gecko extension ID as a bare argument after the app manifest path. Every
// argument is untrusted and compared exactly, so neither browser can name an
// extension that the configuration did not allow.
func validateOrigin(args []string, chromeID, firefoxID string) error {
	if chromeID != "" {
		wantChromeOrigin := "chrome-extension://" + chromeID + "/"
		for _, arg := range args {
			if arg == wantChromeOrigin {
				return nil
			}
		}
	}
	if firefoxID != "" {
		for _, arg := range args {
			if arg == firefoxID {
				return nil
			}
		}
	}
	return errors.New("missing configured browser extension identity argument")
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
	pollFatal := make(chan error, 1)
	go func() {
		defer pollWG.Done()
		if err := b.pollLoop(pollCtx); err != nil {
			select {
			case pollFatal <- err:
			default:
			}
		}
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
		case err := <-pollFatal:
			return err
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
// stdout write failure does, because the port is gone: it is returned so run
// tears the whole bridge down instead of surviving as an inert, non-polling
// process that starves the extension of offers and cancels.
func (b *bridge) pollLoop(ctx context.Context) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			outbound, err := b.syncer.Sync(ctx, nil)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				_, _ = fmt.Fprintln(b.stderr, "papio-native-host: poll browser.sync:", err)
				continue
			}
			if err := b.writeOutbound(outbound); err != nil {
				_, _ = fmt.Fprintln(b.stderr, "papio-native-host: poll write:", err)
				return err
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
	binary.LittleEndian.PutUint32(header[:], uint32(len(data))) //nolint:gosec // G115: len(data) bounded by MaxBrowserMessageBytes check above.
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
