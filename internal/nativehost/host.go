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
// Stdout carries protocol frames only; every diagnostic goes to stderr — and,
// because browsers discard a native host's stderr, to <DataDir>/native-host.log
// as well (see openDiagLog). PDF bytes and secrets never transit this bridge —
// the protocol decoder in internal/protocol structurally forbids them.
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

	"papio/internal/api"
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

// syncFailureDisposition is deliberately out-of-band from the IPC response:
// adding a field to syncResponse would make older hosts reject newer daemons.
// The host tears down a browser session only for an explicit fatal disposition.
type syncFailureDisposition uint8

const (
	syncApplicationFailure syncFailureDisposition = iota + 1
	syncTransportFailure
)

type syncFailure struct {
	disposition syncFailureDisposition
	err         error
}

func (e *syncFailure) Error() string { return e.err.Error() }
func (e *syncFailure) Unwrap() error { return e.err }

func applicationSyncFailure(err error) error {
	if err == nil {
		return nil
	}
	return &syncFailure{disposition: syncApplicationFailure, err: err}
}

func transportSyncFailure(err error) error {
	if err == nil {
		return nil
	}
	return &syncFailure{disposition: syncTransportFailure, err: err}
}

func isApplicationSyncFailure(err error) bool {
	var failure *syncFailure
	return errors.As(err, &failure) && failure.disposition == syncApplicationFailure
}

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

// diagLogName is the file under the papio data dir that every native-host
// diagnostic is mirrored into.
const diagLogName = "native-host.log"

// maxDiagLogBytes bounds that file. Past this it is rotated to
// diagLogName+rotatedDiagLogSuffix at process start, keeping exactly one
// previous generation — these are disposable diagnostics, not an audit trail.
const maxDiagLogBytes = 1 << 20

// rotatedDiagLogSuffix names the single retained previous generation.
const rotatedDiagLogSuffix = ".1"

