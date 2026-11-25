package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// shortTempDir returns a temp directory short enough that a unix socket path
// inside it fits sun_path (104 bytes on macOS, 108 on Linux). t.TempDir is
// unusable here: it embeds the test's name, so a descriptively-named test in
// this file overruns the limit and the server fails to bind. os.MkdirTemp("")
// honors TMPDIR, which keeps the path short without hardcoding /tmp — a path
// sandboxed environments refuse outright.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "papio-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func listenTestEndpoint(t *testing.T, path string) net.Listener {
	t.Helper()
	listener, cleanup, err := listenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		if err := cleanup(); err != nil {
			t.Errorf("clean up test endpoint: %v", err)
		}
	})
	return listener
}

func dialTestEndpoint(t *testing.T, path string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialSocket(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// waitForServerReady is WaitForSocket with a Serve completion side channel.
// Bind failures return from Serve before the socket exists; without selecting on
// done the poll loop waits out the full context deadline and hides the bind error.
func waitForServerReady(ctx context.Context, done <-chan error, socket string, retry time.Duration) error {
	if retry <= 0 {
		retry = time.Millisecond
	}
	for {
		conn, err := NewSocketClient(socket).Dial(ctx)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case serveErr := <-done:
			timer.Stop()
			if serveErr != nil {
				return serveErr
			}
			return fmt.Errorf("server stopped before socket %q was ready", socket)
		case <-timer.C:
		}
	}
}

func startTestServer(t *testing.T, handler Handler) (string, context.CancelFunc) {
	t.Helper()
	socket := filepath.Join(shortTempDir(t), "s")
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{SocketPath: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := waitForServerReady(waitCtx, done, socket, time.Millisecond); err != nil {
		cancel()
		t.Fatalf("server did not start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server stopped with error: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("server did not stop")
		}
	})
	return socket, cancel
}

func TestRoundTrip(t *testing.T) {
	socket, _ := startTestServer(t, HandlerFunc(func(_ context.Context, req Request) ([]byte, *RPCError) {
		if req.Method != "jobs.get" {
			return nil, &RPCError{Code: "unknown_method", Message: "unknown method"}
		}
		var params struct {
			Job string `json:"job"`
		}
		if err := DecodeParams(req.Params, &params); err != nil || params.Job == "" {
			return nil, &RPCError{Code: "invalid_params", Message: "invalid params"}
		}
		return []byte(`{"job":"` + params.Job + `","state":"queued"}`), nil
	}))
	var result struct {
		Job   string `json:"job"`
		State string `json:"state"`
	}
	err := NewSocketClient(socket).Call(context.Background(), "request_01", "jobs.get", struct {
		Job string `json:"job"`
	}{Job: "job_01"}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Job != "job_01" || result.State != "queued" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestServerRejectsInvalidHandlerResult(t *testing.T) {
	oversizedJSON := make([]byte, MaxResultBytes+1)
	oversizedJSON[0] = '"'
	copy(oversizedJSON[1:], bytes.Repeat([]byte("x"), MaxResultBytes-1))
	oversizedJSON[len(oversizedJSON)-1] = '"'

	tests := []struct {
		name   string
		result []byte
	}{
		{name: "empty", result: []byte("")},
		{name: "duplicate object member", result: []byte(`{"a":1,"a":2}`)},
		// validJSON enforces the result cap before encodeResponse sees the
		// envelope, so an oversized result reports internal, not result_too_large.
		{name: "oversized valid JSON", result: oversizedJSON},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			socket, _ := startTestServer(t, HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
				return test.result, nil
			}))
			_, err := NewSocketClient(socket).CallRaw(context.Background(), "request_01", "jobs.get", json.RawMessage(`{}`))
			var remote *RemoteError
			if !errors.As(err, &remote) {
				t.Fatalf("CallRaw error = %v, want decodable RemoteError with code internal", err)
			}
			if remote.Code != "internal" {
				t.Fatalf("CallRaw RemoteError code = %q, want internal", remote.Code)
			}
		})
	}
}

