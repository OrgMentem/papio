package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "papio-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
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
	if err := WaitForSocket(waitCtx, socket, time.Millisecond); err != nil {
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
	err := NewUnixClient(socket).Call(context.Background(), "request_01", "jobs.get", struct {
		Job string `json:"job"`
	}{Job: "job_01"}, &result)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Job != "job_01" || result.State != "queued" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRouterRejectsUnknownMethod(t *testing.T) {
	socket, _ := startTestServer(t, Router{Methods: map[string]MethodHandler{}})
	_, err := NewUnixClient(socket).CallRaw(context.Background(), "request_01", "jobs.unknown", json.RawMessage(`{}`))
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

func TestSocketPermissionsAndRegularFileSafety(t *testing.T) {
	dir := shortTempDir(t)
	socket := filepath.Join(dir, "s")
	server := &Server{SocketPath: socket, Handler: HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) { return []byte(`{}`), nil })}
	listener, err := server.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("Stat socket: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("socket mode = %o, want 0600", got)
	}

	unsafe := filepath.Join(dir, "regular")
	if err := os.WriteFile(unsafe, []byte("do not remove"), 0600); err != nil {
		t.Fatal(err)
	}
	unsafeServer := &Server{SocketPath: unsafe, Handler: server.Handler}
	if _, err := unsafeServer.Listen(); !errors.Is(err, ErrUnsafeSocketPath) {
		t.Fatalf("Listen regular file error = %v, want ErrUnsafeSocketPath", err)
	}
	contents, err := os.ReadFile(unsafe)
	if err != nil || string(contents) != "do not remove" {
		t.Fatalf("regular file changed: %q, %v", contents, err)
	}
}

func TestClientDeadlineCancelsBlockedResponse(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
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
	_, err = NewUnixClient(socket).CallRaw(ctx, "request_01", "jobs.get", json.RawMessage(`{}`))
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
	if err := WaitForSocket(waitCtx, socket, time.Millisecond); err != nil {
		t.Fatalf("WaitForSocket: %v", err)
	}
	idle, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
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
	result, err := NewUnixClient(socket).CallRaw(context.Background(), "request_01", "jobs.get", json.RawMessage(`{}`))
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
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Handler: HandlerFunc(func(context.Context, Request) ([]byte, *RPCError) { return []byte(`{}`), nil })}
	done := make(chan error, 1)
	go func() { done <- server.ServeListener(context.Background(), listener) }()
	idle, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
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

func TestServeConnReadDeadlineReapsStalledClient(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
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

	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
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
