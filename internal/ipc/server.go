package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
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

// Server exposes one-request-per-connection RPC over the daemon's local
// endpoint (a Unix-domain socket or a Windows named pipe).
// Calls are serialized before entering Handler so handlers that perform a
// sequence of writes retain the daemon's single-writer ordering.
type Server struct {
	SocketPath string
	Handler    Handler
	// IdleTimeout bounds reading one request and writing its response on an
	// accepted connection. Zero uses DefaultConnIdleTimeout.
	IdleTimeout time.Duration

	// cleanup removes the endpoint this server created; it is set by Listen and
	// invoked by Serve on exit. It is a no-op transport where nothing persists.
	cleanup func() error

	callMu sync.Mutex
}

// Listen creates the daemon's local endpoint with owner-only permissions,
// delegating the platform specifics (Unix-domain socket or Windows named pipe)
// to listenSocket. A stale endpoint is reclaimed only when no daemon answers.
func (s *Server) Listen() (net.Listener, error) {
	if s.SocketPath == "" {
		return nil, fmt.Errorf("ipc socket path is required")
	}
	if s.Handler == nil {
		return nil, fmt.Errorf("ipc handler is required")
	}
	listener, cleanup, err := listenSocket(s.SocketPath)
	if err != nil {
		return nil, err
	}
	s.cleanup = cleanup
	return listener, nil
}

// Serve listens until ctx is cancelled. It removes only the endpoint that it
// created when it exits.
func (s *Server) Serve(ctx context.Context) error {
	listener, err := s.Listen()
	if err != nil {
		return err
	}
	if s.cleanup != nil {
		defer func() { _ = s.cleanup() }()
	}
	return s.ServeListener(ctx, listener)
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