// TestServerEnforcesResultCapAtTheExactBoundary pins both sides of the cap. A
// single over-cap case cannot distinguish `>` from `>=`: an implementation that
// rejected an exactly-at-cap response would still pass it. The two cases differ
// by ONE byte of result payload, so they straddle the comparison.
func TestServerEnforcesResultCapAtTheExactBoundary(t *testing.T) {
	const requestID = "request_01"
	emptyStringResult := json.RawMessage(`""`)
	minimalEnvelope, err := json.Marshal(Response{
		Protocol: ProtocolVersion,
		ID:       requestID,
		Result:   emptyStringResult,
	})
	if err != nil {
		t.Fatalf("marshal response envelope: %v", err)
	}
	envelopeOverhead := len(minimalEnvelope) - len(emptyStringResult)

	// jsonStringOfEnvelopeSize builds a JSON string result whose whole response
	// envelope is exactly `total` bytes.
	jsonStringOfEnvelopeSize := func(total int) json.RawMessage {
		size := total - envelopeOverhead
		result := make([]byte, size)
		result[0] = '"'
		copy(result[1:], bytes.Repeat([]byte("x"), size-2))
		result[len(result)-1] = '"'
		return result
	}

	for _, test := range []struct {
		name         string
		envelopeSize int
		wantCode     string
	}{
		{name: "exactly_at_cap_is_served", envelopeSize: MaxResultBytes, wantCode: ""},
		{name: "one_byte_over_cap_is_refused", envelopeSize: MaxResultBytes + 1, wantCode: "result_too_large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := jsonStringOfEnvelopeSize(test.envelopeSize)
			socket, _ := startTestServer(t, HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
				return result, nil
			}))
			raw, err := NewSocketClient(socket).CallRaw(context.Background(), requestID, "jobs.get", json.RawMessage(`{}`))
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("CallRaw at exactly MaxResultBytes = %v, want the response served", err)
				}
				if len(raw) != len(result) {
					t.Fatalf("served result = %d bytes, want %d", len(raw), len(result))
				}
				return
			}
			var remote *RemoteError
			if !errors.As(err, &remote) {
				t.Fatalf("CallRaw error = %v, want decodable RemoteError with code %s", err, test.wantCode)
			}
			if remote.Code != test.wantCode {
				t.Fatalf("CallRaw RemoteError code = %q, want %s", remote.Code, test.wantCode)
			}
		})
	}
}

func TestConcurrentCallsDoNotBlockEachOther(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	socket, _ := startTestServer(t, HandlerFunc(func(_ context.Context, req Request) ([]byte, *RPCError) {
		if req.Method == "slow.call" {
			close(started)
			<-release
		}
		return []byte(`{}`), nil
	}))

	slowDone := make(chan error, 1)
	go func() {
		var result map[string]any
		slowDone <- NewSocketClient(socket).Call(context.Background(), "slow_req_01", "slow.call", struct{}{}, &result)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow call did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var result map[string]any
	if err := NewSocketClient(socket).Call(ctx, "fast_req_01", "fast.call", struct{}{}, &result); err != nil {
		t.Fatalf("fast call blocked behind slow call: %v", err)
	}

	close(release)
	select {
	case err := <-slowDone:
		if err != nil {
			t.Fatalf("slow call: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow call did not finish")
	}
}

func TestRouterRejectsUnknownMethod(t *testing.T) {
	socket, _ := startTestServer(t, Router{Methods: map[string]MethodHandler{}})
	_, err := NewSocketClient(socket).CallRaw(context.Background(), "request_01", "jobs.unknown", json.RawMessage(`{}`))
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "unknown_method" {
		t.Fatalf("CallRaw error = %v, want unknown_method", err)
	}
}

func TestStrictRequestValidation(t *testing.T) {
	tests := []struct {
		name string
		data string
		want error
	}{
		{"unknown field", `{"protocol":"papio-ipc/1","id":"request_01","method":"jobs.get","params":{},"extra":true}`, ErrInvalidRequest},
		{"unknown version", `{"protocol":"papio-ipc/9","id":"request_01","method":"jobs.get","params":{}}`, ErrInvalidRequest},
		{"invalid method", `{"protocol":"papio-ipc/1","id":"request_01","method":"jobs get","params":{}}`, ErrInvalidRequest},
		{"trailing json", `{"protocol":"papio-ipc/1","id":"request_01","method":"jobs.get","params":{}} {}`, ErrTrailingJSON},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeRequest(strings.NewReader(test.data))
			if !errors.Is(err, test.want) {
				t.Fatalf("DecodeRequest error = %v, want %v", err, test.want)
			}
		})
	}
	oversized := `{"protocol":"papio-ipc/1","id":"request_01","method":"jobs.get","params":{"value":"` + strings.Repeat("x", MaxRequestBytes) + `"}}`
	_, err := DecodeRequest(strings.NewReader(oversized))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized request error = %v, want ErrTooLarge", err)
	}
}

