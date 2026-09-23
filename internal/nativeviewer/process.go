// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package nativeviewer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const responseLimit = 4096
const requestLimit = 16384

type processDriver struct {
	path       string
	rpcTimeout time.Duration
	// Test helpers are real subprocesses, without touching a browser.
	command func() *exec.Cmd
}

type processSession struct {
	mu         sync.Mutex
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	reader     *bufio.Reader
	done       chan struct{}
	stopOnce   sync.Once
	deadline   time.Time
	timer      *time.Timer
	rpcTimeout time.Duration
	step       string
	closed     bool
}

type reply struct {
	OK     *bool `json:"ok"`
	Result *struct {
		Status string `json:"status"`
		Step   string `json:"step"`
	} `json:"result,omitempty"`
	Error *string `json:"error,omitempty"`
}

func (d *processDriver) Prepare(ctx context.Context, r Request) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRequest(r, time.Now()); err != nil {
		return nil, err
	}
	request := map[string]any{"method": "viewer_prepare", "source_url": r.URL,
		"filename": r.Filename, "expires_at_ms": r.Deadline.UnixMilli()}
	if _, err := encodeRequest(request); err != nil {
		return nil, err
	}
	cmd := exec.Command(d.path)
	if d.command != nil {
		cmd = d.command()
	}
	// stderr is discarded: native diagnostics must not leak provider URLs or
	// dialog contents. Never put the URL, filename or deadline in argv or env.
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errTransport
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, errTransport
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, errTransport
	}
	s := &processSession{cmd: cmd, stdin: stdin, stdout: stdout,
		reader: bufio.NewReaderSize(stdout, responseLimit+1), done: make(chan struct{}),
		deadline: r.Deadline, rpcTimeout: d.rpcTimeout}
	go func() { _ = cmd.Wait(); close(s.done) }() // The sole Wait owner.
	s.timer = time.AfterFunc(time.Until(r.Deadline), s.stop)
	result, err := s.rpc(ctx, request, false)
	if err == nil && (result.Result.Status != string(Pending) || result.Result.Step != "prepared") {
		err = errProtocol
	}
	if err != nil {
		s.stop()
		s.reap()
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		s.stop()
		s.reap()
		return nil, err
	}
	s.step = "prepared"
	return s, nil
}

func (s *processSession) Advance(ctx context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errClosed
	}
	if err := ctx.Err(); err != nil {
		s.abort()
		return "", err
	}
	if !time.Now().Before(s.deadline) {
		s.abort()
		return "", context.DeadlineExceeded
	}
	if s.step == "final_save" {
		return Saved, nil
	}
	r, err := s.rpc(ctx, map[string]any{"method": "viewer_advance"}, false)
	if err == nil && !validTransition(s.step, r.Result.Status, r.Result.Step) {
		err = errProtocol
	}
	if err != nil {
		if errors.Is(err, errRejected) {
			s.cancelPanel()
		}
		s.abort()
		return "", err
	}
	s.step = r.Result.Step
	return Status(r.Result.Status), nil
}

func validTransition(from, status, to string) bool {
	if to == "final_save" {
		return status == string(Saved) && (from == "downloads" || from == "awaiting_downloads")
	}
	if status != string(Pending) {
		return false
	}
	switch from {
	case "prepared":
		return to == "document_save"
	case "document_save", "awaiting_dialog":
		return to == "awaiting_dialog" || to == "rename"
	case "rename":
		return to == "downloads"
	case "downloads", "awaiting_downloads":
		return to == "awaiting_downloads"
	}
	return false
}

func (s *processSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	// No cleanup retry: the helper may cancel only an already retained panel.
	s.cancelPanel()
	s.abort()
	return nil
}

func (s *processSession) cancelPanel() {
	if s.step != "final_save" && time.Now().Before(s.deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, _ = s.rpc(ctx, map[string]any{"method": "viewer_cancel"}, true)
		cancel()
	}
}

func (s *processSession) abort() { s.closed = true; s.stop(); s.reap() }

