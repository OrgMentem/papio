// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package agentcredential

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync/atomic"

	"github.com/zalando/go-keyring"
)

// HelperArgument selects helper mode in the papio executable. It is not
// advertised by the CLI. The executable's dispatcher must call RunHelper when
// it sees it, before normal startup.
const HelperArgument = "--papio-credential-helper"

// maxHelperBytes bounds a helper request or response. The largest value is a
// 2048-byte credential record whose JSON escaping can grow sixfold, so this
// leaves room for the envelope.
const maxHelperBytes = 16384

// helperBinary is the papio executable that serves OS credential calls in a
// separate process. Unset, a Store calls the OS in this process.
var helperBinary atomic.Pointer[string]

// UseHelperProcess makes every Store created afterwards, and every
// internal/credential Store, run each OS credential call in its own helper
// process, started from binary with HelperArgument. One executable serves both
// packages, so the OS credential store sees one application identity.
//
// The OS credential library has no cancellation, so an in-process call that
// never returns holds the gate for the life of the process. A helper process
// can be killed at the deadline, which frees the gate and the Store keeps
// working once the OS store recovers. If the helper cannot start (for example
// the binary was replaced on disk), no OS call was dispatched and the call
// returns ErrUnavailable; it never falls back to an in-process OS call. A Store
// created before UseHelperProcess, and one built by a test, calls the OS in
// process. Call it once at process start, from the executable that dispatches
// HelperArgument to RunHelper.
func UseHelperProcess(binary string) { helperBinary.Store(&binary) }

// HelperBinary returns the executable set by UseHelperProcess, if any.
func HelperBinary() (string, bool) {
	if b := helperBinary.Load(); b != nil && *b != "" {
		return *b, true
	}
	return "", false
}

// Namespace lets another package serve its own OS credential service through
// this helper. The helper refuses any service or account it does not validate.
type Namespace struct {
	Service      string
	ValidAccount func(string) bool
	ValidValue   func(string) bool
}

type helperRequest struct {
	Op      string `json:"op"`
	Service string `json:"service"`
	Account string `json:"account"`
	// Key is a byte slice so JSON carries it as base64 and invalid UTF-8
	// survives the round trip unchanged.
	Key []byte `json:"key,omitempty"`
}

type helperResponse struct {
	Status string `json:"status"`
	Key    []byte `json:"key,omitempty"`
}

const (
	// OpLoad, OpSave and OpDelete are the helper operations.
	OpLoad   = "load"
	OpSave   = "save"
	OpDelete = "delete"

	statusOK          = "ok"
	statusNotFound    = "not_found"
	statusUnavailable = "unavailable"
)

// ErrHelperNotStarted means the helper process could not be started, so no OS
// call was dispatched. A binary replaced on disk since this process began is the
// usual cause.
var ErrHelperNotStarted = errors.New("credential helper did not start")

// CallHelper performs one OS credential call for service in a helper process
// started from binary. It returns only ErrNotFound, ErrUnavailable,
// ErrHelperNotStarted or the context error, and returns after the process has
// exited. Cancelling ctx kills the process and its children.
func CallHelper(ctx context.Context, binary, service, op, account, value string) (string, error) {
	return runHelperProcess(ctx, binary, helperRequest{Op: op, Service: service, Account: account, Key: []byte(value)})
}

// runHelperProcess performs one OS credential call in a helper process and
// returns only sentinel errors, or ErrHelperNotStarted before any dispatch. It returns after the process has exited, so a
// caller that holds the gate may release it on return. Cancelling ctx kills
// the process and its children.
func runHelperProcess(ctx context.Context, binary string, req helperRequest) (string, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return "", ErrUnavailable
	}
	cmd := exec.CommandContext(ctx, binary, HelperArgument)
	configureHelperProcess(cmd)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout bytes.Buffer
	cmd.Stdout = &cappedWriter{buf: &stdout, left: maxHelperBytes}
	if err := cmd.Start(); err != nil {
		return "", ErrHelperNotStarted
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrUnavailable
	}
	var resp helperResponse
	dec := json.NewDecoder(&stdout)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return "", ErrUnavailable
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return "", ErrUnavailable
	}
	switch resp.Status {
	case statusOK:
		return string(resp.Key), nil
	case statusNotFound:
		return "", ErrNotFound
	default:
		return "", ErrUnavailable
	}
}

// cappedWriter keeps a helper's output bounded; a write past the limit fails,
// which ends the helper with a broken pipe.
type cappedWriter struct {
	buf  *bytes.Buffer
	left int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errors.New("helper output too large")
	}
	w.left -= len(p)
	return w.buf.Write(p)
}

// RunHelper serves one helper request from in and writes one response to out,
// calling the OS credential store in this process. It is the worker side of
// UseHelperProcess.
func RunHelper(in io.Reader, out io.Writer, extra ...Namespace) error {
	return serveHelper(in, out, backend{load: keyring.Get, save: keyring.Set, delete: keyring.Delete}, extra...)
}

func serveHelper(in io.Reader, out io.Writer, b backend, extra ...Namespace) error {
	raw, err := io.ReadAll(io.LimitReader(in, maxHelperBytes+1))
	if err != nil {
		return errors.New("read credential helper request")
	}
	if len(raw) > maxHelperBytes {
		return errors.New("credential helper request is too large")
	}
	var req helperRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return errors.New("decode credential helper request")
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return errors.New("credential helper request contains multiple values")
	}
	validAccount, validValue := validProfile, func(v string) bool { return ValidateKey(v) == nil }
	if req.Service != serviceName {
		known := false
		for _, ns := range extra {
			if ns.Service == req.Service && ns.ValidAccount != nil && ns.ValidValue != nil {
				validAccount, validValue, known = ns.ValidAccount, ns.ValidValue, true
			}
		}
		if !known {
			return errors.New("credential helper request names another credential")
		}
	}
	if !validAccount(req.Account) {
		return errors.New("credential helper request names another credential")
	}
	var resp helperResponse
	switch req.Op {
	case OpLoad:
		var loaded string
		loaded, err = b.load(req.Service, req.Account)
		resp.Key = []byte(loaded)
	case OpSave:
		if !validValue(string(req.Key)) {
			return errors.New("credential helper request has an invalid key")
		}
		err = b.save(req.Service, req.Account, string(req.Key))
	case OpDelete:
		err = b.delete(req.Service, req.Account)
	default:
		return errors.New("credential helper request has an unknown operation")
	}
	switch err = sanitize(err); {
	case err == nil:
		resp.Status = statusOK
	case errors.Is(err, ErrNotFound):
		resp.Status, resp.Key = statusNotFound, nil
	default:
		resp.Status, resp.Key = statusUnavailable, nil
	}
	return json.NewEncoder(out).Encode(resp)
}