func TestClientDeadlineCancelsBlockedResponse(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s")
	listener := listenTestEndpoint(t, socket)
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		close(accepted)
		_, _ = DecodeRequest(conn)
		time.Sleep(time.Second)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := NewSocketClient(socket).CallRaw(ctx, "request_01", "jobs.get", json.RawMessage(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CallRaw error = %v, want deadline exceeded", err)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("test listener did not receive request")
	}
}

func TestStrictRequestValidationRejectsDuplicateKeys(t *testing.T) {
	data := `{"protocol":"papio-ipc/1","id":"request_01","method":"jobs.get","method":"jobs.cancel","params":{}}`
	if _, err := DecodeRequest(strings.NewReader(data)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("DecodeRequest error = %v, want ErrInvalidRequest", err)
	}
	var params struct {
		ID string `json:"id"`
	}
	if err := DecodeParams(json.RawMessage(`{"id":"first","id":"second"}`), &params); err == nil {
		t.Fatal("DecodeParams accepted duplicate member")
	}
}

func TestServerCancellationClosesIdleConnection(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &Server{SocketPath: socket, Handler: HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
		return []byte(`{}`), nil
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := waitForServerReady(waitCtx, done, socket, time.Millisecond); err != nil {
		t.Fatalf("waitForServerReady: %v", err)
	}
	idle := dialTestEndpoint(t, socket)
	defer func() { _ = idle.Close() }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop after cancellation with an idle client")
	}
}

func TestRoundTripAllowsLargeJSONNumber(t *testing.T) {
	socket, _ := startTestServer(t, HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
		return []byte(`{"value":1e1000}`), nil
	}))
	result, err := NewSocketClient(socket).CallRaw(context.Background(), "request_01", "jobs.get", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallRaw: %v", err)
	}
	if string(result) != `{"value":1e1000}` {
		t.Fatalf("result = %s", result)
	}
}

type closeWriteCancellationConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *closeWriteCancellationConn) CloseWrite() error {
	c.cancel()
	return errors.New("close write interrupted")
}

func TestClientReturnsCancellationFromCloseWrite(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	go func() {
		buf := make([]byte, 1024)
		_, _ = serverConn.Read(buf)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &Client{Dial: func(context.Context) (net.Conn, error) {
		return &closeWriteCancellationConn{Conn: clientConn, cancel: cancel}, nil
	}}
	_, err := client.CallRaw(ctx, "request_01", "jobs.get", json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CallRaw error = %v, want context.Canceled", err)
	}
}

// TestClientCancellationPropagatesToHandler pins papio-1af1e6d: cancelling
// the caller's context closes the connection, and the server cancels the
// handler that is still running instead of letting it mutate state with no
// waiter.
func TestClientCancellationPropagatesToHandler(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("disconnect detection needs the unix poll watcher; the Windows watcher is a no-op")
	}
	entered := make(chan struct{})
	handlerDone := make(chan struct{})
	socket, _ := startTestServer(t, HandlerFunc(func(ctx context.Context, _ Request) ([]byte, *RPCError) {
		close(entered)
		<-ctx.Done()
		close(handlerDone)
		return nil, &RPCError{Code: "cancelled", Message: "cancelled"}
	}))
	callCtx, cancelCall := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, err := NewSocketClient(socket).CallRaw(callCtx, "request_01", "jobs.get", json.RawMessage(`{}`))
		callDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	cancelCall()
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("handler context was not cancelled after client cancellation")
	}
	select {
	case err := <-callDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CallRaw error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CallRaw did not return after cancellation")
	}
}

type noCloseWriteConn struct{ net.Conn }