// openDiagLog opens the native-host diagnostic log for appending.
//
// Chrome forwards a native host's stderr NOWHERE — not into chrome_debug.log,
// not even with --enable-logging --v=0 — so a host that rejects a frame and
// tears the session down leaves no browser-side trace at all. The operator
// sees only the downstream symptom (a nav_failed, a re-parked handoff, a
// session that will not connect) and the actual cause, which the host already
// names precisely, is discarded. Mirroring stderr here is what makes that
// message readable after the fact instead of only when someone reproduces the
// failure by driving the host by hand.
//
// Best effort by construction: a log that cannot be opened must never stop the
// relay, so every failure returns an error the caller ignores in favour of
// plain stderr.
func openDiagLog(dataDir string) (*os.File, error) {
	if dataDir == "" {
		return nil, errors.New("no data dir configured")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, diagLogName)
	// Rotate rather than truncate. Every native-messaging connection is its
	// own host process, and an MV3 service-worker reconnect (or Chrome and
	// Firefox connected at once) puts two of them on this file with
	// overlapping lifetimes. Truncating the path would discard whatever a live
	// sibling had already written — including the frame rejection that killed
	// its session, which is the one line this whole mechanism exists to keep.
	// A rename leaves that sibling's descriptor attached to the same inode, so
	// its trace survives under the rotated name and stays readable. Both
	// operations are best effort: a failed rotation just means this process
	// appends to a slightly oversized log.
	if info, err := os.Stat(path); err == nil && info.Size() > maxDiagLogBytes {
		_ = os.Rename(path, path+rotatedDiagLogSuffix)
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

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
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (err error) {
	// The host is entered by basename, so `papio-native-host --version` would
	// otherwise be parsed as an extension origin and rejected. Chrome always
	// passes exactly one origin argument and never a flag, so a lone --version
	// can only be a human — or `papio doctor`'s skew check — asking which binary
	// this symlink resolves to. Answer before loading config: a stale host is
	// precisely what the caller is diagnosing, and a config error must not hide
	// the version.
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Fprintln(stdout, "papio "+api.Version)
		return nil
	}
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Mirror diagnostics to disk from here on: everything below can fail the
	// session, and the browser will not show the operator any of it.
	if diag, diagErr := openDiagLog(cfg.DataDir); diagErr == nil {
		defer diag.Close()
		stderr = io.MultiWriter(stderr, diag)
		fmt.Fprintf(stderr, "papio-native-host %s: start pid=%d at %s\n",
			api.Version, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
		// Registered after the Close defer, so it runs first (LIFO). Every
		// fatal exit below returns rather than printing, so without this the
		// reason a session died never reaches the log at all.
		defer func() {
			if err != nil {
				fmt.Fprintf(stderr, "papio-native-host: exit at %s: %v\n",
					time.Now().UTC().Format(time.RFC3339), err)
			}
		}()
	}
	chromeIDs := cfg.Browser.ChromiumExtensionIDs()
	if len(chromeIDs) == 0 && cfg.Browser.FirefoxExtensionID == "" {
		return errors.New("browser bridge disabled: browser.extension_id and browser.firefox_extension_id are not configured")
	}
	if err := validateOrigin(args, chromeIDs, cfg.Browser.FirefoxExtensionID); err != nil {
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

	syncer := &ipcSyncer{client: ipc.NewSocketClient(socket), sessionID: newSessionID()}
	runErr := newBridge(syncer, stdin, stdout, stderr).run(ctx)
	// Best-effort goodbye so the daemon releases this browser's session
	// immediately instead of waiting out the staleness window. The relay ctx
	// may already be cancelled (browser closed the port), so use a fresh one.
	goodbyeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = syncer.goodbye(goodbyeCtx)
	return runErr
}

// newSessionID mints the per-native-host-process browser session identity the
// daemon arbitrates on. Identity must never abort the host: an entropy
// failure falls back to a process-unique string.
func newSessionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("fallback-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

// validateOrigin accepts only configured browser invocation identities. A
// Chromium browser passes exactly "chrome-extension://<id>/" — any of the
// configured Chrome-family IDs is accepted, so the same daemon serves the same
// extension across Chrome, Edge, Vivaldi, Brave, and Opera (and an Edge-store
// copy with a different ID). Firefox passes its configured Gecko extension ID as
// a bare argument after the app manifest path. Every argument is untrusted and
// compared exactly, so no browser can name an extension the configuration did
// not allow.
func validateOrigin(args []string, chromeIDs []string, firefoxID string) error {
	for _, chromeID := range chromeIDs {
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
// fresh one-shot connection (ipc.Client semantics); session_id lets the daemon
// arbitrate between concurrently connected browsers.
type ipcSyncer struct {
	client    *ipc.Client
	sessionID string
}

// syncRequest is the browser.sync request body.
type syncRequest struct {
	SessionID string            `json:"session_id,omitempty"`
	Goodbye   bool              `json:"goodbye,omitempty"`
	Messages  []json.RawMessage `json:"messages"`
}

// syncResponse is the browser.sync response body.
type syncResponse struct {
	Outbound []json.RawMessage `json:"outbound"`
}

// request builds the browser.sync body for this session.
func (s *ipcSyncer) request(goodbye bool, messages []json.RawMessage) syncRequest {
	if messages == nil {
		messages = []json.RawMessage{}
	}
	return syncRequest{SessionID: s.sessionID, Goodbye: goodbye, Messages: messages}
}

func (s *ipcSyncer) Sync(ctx context.Context, messages []json.RawMessage) ([]json.RawMessage, error) {
	var resp syncResponse
	if err := s.client.Call(ctx, job.NewID("rpc"), syncMethod, s.request(false, messages), &resp); err != nil {
		var remote *ipc.RemoteError
		if errors.As(err, &remote) && remote.Code == "application_failure" {
			// browser.sync uses this code only for an explicitly structured
			// application disposition. Other RPC errors (including internal,
			// invalid_argument, and result_too_large) remain fatal.
			return nil, applicationSyncFailure(err)
		}
		// Dial, framing, size-cap, malformed-response, and daemon
		// self-validation failures are explicitly fatal at the host boundary.
		return nil, transportSyncFailure(err)
	}
	return resp.Outbound, nil
}

// goodbye tells the daemon this browser session is gone. Fire-and-forget.
func (s *ipcSyncer) goodbye(ctx context.Context) error {
	var resp syncResponse
	return s.client.Call(ctx, job.NewID("rpc"), syncMethod, s.request(true, nil), &resp)
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

	pollInterval time.Duration
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
				if sendErr := b.sendError("frame_too_large", "inbound frame exceeds size cap"); sendErr != nil {
					return fmt.Errorf("send frame-too-large error: %w", sendErr)
				}
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
// writes any resulting outbound frames. Validation and transport/framing
// failures return a non-nil error; an explicitly application-level sync
// failure is logged, reported as one correlated error frame, and leaves the
// session live.
func (b *bridge) handleInbound(ctx context.Context, frame []byte) error {
	msg, err := protocol.DecodeBrowserMessage(frame)
	if err != nil {
		_, _ = fmt.Fprintln(b.stderr, "papio-native-host: reject inbound frame:", err)
		if sendErr := b.sendError("invalid_frame", "inbound frame failed strict decode"); sendErr != nil {
			return fmt.Errorf("send invalid-frame error: %w", sendErr)
		}
		return fmt.Errorf("decode inbound frame: %w", err)
	}

	if !b.seenHello {
		if msg.Type != protocol.MsgHello {
			if sendErr := b.sendError("expected_hello", "first frame must be hello", inboundRequestID(msg)); sendErr != nil {
				return fmt.Errorf("send expected-hello error: %w", sendErr)
			}
			return fmt.Errorf("first frame type %q, want hello", msg.Type)
		}
		b.seenHello = true
	} else if msg.Type == protocol.MsgHello {
		if sendErr := b.sendError("unexpected_hello", "hello already received on this connection", inboundRequestID(msg)); sendErr != nil {
			return fmt.Errorf("send duplicate-hello error: %w", sendErr)
		}
		return errors.New("duplicate hello frame")
	}

	if msg.Seq <= b.lastSeq {
		if sendErr := b.sendError("seq_regression", "seq must strictly increase", inboundRequestID(msg)); sendErr != nil {
			return fmt.Errorf("send sequence error: %w", sendErr)
		}
		return fmt.Errorf("seq %d not greater than %d", msg.Seq, b.lastSeq)
	}

	b.lastSeq = msg.Seq

	outbound, err := b.syncer.Sync(ctx, []json.RawMessage{frame})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if isApplicationSyncFailure(err) {
			_, _ = fmt.Fprintln(b.stderr, "papio-native-host: browser.sync application failure:", err)
			if sendErr := b.sendError("application_error", "the daemon could not complete this request", inboundRequestID(msg)); sendErr != nil {
				return fmt.Errorf("send application-error frame: %w", sendErr)
			}
			return nil
		}
		_, _ = fmt.Fprintln(b.stderr, "papio-native-host: browser.sync transport failure:", err)
		if sendErr := b.sendError("daemon_unavailable", "browser.sync transport failed", inboundRequestID(msg)); sendErr != nil {
			return fmt.Errorf("send daemon-unavailable error: %w", sendErr)
		}
		return fmt.Errorf("%s: %w", syncMethod, err)
	}
	return b.writeOutbound(outbound)
}
func (b *bridge) effectivePollInterval() time.Duration {
	if b.pollInterval != 0 {
		return b.pollInterval
	}
	return pollInterval
}

// pollLoop drains daemon-initiated frames while stdin is idle. Explicitly
// application-level Sync failures are transient and never terminate the
// bridge; transport/framing failures and stdout writes are fatal.
func (b *bridge) pollLoop(ctx context.Context) error {
	ticker := time.NewTicker(b.effectivePollInterval())
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
				if isApplicationSyncFailure(err) {
					_, _ = fmt.Fprintln(b.stderr, "papio-native-host: poll browser.sync application failure:", err)
					continue
				}
				_, _ = fmt.Fprintln(b.stderr, "papio-native-host: poll browser.sync transport failure:", err)
				return err
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

func inboundRequestID(msg *protocol.BrowserMessage) string {
	if msg == nil {
		return ""
	}
	raw, err := json.Marshal(msg.Payload)
	if err == nil {
		var payload struct {
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(raw, &payload) == nil && payload.RequestID != "" {
			return payload.RequestID
		}
	}
	// page_acquire has no payload request_id; its FIFO waiter can still be
	// failed by naming the originating message id.
	return msg.MsgID
}

// hostErrorFrame is a host-originated protocol error. It carries no job_id
// (error is not job-scoped) and seq 0 because the daemon owns seq numbering
// for the normal outbound stream. RequestID correlates an application
// failure with the inbound request that produced it.
type hostErrorFrame struct {
	Protocol string                `json:"protocol"`
	Type     string                `json:"type"`
	MsgID    string                `json:"msg_id"`
	Seq      int64                 `json:"seq"`
	Payload  protocol.ErrorPayload `json:"payload"`
}

// sendError writes a protocol error frame. A failed write is fatal: the
// browser may have received only a prefix, so the relay must not continue.
func (b *bridge) sendError(code, message string, requestID ...string) error {
	payload := protocol.ErrorPayload{Code: code, Message: message}
	if len(requestID) > 0 {
		payload.RequestID = requestID[0]
	}
	frame := hostErrorFrame{
		Protocol: protocol.BrowserProtocolVersion,
		Type:     protocol.MsgError,
		MsgID:    newMsgID(),
		Seq:      0,
		Payload:  payload,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		_, _ = fmt.Fprintln(b.stderr, "papio-native-host: encode error frame:", err)
		return err
	}
	if err := b.writeFrame(data); err != nil {
		_, _ = fmt.Fprintln(b.stderr, "papio-native-host: write error frame:", err)
		return err
	}
	return nil
}

// newMsgID returns a msg_id matching protocol.msgIDRE (^[A-Za-z0-9_-]{8,64}$).
func newMsgID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
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
