// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package nativeviewer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const testURL = "https://example.test/resident.pdf?signature=private-test-sentinel#page=1"

// sameSentinel reports whether err is target with nothing added: errors.Is
// matches and the message is target's own, so no wrapper carries private
// content out.
func sameSentinel(err, target error) bool {
	return errors.Is(err, target) && err.Error() == target.Error()
}

func requestForTest() Request {
	return Request{URL: testURL, Filename: "papio-viewer-test_123.pdf", Deadline: time.Now().Add(20 * time.Second)}
}

func helperDriver(t *testing.T, mode string) (*processDriver, string) {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "calls")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &processDriver{rpcTimeout: 3 * time.Second, command: func() *exec.Cmd {
		cmd := exec.Command(executable, "-test.run=^TestViewerHelperProcess$")
		cmd.Env = append(os.Environ(), "PAPIO_VIEWER_TEST_MODE="+mode, "PAPIO_VIEWER_TEST_TRACE="+trace)
		return cmd
	}}, trace
}

// Every response depends on the actual request and phase. This is a real child
// process with real pipes, not an in-memory driver or uniform-response stub.
func TestViewerHelperProcess(t *testing.T) {
	mode := os.Getenv("PAPIO_VIEWER_TEST_MODE")
	if mode == "" {
		return
	}
	trace, err := os.OpenFile(os.Getenv("PAPIO_VIEWER_TEST_TRACE"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(90)
	}
	defer trace.Close()
	scanner := bufio.NewScanner(os.Stdin)
	step := 0
	for scanner.Scan() {
		var request map[string]any
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(91)
		}
		method, _ := request["method"].(string)
		fmt.Fprintln(trace, method)
		if method == "viewer_prepare" {
			if step != 0 || len(request) != 4 || request["source_url"] != testURL || request["filename"] != "papio-viewer-test_123.pdf" {
				os.Exit(92)
			}
			deadline, ok := request["expires_at_ms"].(float64)
			if !ok || deadline <= float64(time.Now().UnixMilli()) {
				os.Exit(93)
			}
			// The transient URL must never be in process arguments or env.
			if strings.Contains(strings.Join(os.Args, " ")+strings.Join(os.Environ(), " "), "private-test-sentinel") {
				os.Exit(94)
			}
			step++
			if mode == "hang_prepare" {
				time.Sleep(time.Minute)
				os.Exit(95)
			}
			if strings.HasPrefix(mode, "raw:") {
				fmt.Println(strings.TrimPrefix(mode, "raw:"))
				continue
			}
			if mode == "oversize" {
				fmt.Println(strings.Repeat("x", responseLimit+1))
				continue
			}
			if mode == "partial" {
				fmt.Print(`{"ok":`)
				time.Sleep(time.Minute)
				continue
			}
			fmt.Println(`{"ok":true,"result":{"status":"pending","step":"prepared"}}`)
		} else if method == "viewer_advance" {
			if len(request) != 1 || step == 0 {
				os.Exit(96)
			}
			if mode == "hang_advance" {
				time.Sleep(time.Minute)
				continue
			}
			if mode == "reject_advance" {
				fmt.Println(`{"ok":false,"error":"https://example.test/secret?signature=do-not-log"}`)
				continue
			}
			if mode == "uniform" {
				fmt.Println(`{"ok":true,"result":{"status":"pending","step":"prepared"}}`)
				continue
			}
			if mode == "early_saved" {
				fmt.Println(`{"ok":true,"result":{"status":"saved","step":"final_save"}}`)
				continue
			}
			steps := []string{"", "document_save", "awaiting_dialog", "rename", "downloads", "awaiting_downloads", "final_save"}
			if step >= len(steps) {
				os.Exit(97)
			}
			status := "pending"
			if steps[step] == "final_save" {
				status = "saved"
			}
			fmt.Printf("{\"ok\":true,\"result\":{\"status\":%q,\"step\":%q}}\n", status, steps[step])
			step++
		} else if method == "viewer_cancel" {
			if len(request) != 1 || step == 0 {
				os.Exit(98)
			}
			if mode == "hang_cancel" {
				time.Sleep(time.Minute)
				continue
			}
			fmt.Println(`{"ok":true,"result":{"status":"cancelled","step":"cancelled"}}`)
		} else {
			os.Exit(99)
		}
	}
	os.Exit(0)
}

func readCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func assertReaped(t *testing.T, session Session) {
	t.Helper()
	select {
	case <-session.(*processSession).done:
	case <-time.After(2 * time.Second):
		t.Fatal("child was not reaped")
	}
}

