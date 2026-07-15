// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package ipc implements the local, strict JSON RPC transport used by papio
// commands to communicate with the daemon.
package ipc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const (
	// ProtocolVersion identifies the incompatible IPC envelope version.
	ProtocolVersion = "papio-ipc/1"

	// MaxRequestBytes bounds one complete request envelope.
	MaxRequestBytes = 64 << 10
	// MaxResultBytes bounds a successful result or an error response envelope.
	MaxResultBytes = 1 << 20
)

var (
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	methodPattern    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,127}$`)

	// ErrTooLarge indicates a request or response that exceeds its transport cap.
	ErrTooLarge = errors.New("ipc message exceeds size limit")
	// ErrTrailingJSON indicates an envelope followed by another JSON value.
	ErrTrailingJSON = errors.New("ipc message has trailing JSON")
	// ErrInvalidRequest indicates a malformed, unsupported, or unsafe request.
	ErrInvalidRequest = errors.New("invalid ipc request")
)

// Request is one local RPC invocation. Params must be a JSON object (or null);
// concrete methods own the schema within that object.
type Request struct {
	Protocol string          `json:"protocol"`
	ID       string          `json:"id"`
	Method   string          `json:"method"`
	Params   json.RawMessage `json:"params"`
}

// Error is a machine-readable RPC failure. Its message and optional detail are
// intentionally safe to render to a local command user.
type Error struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Detail  *ErrorDetail `json:"detail,omitempty"`
}

// ErrorDetail carries bounded, pre-sanitized diagnostic metadata for a safe
// RPC failure. Handlers must never put raw upstream error text here.
type ErrorDetail struct {
	ErrorClass      string `json:"error_class,omitempty"`
	ErrorHint       string `json:"error_hint,omitempty"`
	ErrorHTTPStatus int    `json:"error_http_status,omitempty"`
}

// Response is returned for every validly decoded request.
type Response struct {
	Protocol string          `json:"protocol"`
	ID       string          `json:"id"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *Error          `json:"error,omitempty"`
}

// RPCError permits a handler to return a classified error without exposing an
// implementation error over the local transport.
type RPCError struct {
	Code    string
	Message string
	Detail  *ErrorDetail
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// ValidateRequest validates the protocol and envelope metadata before a method
// handler is allowed to inspect params.
func ValidateRequest(req Request) error {
	if req.Protocol != ProtocolVersion {
		return fmt.Errorf("%w: unsupported protocol %q", ErrInvalidRequest, req.Protocol)
	}
	if !requestIDPattern.MatchString(req.ID) {
		return fmt.Errorf("%w: bad request id", ErrInvalidRequest)
	}
	if !methodPattern.MatchString(req.Method) {
		return fmt.Errorf("%w: bad method", ErrInvalidRequest)
	}
	if len(req.Params) == 0 {
		return fmt.Errorf("%w: missing params", ErrInvalidRequest)
	}
	if len(req.Params) > MaxRequestBytes {
		return ErrTooLarge
	}
	trimmed := bytes.TrimSpace(req.Params)
	if !bytes.Equal(trimmed, []byte("null")) && (len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}') {
		return fmt.Errorf("%w: params must be an object or null", ErrInvalidRequest)
	}
	return nil
}

// DecodeRequest reads exactly one strict request envelope from r. Unknown
// fields, trailing JSON, and oversize envelopes are rejected.
func DecodeRequest(r io.Reader) (Request, error) {
	var req Request
	if err := decodeStrict(r, MaxRequestBytes, &req); err != nil {
		return Request{}, err
	}
	if err := ValidateRequest(req); err != nil {
		return Request{}, err
	}
	return req, nil
}

// DecodeResponse reads exactly one strict response envelope from r.
// DecodeParams strictly decodes a method's params object. Method handlers use it
// to reject unknown fields and a second JSON value in their concrete schema.
func DecodeParams(raw json.RawMessage, dst any) error {
	return decodeJSON(raw, dst)
}

// DecodeResult strictly decodes a successful method result into its concrete
// command-side schema.
func DecodeResult(raw json.RawMessage, dst any) error {
	return decodeJSON(raw, dst)
}

func decodeJSON(raw []byte, dst any) error {
	if err := rejectDuplicateKeys(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ErrTrailingJSON
		}
		return err
	}
	return nil
}

func DecodeResponse(r io.Reader) (Response, error) {
	var res Response
	if err := decodeStrict(r, MaxResultBytes, &res); err != nil {
		return Response{}, err
	}
	if res.Protocol != ProtocolVersion {
		return Response{}, fmt.Errorf("%w: unsupported response protocol", ErrInvalidRequest)
	}
	if !requestIDPattern.MatchString(res.ID) {
		return Response{}, fmt.Errorf("%w: bad response id", ErrInvalidRequest)
	}
	if res.Error != nil && len(res.Result) != 0 {
		return Response{}, fmt.Errorf("%w: response contains result and error", ErrInvalidRequest)
	}
	if res.Error == nil && len(res.Result) == 0 {
		return Response{}, fmt.Errorf("%w: response has neither result nor error", ErrInvalidRequest)
	}
	return res, nil
}

func decodeStrict(r io.Reader, max int, dst any) error {
	limited := &io.LimitedReader{R: r, N: int64(max) + 1}
	raw, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if len(raw) > max {
		return ErrTooLarge
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		if errors.Is(err, ErrTrailingJSON) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ErrTrailingJSON
		}
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return nil
}

// rejectDuplicateKeys rejects ambiguous JSON objects at every nesting level.
// encoding/json otherwise accepts duplicate object members and silently keeps
// only the last one, which is unsuitable for a strict RPC boundary.
func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var readValue func() error
	readValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				name, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := name.(string)
				if !ok {
					return errors.New("object member is not a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate object member %q", key)
				}
				seen[key] = struct{}{}
				if err := readValue(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := readValue(); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	if err := readValue(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return ErrTrailingJSON
		}
		return err
	}
	return nil
}

func encodeResponse(res Response) ([]byte, error) {
	data, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxResultBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}
