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

// maxHelperBytes bounds a helper request or response. A valid key is at most
// 1024 bytes, so this leaves room for the envelope.
const maxHelperBytes = 4096

// helperBinary is the papio executable that serves OS credential calls in a
// separate process. Unset, a Store calls the OS in this process.
var helperBinary atomic.Pointer[string]

// UseHelperProcess makes every Store created afterwards run each OS credential
// call in its own helper process, started from binary with HelperArgument.
//
// The OS credential library has no cancellation, so an in-process call that
// never returns holds this package's gate for the life of the process. A
// helper process can be killed at the deadline, which frees the gate and the
// Store keeps working once the OS store recovers. Call it once at process
// start, from the executable that dispatches HelperArgument to RunHelper.
func UseHelperProcess(binary string) { helperBinary.Store(&binary) }

type helperRequest struct {
	Op      string `json:"op"`
	Service string `json:"service"`
	Account string `json:"account"`
	Key     string `json:"key,omitempty"`
}

type helperResponse struct {
	Status string `json:"status"`
	Key    string `json:"key,omitempty"`
}

const (
	opLoad   = "load"
	opSave   = "save"
	opDelete = "delete"

	statusOK          = "ok"
	statusNotFound    = "not_found"
	statusUnavailable = "unavailable"
)

// errHelperNotStarted means the helper process could not be started, so no OS
// call was dispatched. A binary replaced on disk since this process began is the
// usual cause.
var errHelperNotStarted = errors.New("credential helper did not start")

// runHelperProcess performs one OS credential call in a helper process and
// returns only sentinel errors, or errHelperNotStarted before any dispatch. It returns after the process has exited, so a
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
		return "", errHelperNotStarted
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
		return resp.Key, nil
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
func RunHelper(in io.Reader, out io.Writer) error {
	return serveHelper(in, out, backend{load: keyring.Get, save: keyring.Set, delete: keyring.Delete})
}

func serveHelper(in io.Reader, out io.Writer, b backend) error {
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
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return errors.New("credential helper request contains multiple values")
	}
	if req.Service != serviceName || !validProfile(req.Account) {
		return errors.New("credential helper request names another credential")
	}
	var resp helperResponse
	switch req.Op {
	case opLoad:
		resp.Key, err = b.load(req.Service, req.Account)
	case opSave:
		if ValidateKey(req.Key) != nil {
			return errors.New("credential helper request has an invalid key")
		}
		err = b.save(req.Service, req.Account, req.Key)
	case opDelete:
		err = b.delete(req.Service, req.Account)
	default:
		return errors.New("credential helper request has an unknown operation")
	}
	switch sanitize(err) {
	case nil:
		resp.Status = statusOK
	case ErrNotFound:
		resp.Status, resp.Key = statusNotFound, ""
	default:
		resp.Status, resp.Key = statusUnavailable, ""
	}
	return json.NewEncoder(out).Encode(resp)
}
