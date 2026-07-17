package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrSocketInUse means another daemon is already listening on the socket.
	ErrSocketInUse = errors.New("ipc socket already in use")
	// ErrUnsafeSocketPath means the socket path points at a non-socket file.
	ErrUnsafeSocketPath = errors.New("ipc socket path is not a socket")
)

// Handler dispatches one validated request. Result must be valid JSON and must
// fit MaxResultBytes. Returning an RPCError exposes its safe code and message.
type Handler interface {
	Handle(context.Context, Request) (jsonResult []byte, rpcErr *RPCError)
}

// HandlerFunc adapts a function into a Handler.
type HandlerFunc func(context.Context, Request) ([]byte, *RPCError)

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, req Request) ([]byte, *RPCError) {
	return f(ctx, req)
}

// MethodHandler handles the concrete params for one named method.
type MethodHandler func(context.Context, json.RawMessage) ([]byte, *RPCError)

// Router is the small explicit method registry used by daemon wiring. Unknown
// methods are rejected without invoking application code.
type Router struct {
	Methods map[string]MethodHandler
}

// Handle implements Handler.
func (r Router) Handle(ctx context.Context, req Request) ([]byte, *RPCError) {
	handler := r.Methods[req.Method]
	if handler == nil {
		return nil, &RPCError{Code: "unknown_method", Message: "unknown method"}
	}
	return handler(ctx, req.Params)
}

// Server exposes one-request-per-connection RPC over a Unix-domain socket.
// Calls are serialized before entering Handler so handlers that perform a
// sequence of writes retain the daemon's single-writer ordering.
type Server struct {
	SocketPath string
	Handler    Handler
	// IdleTimeout bounds reading one request and writing its response on an
	// accepted connection. Zero uses DefaultConnIdleTimeout.
	IdleTimeout time.Duration

	callMu sync.Mutex
}

// Listen creates Server.SocketPath with owner-only permissions. A regular file
// is never removed. A stale Unix socket is removed only after a failed dial;
// an active listener is left untouched.
func (s *Server) Listen() (*net.UnixListener, error) {
	if s.SocketPath == "" {
		return nil, fmt.Errorf("ipc socket path is required")
	}
	if s.Handler == nil {
		return nil, fmt.Errorf("ipc handler is required")
	}
	if err := os.MkdirAll(filepath.Dir(s.SocketPath), 0700); err != nil {
		return nil, fmt.Errorf("create ipc directory: %w", err)
	}
	if err := prepareSocket(s.SocketPath); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.SocketPath, Net: "unix"})
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, ErrSocketInUse
		}
		return nil, fmt.Errorf("listen ipc socket: %w", err)
	}
	// net.UnixListener otherwise unlinks its path on Close, which could remove
	// a replacement file created after the daemon started.
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(s.SocketPath, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.SocketPath)
		return nil, fmt.Errorf("restrict ipc socket permissions: %w", err)
	}
	return listener, nil
}

func prepareSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect ipc socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return ErrUnsafeSocketPath
	}

	conn, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return ErrSocketInUse
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) && (errors.Is(netErr.Err, syscall.ECONNREFUSED) || errors.Is(netErr.Err, syscall.ENOENT)) {
		if err := removeSocketIfSame(path, info); err != nil {
			return fmt.Errorf("remove stale ipc socket: %w", err)
		}
		return nil
	}
	return fmt.Errorf("probe existing ipc socket: %w", err)
}

// Serve listens until ctx is cancelled. It removes only the socket that it
// created when it exits.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := s.Listen()
	if err != nil {
		return err
	}
	info, err := os.Lstat(s.SocketPath)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("inspect created ipc socket: %w", err)
	}
	defer func() { _ = removeSocketIfSame(s.SocketPath, info) }()
	return s.ServeListener(ctx, listener)
}

// removeSocketIfSame removes path only if it is still the socket identified by
// expected. It prevents shutdown cleanup from deleting a replacement file.
func removeSocketIfSame(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSocket == 0 || !os.SameFile(expected, current) {
		return nil
	}
	return os.Remove(path)
}

// ServeListener serves an already-created listener, which makes deterministic
// tests possible without opening a filesystem socket.
func (s *Server) ServeListener(ctx context.Context, listener net.Listener) error {
	if s.Handler == nil {
		return fmt.Errorf("ipc handler is required")
	}
	var conns sync.WaitGroup
	var connMu sync.Mutex
	active := make(map[net.Conn]struct{})
	closeAll := func() {
		_ = listener.Close()
		connMu.Lock()
		defer connMu.Unlock()
		for conn := range active {
			_ = conn.Close()
		}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeAll()
		case <-done:
		}
	}()
	defer close(done)
	defer conns.Wait()

	for {
		conn, err := listener.Accept()
		if err != nil {
			closeAll()
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept ipc connection: %w", err)
		}
		connMu.Lock()
		if ctx.Err() != nil {
			connMu.Unlock()
			_ = conn.Close()
			return nil
		}
		active[conn] = struct{}{}
		connMu.Unlock()
		conns.Add(1)
		go func(conn net.Conn) {
			defer conns.Done()
			defer func() { _ = conn.Close() }()
			defer func() {
				connMu.Lock()
				delete(active, conn)
				connMu.Unlock()
			}()
			s.serveConn(ctx, conn)
		}(conn)
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	timeout := s.IdleTimeout
	if timeout <= 0 {
		timeout = DefaultConnIdleTimeout
	}
	// Framing relies on EOF after CloseWrite, so a half-open peer that opens
	// the socket and sends a partial frame (or nothing) without closing would
	// otherwise block this goroutine and hold its fd until daemon shutdown.
	// Bound the request read so such a client is reaped instead.
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	req, err := DecodeRequest(conn)
	if err != nil {
		return
	}
	s.callMu.Lock()
	result, rpcErr := s.Handler.Handle(ctx, req)
	s.callMu.Unlock()

	res := Response{Protocol: ProtocolVersion, ID: req.ID}
	if rpcErr != nil {
		res.Error = &Error{Code: rpcErr.Code, Message: rpcErr.Message, Detail: rpcErr.Detail}
	} else if len(result) == 0 || !validJSON(result) {
		res.Error = &Error{Code: "internal", Message: "handler returned an invalid result"}
	} else {
		res.Result = result
	}
	data, err := encodeResponse(res)
	if err != nil {
		fallback := Response{Protocol: ProtocolVersion, ID: req.ID, Error: &Error{Code: "result_too_large", Message: "result exceeds transport limit"}}
		data, err = encodeResponse(fallback)
		if err != nil {
			return
		}
	}
	// Bound the response write so a client that never reads cannot stall this
	// goroutine indefinitely.
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	_, _ = conn.Write(data)
}

func validJSON(raw []byte) bool {
	return len(raw) <= MaxResultBytes && rejectDuplicateKeys(raw) == nil && json.Valid(raw)
}
