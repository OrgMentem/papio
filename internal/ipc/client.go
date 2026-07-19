package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// DialFunc opens a transport connection. It exists so command wiring can use a
// Unix socket while tests can use a deterministic in-memory or temporary socket.
type DialFunc func(context.Context) (net.Conn, error)

// Client sends local RPC requests over a configured connection factory.
type Client struct {
	Dial DialFunc
}

// NewSocketClient constructs a client for the daemon's local endpoint at path
// (a Unix-domain socket or a Windows named pipe, resolved by dialSocket).
func NewSocketClient(path string) *Client {
	return &Client{Dial: func(ctx context.Context) (net.Conn, error) {
		return dialSocket(ctx, path)
	}}
}

// Dial opens one connection to the daemon's local endpoint at path. It lets
// callers outside this package probe the endpoint without building a Client.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	return dialSocket(ctx, path)
}

// RemoteError is a classified error returned by the daemon.
type RemoteError struct {
	Code    string
	Message string
	Detail  *ErrorDetail
}

func (e *RemoteError) Error() string {
	if e == nil {
		return ""
	}
	message := e.Code + ": " + e.Message
	if e.Detail == nil || e.Detail.ErrorClass == "" {
		return message
	}
	detail := e.Detail.ErrorClass
	if e.Detail.ErrorHint != "" {
		detail += ": " + e.Detail.ErrorHint
	}
	return message + " [" + detail + "]"
}

// CallRaw invokes method with one raw JSON object (or null) and returns the raw
// JSON result. It closes the write side before reading, allowing the server to
// reject trailing JSON without relying on a timeout.
func (c *Client) CallRaw(ctx context.Context, id, method string, params json.RawMessage) (json.RawMessage, error) {
	if c == nil || c.Dial == nil {
		return nil, fmt.Errorf("ipc client dialer is required")
	}
	req := Request{Protocol: ProtocolVersion, ID: id, Method: method, Params: params}
	if err := ValidateRequest(req); err != nil {
		return nil, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode ipc request: %w", err)
	}
	if len(data) > MaxRequestBytes {
		return nil, ErrTooLarge
	}
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial ipc daemon: %w", err)
	}
	defer func() { _ = conn.Close() }()
	closeWriter, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		// Strict one-request framing depends on EOF after the request. Reject
		// unsupported transports before any bytes can reach a handler.
		return nil, errors.New("ipc transport does not support close-write")
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write(data); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("write ipc request: %w", err)
	}
	if err := closeWriter.CloseWrite(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("finish ipc request: %w", err)
	}
	res, err := DecodeResponse(conn)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return nil, context.DeadlineExceeded
		}
		return nil, fmt.Errorf("decode ipc response: %w", err)
	}
	if res.ID != id {
		return nil, fmt.Errorf("%w: response id mismatch", ErrInvalidRequest)
	}
	if res.Error != nil {
		return nil, &RemoteError{Code: res.Error.Code, Message: res.Error.Message, Detail: res.Error.Detail}
	}
	return res.Result, nil
}

// Call marshals params, invokes method, and unmarshals its result into result.
// The underlying envelope and method parameters remain strict JSON objects.
func (c *Client) Call(ctx context.Context, id, method string, params any, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode ipc params: %w", err)
	}
	response, err := c.CallRaw(ctx, id, method, raw)
	if err != nil {
		return err
	}
	if err := DecodeResult(response, result); err != nil {
		return fmt.Errorf("decode ipc result: %w", err)
	}
	return nil
}

// WaitForSocket waits until the daemon's local endpoint accepts a connection.
// It is used by autostart to converge concurrent command invocations without
// spawn storms.
func WaitForSocket(ctx context.Context, socketPath string, retry time.Duration) error {
	if retry <= 0 {
		retry = 25 * time.Millisecond
	}
	for {
		conn, err := NewSocketClient(socketPath).Dial(ctx)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