func (s *processSession) stop() {
	s.stopOnce.Do(func() {
		_ = s.cmd.Process.Kill()
		_ = s.stdin.Close()
		_ = s.stdout.Close()
	})
}

func (s *processSession) reap() {
	if s.timer != nil {
		s.timer.Stop()
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
	}
}

func (s *processSession) rpc(ctx context.Context, request any, cancelling bool) (reply, error) {
	var zero reply
	if err := ctx.Err(); err != nil {
		s.stop()
		return zero, err
	}
	if !time.Now().Before(s.deadline) {
		s.stop()
		return zero, context.DeadlineExceeded
	}
	bound, cancel := context.WithDeadline(ctx, s.deadline)
	defer cancel()
	call, cancelCall := context.WithTimeout(bound, s.rpcTimeout)
	defer cancelCall()
	data, err := encodeRequest(request)
	if err != nil {
		return zero, err
	}
	type outcome struct {
		r   reply
		err error
	}
	ready := make(chan outcome, 1)
	go func() {
		if _, err := s.stdin.Write(data); err != nil {
			ready <- outcome{err: errTransport}
			return
		}
		line, err := s.reader.ReadSlice('\n')
		if err != nil || len(line) > responseLimit {
			ready <- outcome{err: errProtocol}
			return
		}
		var r reply
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if !uniqueJSONKeys(line) || decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF || r.OK == nil {
			ready <- outcome{err: errProtocol}
			return
		}
		if !*r.OK {
			if r.Result != nil || r.Error == nil {
				ready <- outcome{err: errProtocol}
				return
			}
			ready <- outcome{err: helperRejection(*r.Error)}
			return
		}
		if r.Result == nil || r.Error != nil || r.Result.Status == "" || r.Result.Step == "" ||
			(cancelling && (r.Result.Status != "cancelled" || r.Result.Step != "cancelled")) {
			ready <- outcome{err: errProtocol}
			return
		}
		ready <- outcome{r: r}
	}()
	select {
	case <-call.Done():
		s.stop()
		return zero, call.Err()
	case o := <-ready:
		if err := call.Err(); err != nil {
			s.stop()
			return zero, err
		}
		return o.r, o.err
	}
}

// Preserve only this finite helper vocabulary for diagnostics. Never return an
// arbitrary helper string: it may contain a signed URL or native dialog text.
func helperRejection(code string) error {
	switch code {
	case "viewer_invalid_request", "viewer_expired", "viewer_unavailable", "viewer_ambiguous",
		"viewer_changed", "viewer_unsupported", "viewer_action_failed", "viewer_terminal",
		"viewer_application_changed", "viewer_document_changed", "viewer_save_control_changed",
		"viewer_attention_unavailable", "viewer_attention_changed":
		return fmt.Errorf("%w (%s)", errRejected, code)
	default:
		return errRejected
	}
}

func encodeRequest(request any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(request) != nil || buffer.Len() > requestLimit {
		return nil, errRequest
	}
	return buffer.Bytes(), nil // Encoder includes the line terminator.
}

// encoding/json otherwise accepts duplicate members (and case-insensitive
// struct names). Require the exact small schema before decoding its values.
func uniqueJSONKeys(line []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(line))
	var object func(bool) bool
	object = func(result bool) bool {
		token, err := decoder.Token()
		if err != nil || token != json.Delim('{') {
			return false
		}
		seen := map[string]bool{}
		for decoder.More() {
			token, err = decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || seen[key] {
				return false
			}
			seen[key] = true
			if result {
				if key != "status" && key != "step" {
					return false
				}
			} else if key != "ok" && key != "result" && key != "error" {
				return false
			}
			if key == "result" && !result {
				if !object(true) {
					return false
				}
			} else {
				token, err = decoder.Token()
				if err != nil || token == nil {
					return false
				}
				if _, nested := token.(json.Delim); nested {
					return false
				}
			}
		}
		token, err = decoder.Token()
		return err == nil && token == json.Delim('}')
	}
	return object(false) && decoder.Decode(new(any)) == io.EOF
}