func TestClientRejectsTransportWithoutCloseWriteBeforeRequest(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	decoded := make(chan struct{}, 1)
	go func() {
		if _, err := DecodeRequest(serverConn); err == nil {
			decoded <- struct{}{}
		}
	}()
	client := &Client{Dial: func(context.Context) (net.Conn, error) {
		return noCloseWriteConn{Conn: clientConn}, nil
	}}
	_, err := client.CallRaw(context.Background(), "request_01", "jobs.get", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "close-write") {
		t.Fatalf("CallRaw error = %v, want close-write error", err)
	}
	select {
	case <-decoded:
		t.Fatal("request reached peer without strict EOF framing")
	case <-time.After(25 * time.Millisecond):
	}
}

func TestServeListenerClosesIdleClientsWhenListenerCloses(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s")
	listener := listenTestEndpoint(t, socket)
	server := &Server{Handler: HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) { return []byte(`{}`), nil })}
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(context.Background(), listener) }()
	idle := dialTestEndpoint(t, socket)
	defer func() { _ = idle.Close() }()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeListener: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServeListener did not stop after listener close with idle client")
	}
}

type acceptResult struct {
	conn net.Conn
	err  error
}

type scriptedListener struct {
	mu      sync.Mutex
	results []acceptResult
	accepts chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newScriptedListener(results ...acceptResult) *scriptedListener {
	return &scriptedListener{
		results: results,
		accepts: make(chan struct{}, len(results)+1),
		closed:  make(chan struct{}),
	}
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if len(l.results) > 0 {
		result := l.results[0]
		l.results = l.results[1:]
		l.mu.Unlock()
		l.accepts <- struct{}{}
		return result.conn, result.err
	}
	l.mu.Unlock()
	l.accepts <- struct{}{}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *scriptedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *scriptedListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "scripted-listener", Net: "unix"}
}

func TestServeListenerRetriesTransientAcceptErrors(t *testing.T) {
	firstServer, firstClient := net.Pipe()
	t.Cleanup(func() { _ = firstClient.Close() })
	secondServer, secondClient := net.Pipe()
	t.Cleanup(func() { _ = secondClient.Close() })
	listener := newScriptedListener(
		acceptResult{conn: firstServer},
		acceptResult{err: fmt.Errorf("fd pressure: %w", syscall.EMFILE)},
		acceptResult{conn: secondServer},
	)
	server := &Server{Handler: HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
		return []byte(`{}`), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ServeListener: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("ServeListener did not stop after cancellation")
		}
	})

	for range 4 {
		select {
		case <-listener.accepts:
		case <-time.After(time.Second):
			t.Fatal("ServeListener did not continue accepting after transient error")
		}
	}

	if err := firstClient.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := firstClient.Read(make([]byte, 1))
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("first client read error = %v, want timeout while connection remains open", err)
	}
}

func TestServeListenerReturnsPermanentAcceptError(t *testing.T) {
	permanent := errors.New("listener failed")
	listener := newScriptedListener(acceptResult{err: permanent})
	server := &Server{Handler: HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
		return []byte(`{}`), nil
	})}

	err := server.ServeListener(context.Background(), listener)
	if !errors.Is(err, permanent) {
		t.Fatalf("ServeListener error = %v, want wrapped %v", err, permanent)
	}
}

// gatedListener models a kernel that finished an accept just before the
// listener closed: Accept returns only what the test hands it, even after
// Close, so a test can deliver a connection at an exact point of shutdown.
type gatedListener struct {
	accepted chan acceptResult
	entered  chan struct{}
	closed   chan struct{}
	once     sync.Once
}

func newGatedListener() *gatedListener {
	return &gatedListener{
		accepted: make(chan acceptResult),
		entered:  make(chan struct{}, 16),
		closed:   make(chan struct{}),
	}
}

func (l *gatedListener) Accept() (net.Conn, error) {
	l.entered <- struct{}{}
	result := <-l.accepted
	return result.conn, result.err
}

func (l *gatedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *gatedListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "gated-listener", Net: "unix"}
}