func TestProcessSequenceAndNoReplay(t *testing.T) {
	d, trace := helperDriver(t, "normal")
	ctx, cancel := context.WithCancel(context.Background())
	s, err := d.Prepare(ctx, requestForTest())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cancel() // A transient prepare context does not own the returned session.
	if calls := readCalls(t, trace); len(calls) != 1 || calls[0] != "viewer_prepare" {
		t.Fatalf("prepare dispatched effects: %v", calls)
	}
	for i := 0; i < 6; i++ {
		status, err := s.Advance(context.Background())
		want := Pending
		if i == 5 {
			want = Saved
		}
		if err != nil || status != want {
			t.Fatalf("advance %d: %q, %v", i, status, err)
		}
	}
	status, err := s.Advance(context.Background())
	if status != Saved || err != nil {
		t.Fatalf("terminal read: %q %v", status, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertReaped(t, s)
	if calls := readCalls(t, trace); len(calls) != 7 {
		t.Fatalf("replayed a terminal effect: %v", calls)
	}
}

func TestPrepareRejectsMalformedOrUnsafeResponses(t *testing.T) {
	responses := []string{``, `null`, `[]`, `{}`, `{"ok":true}`, `{"ok":true,"result":null}`,
		`{"OK":true,"result":{"status":"pending","step":"prepared"}}`,
		`{"ok":true,"ok":true,"result":{"status":"pending","step":"prepared"}}`,
		`{"ok":true,"result":{"status":"pending","status":"pending","step":"prepared"}}`,
		`{"ok":true,"result":{"status":"pending","step":"prepared","extra":1}}`,
		`{"ok":true,"result":{"status":"pending","step":"prepared"},"extra":1}`,
		`{"ok":true,"result":{"status":"saved","step":"final_save"}}`,
		`{"ok":true,"result":{"status":"pending","step":"document_save"}}`,
		`{"ok":true,"result":{"status":"pending","step":"prepared"}} {}`,
		`{"ok":true,"error":"secret","result":{"status":"pending","step":"prepared"}}`,
		`{"ok":true,"error":null,"result":{"status":"pending","step":"prepared"}}`,
		`{"ok":false,"error":"https://example.test/signed?signature=do-not-log"}`}
	for i, response := range responses {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			d, _ := helperDriver(t, "raw:"+response)
			s, err := d.Prepare(context.Background(), requestForTest())
			if err == nil || s != nil {
				if s != nil {
					_ = s.Close()
				}
				t.Fatal("invalid reply accepted")
			}
			if strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "example.test") || strings.Contains(err.Error(), response) && response != "" {
				t.Fatalf("raw response leaked: %v", err)
			}
		})
	}
	t.Run("oversize", func(t *testing.T) {
		d, _ := helperDriver(t, "oversize")
		if _, err := d.Prepare(context.Background(), requestForTest()); err == nil {
			t.Fatal("oversize reply accepted")
		}
	})
}

func TestRejectsUniformAndEarlySuccessResponses(t *testing.T) {
	for _, mode := range []string{"uniform", "early_saved"} {
		t.Run(mode, func(t *testing.T) {
			d, _ := helperDriver(t, mode)
			s, err := d.Prepare(context.Background(), requestForTest())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Advance(context.Background()); !sameSentinel(err, errProtocol) {
				t.Fatalf("accepted invalid transition: %v", err)
			}
			assertReaped(t, s)
			_ = s.Close()
		})
	}
}