func (l *gatedListener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

// requireClosedByServer fails unless the server side of peer's pipe is closed,
// which a net.Pipe reader sees as EOF.
func requireClosedByServer(t *testing.T, name string, peer net.Conn) {
	t.Helper()
	// net.Pipe refuses SetReadDeadline once the remote end is closed, so bound
	// the read with a timer instead.
	result := make(chan error, 1)
	go func() {
		_, err := peer.Read(make([]byte, 1))
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("%s: read error = %v, want EOF from a server-side close", name, err)
		}
	case <-time.After(time.Second):
		_ = peer.Close()
		t.Fatalf("%s: server did not close the connection", name)
	}
}

func waitServeListener(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeListener: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("ServeListener did not return %s", what)
	}
}

func idleHandler() Handler {
	return HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) { return []byte(`{}`), nil })
}

// TestServeListenerClosesListenerWhenCancelledBeforeAccept pins the return path
// that finds the context already cancelled once Accept hands back a connection.
// That path must close the listener itself before returning, not leave it to a
// helper goroutine that has had no chance to run: it must also close the
// connection it was just handed.
func TestServeListenerClosesListenerWhenCancelledBeforeAccept(t *testing.T) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prev) })
	server := &Server{Handler: idleHandler()}
	for i := range 300 {
		accepted, peer := net.Pipe()
		listener := newGatedListener()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		go func() { listener.accepted <- acceptResult{conn: accepted} }()
		if err := server.ServeListener(ctx, listener); err != nil {
			t.Fatalf("iteration %d: ServeListener: %v", i, err)
		}
		if !listener.isClosed() {
			t.Fatalf("iteration %d: ServeListener returned with its listener still open", i)
		}
		requireClosedByServer(t, "accepted connection", peer)
		_ = peer.Close()
	}
}

// TestServeListenerCancelledBetweenAcceptsClosesEveryConnection cancels the
// server while one connection is active and delivers a second connection that
// the kernel accepted before the listener closed. Both must be closed and
// ServeListener must return.
func TestServeListenerCancelledBetweenAcceptsClosesEveryConnection(t *testing.T) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prev) })
	active, activePeer := net.Pipe()
	late, latePeer := net.Pipe()
	t.Cleanup(func() { _ = activePeer.Close(); _ = latePeer.Close() })
	listener := newGatedListener()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	server := &Server{Handler: idleHandler()}
	go func() { done <- server.ServeListener(ctx, listener) }()

	<-listener.entered
	listener.accepted <- acceptResult{conn: active}
	<-listener.entered // the loop registered the first connection before accepting again
	cancel()
	select {
	case <-listener.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not close the listener")
	}
	listener.accepted <- acceptResult{conn: late}

	waitServeListener(t, done, "after cancellation with an active connection")
	requireClosedByServer(t, "active connection", activePeer)
	requireClosedByServer(t, "late connection", latePeer)
}

// TestServeListenerCancelledDuringAcceptBackoffClosesActiveConnection cancels
// while the loop sleeps between transient accept failures.
func TestServeListenerCancelledDuringAcceptBackoffClosesActiveConnection(t *testing.T) {
	prev := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prev) })
	active, activePeer := net.Pipe()
	t.Cleanup(func() { _ = activePeer.Close() })
	transient := acceptResult{err: fmt.Errorf("fd pressure: %w", syscall.EMFILE)}
	listener := newScriptedListener(acceptResult{conn: active}, transient, transient, transient, transient)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	server := &Server{Handler: idleHandler()}
	go func() { done <- server.ServeListener(ctx, listener) }()

	// The fifth accept returns the fourth failure; the loop then sleeps 40ms.
	for range 5 {
		select {
		case <-listener.accepts:
		case <-time.After(2 * time.Second):
			t.Fatal("ServeListener stopped accepting before the backoff")
		}
	}
	cancel()
	waitServeListener(t, done, "after cancellation during accept backoff")
	requireClosedByServer(t, "active connection", activePeer)
}

// TestServeListenerCancelledWithRequestInFlight cancels while a handler runs:
// the handler's context must end, its connection must close, and
// ServeListener must return once the handler does.
func TestServeListenerCancelledWithRequestInFlight(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s")
	listener := listenTestEndpoint(t, socket)
	entered := make(chan struct{})
	handlerCtxDone := make(chan struct{})
	server := &Server{Handler: HandlerFunc(func(ctx context.Context, _ Request) ([]byte, *RPCError) {
		close(entered)
		<-ctx.Done()
		close(handlerCtxDone)
		return nil, &RPCError{Code: "cancelled", Message: "server stopping"}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(ctx, listener) }()

	client := dialTestEndpoint(t, socket)
	request, err := json.Marshal(Request{Protocol: ProtocolVersion, ID: "request_01", Method: "jobs.get", Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(request); err != nil {
		t.Fatal(err)
	}
	closeWriter, ok := client.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("client connection does not support CloseWrite")
	}
	if err := closeWriter.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	waitServeListener(t, done, "after cancellation with a request in flight")
	select {
	case <-handlerCtxDone:
	default:
		t.Fatal("ServeListener returned before the handler observed cancellation")
	}
}

func TestServeConnReadDeadlineReapsStalledClient(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s")
	listener := listenTestEndpoint(t, socket)
	server := &Server{
		Handler:     HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) { return []byte(`{}`), nil }),
		IdleTimeout: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ServeListener: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("ServeListener did not stop after cancellation")
		}
	})

	conn := dialTestEndpoint(t, socket)
	defer func() { _ = conn.Close() }()
	// Send a partial request prefix and never CloseWrite: framing relies on
	// EOF, so without the server's read deadline this connection would stall
	// the serveConn goroutine indefinitely.
	if _, err := conn.Write([]byte(`{"protocol":`)); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	// Bound comfortably above IdleTimeout but well below any hang: the server
	// must close the connection once its read deadline fires.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err == nil {
		t.Fatalf("read returned %d bytes with no error; server did not reap stalled client", n)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("client read hit its own 2s deadline (%v); server never closed the stalled connection", err)
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read error = %v, want server-side close (EOF or reset)", err)
	}
}

// TestServerLogsPartialFramesButNotEmptyProbes pins the one distinction the
// decode-failure log makes. A peer that dials and closes without a byte is the
// autostart liveness probe (daemon.probeSocket), issued by every CLI invocation;
// logging it buried real decode failures under thousands of identical lines. A
// peer that sends a partial frame is a broken client and must still be logged.
func TestServerLogsPartialFramesButNotEmptyProbes(t *testing.T) {
	logs := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	socket, _ := startTestServer(t, HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) {
		return json.RawMessage(`{}`), nil
	}))
	dial := func() net.Conn { return dialTestEndpoint(t, socket) }
	// Wait until the server has observed the close: it answers a real request
	// on a later connection, and serveConn goroutines run in accept order.
	settle := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := NewSocketClient(socket).CallRaw(ctx, "probe_01", "jobs.get", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("settling call: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := dial().Close(); err != nil {
		t.Fatal(err)
	}
	settle()
	if got := logs.String(); strings.Contains(got, "decode request") {
		t.Fatalf("an empty dial-and-close was logged as a decode failure:\n%s", got)
	}

	partial := dial()
	if _, err := partial.Write([]byte(`{"protocol":`)); err != nil {
		t.Fatal(err)
	}
	// Flush the partial frame before disconnecting. A named pipe can discard
	// unread buffered bytes on Close; CloseWrite delivers the framing EOF.
	closeWriter, ok := partial.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("test endpoint does not support CloseWrite")
	}
	if err := closeWriter.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}
	settle()
	if got := logs.String(); !strings.Contains(got, "ipc: decode request: invalid ipc request") {
		t.Fatalf("a partial frame was not logged as a decode failure:\n%s", got)
	}
}

// syncBuffer is a log sink the server goroutines write while the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRejectDuplicateKeysCaseVariants(t *testing.T) {
	for _, raw := range []string{
		`{"method":"a","METHOD":"b"}`,
		`{"params":{"url":"a","URL":"b"}}`,
		`[{"Id":1,"iD":2}]`,
	} {
		if err := rejectDuplicateKeys([]byte(raw)); err == nil {
			t.Errorf("%s: expected duplicate error", raw)
		}
	}
	if err := rejectDuplicateKeys([]byte(`{"a":{"x":1},"b":{"X":2}}`)); err != nil {
		t.Errorf("distinct objects rejected: %v", err)
	}
}