func TestRefusedSurfaceCancelsBeforeReapWithoutLeakingError(t *testing.T) {
	d, trace := helperDriver(t, "reject_advance")
	s, err := d.Prepare(context.Background(), requestForTest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance(context.Background()); !sameSentinel(err, errRejected) {
		t.Fatalf("unexpected error: %v", err)
	}
	assertReaped(t, s)
	if calls := readCalls(t, trace); strings.Join(calls, ",") != "viewer_prepare,viewer_advance,viewer_cancel" {
		t.Fatalf("cleanup sequence: %v", calls)
	}
	_ = s.Close()
}

func TestOnlyKnownHelperCodesSurviveInErrors(t *testing.T) {
	for _, code := range []string{"viewer_document_changed", "viewer_attention_changed", "viewer_attention_unavailable", "viewer_action_failed"} {
		d, _ := helperDriver(t, `raw:{"ok":false,"error":"`+code+`"}`)
		_, err := d.Prepare(context.Background(), requestForTest())
		if !errors.Is(err, errRejected) || !strings.Contains(err.Error(), code) {
			t.Fatalf("known refusal lost: %v", err)
		}
	}
	for _, code := range []string{"viewer_changed https://example.test/?secret=x", "viewer_changed\nsecret", "unknown"} {
		err := helperRejection(code)
		if !sameSentinel(err, errRejected) {
			t.Fatal("unrecognized helper text escaped the allowlist")
		}
	}
}

func TestPrepareCancellationAndPerRPCTimeout(t *testing.T) {
	for _, mode := range []string{"hang_prepare", "partial"} {
		t.Run(mode, func(t *testing.T) {
			d, _ := helperDriver(t, mode)
			d.rpcTimeout = 150 * time.Millisecond
			start := time.Now()
			if _, err := d.Prepare(context.Background(), requestForTest()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout: %v", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("RPC timeout failed to bound prepare")
			}
		})
	}
	d, _ := helperDriver(t, "hang_prepare")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := d.Prepare(ctx, requestForTest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestAdvanceCancellationAndIdleDeadlineReap(t *testing.T) {
	d, _ := helperDriver(t, "hang_advance")
	s, err := d.Prepare(context.Background(), requestForTest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, err := s.Advance(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("advance cancellation: %v", err)
	}
	assertReaped(t, s)
	_ = s.Close()

	d, _ = helperDriver(t, "normal")
	r := requestForTest()
	r.Deadline = time.Now().Add(time.Second)
	s, err = d.Prepare(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	assertReaped(t, s) // Absolute expiry kills an idle child without another RPC.
	if _, err := s.Advance(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired advance: %v", err)
	}
	_ = s.Close()
}

func TestCloseIsBoundedConcurrentAndIdempotent(t *testing.T) {
	d, trace := helperDriver(t, "hang_cancel")
	s, err := d.Prepare(context.Background(), requestForTest())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	assertReaped(t, s)
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close did not bound cancel and reap")
	}
	if calls := readCalls(t, trace); strings.Join(calls, ",") != "viewer_prepare,viewer_cancel" {
		t.Fatalf("repeated cancellation: %v", calls)
	}
}

func TestValidateRequestBeforeStartingHelper(t *testing.T) {
	for _, change := range []func(*Request){
		func(r *Request) { r.URL = "file:///tmp/paper.pdf" }, func(r *Request) { r.URL = "https://user:pass@example.test/paper.pdf" }, // gitleaks:allow -- synthetic userinfo rejection fixture
		func(r *Request) { r.URL = "https://example.test/a\nb" }, func(r *Request) { r.URL = "https://example.test/a b" },
		func(r *Request) { r.URL = "https://example.test/a\\b" },
		func(r *Request) { r.URL = "https://example.test/?x=\u0085" },
		func(r *Request) { r.URL = "https://example.test/?x=%zz" },
		func(r *Request) { r.URL = "https://example.test:65536/paper.pdf" },
		func(r *Request) { r.Filename = "papio-viewer-UPPER.pdf" }, func(r *Request) { r.Filename = "../papio-viewer-test.pdf" },
		func(r *Request) { r.Filename = "papio-viewer-.pdf" }, func(r *Request) { r.Filename = "papio-viewer-" + strings.Repeat("a", 112) + ".pdf" },
		func(r *Request) { r.Deadline = time.Time{} }, func(r *Request) { r.Deadline = time.Now().Add(-time.Second) },
		func(r *Request) { r.Deadline = time.Now().Add(3 * time.Minute) },
	} {
		d, trace := helperDriver(t, "normal")
		r := requestForTest()
		change(&r)
		if _, err := d.Prepare(context.Background(), r); !sameSentinel(err, errRequest) {
			t.Fatalf("invalid request: %v", err)
		}
		if len(readCalls(t, trace)) != 0 {
			t.Fatal("started a child for invalid request")
		}
	}
}

func TestPrepareEnvelopePreservesURLAndBoundsEncodedLine(t *testing.T) {
	r := requestForTest()
	prefix := "https://example.test/?signature="
	r.URL = prefix + strings.Repeat("&", 8192-len(prefix))
	if err := validateRequest(r, time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := encodeRequest(map[string]any{"method": "viewer_prepare", "source_url": r.URL, "filename": r.Filename, "expires_at_ms": r.Deadline.UnixMilli()})
	if err != nil || len(data) > requestLimit {
		t.Fatalf("bounded URL exceeds request envelope: %v", err)
	}
	if strings.Contains(string(data), `\u0026`) {
		t.Fatal("HTML escaping inflated the line")
	}
	var decoded map[string]any
	if json.Unmarshal(data, &decoded) != nil || decoded["source_url"] != r.URL {
		t.Fatal("control URL changed in transit")
	}
	// Quotes still require JSON escaping. Refuse an oversized envelope before
	// starting a helper, rather than writing a line its bounded reader rejects.
	d, trace := helperDriver(t, "normal")
	r.URL = prefix + strings.Repeat(`"`, 8192-len(prefix))
	if _, err := d.Prepare(context.Background(), r); !sameSentinel(err, errRequest) {
		t.Fatalf("oversized encoded line: %v", err)
	}
	if len(readCalls(t, trace)) != 0 {
		t.Fatal("spawned child for oversized envelope")
	}
}

func TestNewDriverRequiresExplicitEligibleHelper(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "helper")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "helper", dir, file, filepath.Join(dir, "absent")} {
		if NewDriver(path) != nil {
			t.Fatalf("ineligible helper accepted: %q", path)
		}
	}
	if err := os.Chmod(file, 0700); err != nil {
		t.Fatal(err)
	}
	if got := NewDriver(file); (got != nil) != (runtime.GOOS == "darwin") {
		t.Fatal("platform eligibility incorrect")
	}
}
