// Copyright 2026 OrgMentem. Licensed under MIT.

package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"papio/internal/job"
)

type recordingResolver struct {
	mu      sync.Mutex
	calls   []job.ResolveReviewInput
	outcome job.ReviewOutcome
	err     error

	// entered, when non-nil, receives once ResolveReviewCAS starts running —
	// letting a test block until a concurrent submit is genuinely in flight
	// instead of racing it with a sleep. release, when non-nil, is read from
	// before returning, letting a test hold that call open on purpose to
	// exercise the window where a second submit must see "still resolving",
	// not "recorded".
	entered chan struct{}
	release chan struct{}
}

func (r *recordingResolver) ResolveReviewCAS(_ context.Context, input job.ResolveReviewInput) (job.ReviewResolution, error) {
	r.mu.Lock()
	r.calls = append(r.calls, input)
	outcome := r.outcome
	if outcome == "" {
		outcome = job.ReviewApplied
	}
	err := r.err
	entered := r.entered
	release := r.release
	r.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		<-release
	}
	return job.ReviewResolution{Outcome: outcome}, err
}

func (r *recordingResolver) inputs() []job.ResolveReviewInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]job.ResolveReviewInput(nil), r.calls...)
}

func TestIssueServesCitationShellWithBoundPDFEmbed(t *testing.T) {
	server := New(&recordingResolver{})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\npreview bytes\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{
		Title: "A Useful Paper", Authors: []string{"Ada Lovelace", "Grace Hopper"}, Year: 2026,
	})

	response, body := getResponse(t, capabilityURL)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("shell status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	parsed, err := url.Parse(capabilityURL)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(parsed.Path, "/p/")
	if len(token) != 43 {
		t.Fatalf("token length = %d, want 43 for 256 random bits in raw base64url", len(token))
	}
	for _, want := range []string{
		`<span class="brand-name" aria-label="papio"><em>papio</em></span>`,
		"Is this “A Useful Paper” (Ada Lovelace, Grace Hopper, 2026)?",
		`src="/p/` + token + `/file"`,
		`class="primary" type="button" data-verdict="accept">Yes, correct file</button>`,
		`data-verdict="reject">No, wrong file</button>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("shell missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, path) {
		t.Fatalf("shell leaked quarantine path %q", path)
	}
	assertHeader(t, response, "Content-Type", "text/html; charset=utf-8")
	assertHeader(t, response, "Cache-Control", "no-store")
	assertHeader(t, response, "Referrer-Policy", "no-referrer")
	if csp := response.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "'unsafe-inline'") {
		t.Fatalf("Content-Security-Policy = %q", csp)
	}
	// The shell's only controls apply an irreversible, CAS-guarded verdict, so a
	// page holding the capability URL must not be able to frame it and harvest
	// a confirmation for a file the operator never saw. The extension only ever
	// opens this top-level, so refusing every ancestor costs nothing.
	assertHeader(t, response, "X-Frame-Options", "DENY")
	if csp := response.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("Content-Security-Policy missing frame-ancestors 'none': %q", csp)
	}
	if strings.Contains(body, "<script>") {
		t.Fatal("shell contained an inline script")
	}

	styleResponse, style := getResponse(t, capabilityURL+"/style.css")
	defer styleResponse.Body.Close()
	if styleResponse.StatusCode != http.StatusOK {
		t.Fatalf("style status = %d, want %d", styleResponse.StatusCode, http.StatusOK)
	}
	for _, want := range []string{
		"--color-ink: #182231",
		"--color-brand-ink: #2b2d42",
		"--color-brand-accent: #d94f3d",
		"--color-muted: #607080",
		"--color-border: #dce3ea",
		"--color-page: #f4f7f9",
		"--color-surface: #fdfefe",
		"--color-primary: #12549b",
		"--color-primary-surface: #eaf3ff",
		"--color-primary-border: #8db9eb",
		"font-family: ui-sans-serif, system-ui",
		"white-space: nowrap",
	} {
		if !strings.Contains(style, want) {
			t.Fatalf("style missing %q:\n%s", want, style)
		}
	}

	scriptResponse, script := getResponse(t, capabilityURL+"/script.js")
	defer scriptResponse.Body.Close()
	if scriptResponse.StatusCode != http.StatusOK {
		t.Fatalf("script status = %d, want %d", scriptResponse.StatusCode, http.StatusOK)
	}
	for _, want := range []string{
		"button.textContent = 'Recording…'",
		"'marked correct'",
		"'marked wrong file'",
		"'Closing this tab…'",
		"window.close()",
		"setTimeout(showBlockedClose, 600)",
		"'You can close this tab.'",
		"'Review recorded. This preview can now be closed.'",
		"await response.text()",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestAcceptVerdictResolvesReviewExactlyOnceAndRecordsCapability(t *testing.T) {
	resolver := &recordingResolver{}
	server := New(resolver)
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\naccept\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{})
	assertStatus(t, capabilityURL+"/file", http.StatusOK)

	response, body := postVerdict(t, capabilityURL, "accept")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "Recorded — review complete") || !strings.Contains(body, "You can close this tab.") {
		t.Fatalf("accept response = %d %q", response.StatusCode, body)
	}
	calls := resolver.inputs()
	if len(calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1", len(calls))
	}
	if got := calls[0]; got.ActionID != 42 || got.Verdict != "accept" || got.ExpectedRevision != 7 || got.ExpectedSHA256 != digest {
		t.Fatalf("resolve input = %+v", got)
	}

	second, secondBody := postVerdict(t, capabilityURL, "reject")
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict || !strings.Contains(secondBody, "Recorded — review complete") || !strings.Contains(secondBody, "You can close this tab.") {
		t.Fatalf("second response = %d %q", second.StatusCode, secondBody)
	}
	if calls := resolver.inputs(); len(calls) != 1 {
		t.Fatalf("resolver calls after second POST = %d, want 1", len(calls))
	}

	rendered, renderedBody := getResponse(t, capabilityURL)
	defer rendered.Body.Close()
	if rendered.StatusCode != http.StatusOK || !strings.Contains(renderedBody, "Recorded — review complete") || !strings.Contains(renderedBody, "You can close this tab.") || !strings.Contains(renderedBody, "disabled") || !strings.Contains(renderedBody, "fallback-panel") {
		t.Fatalf("recorded shell = %d %q", rendered.StatusCode, renderedBody)
	}
}

func TestRejectVerdictUsesReviewResolutionPath(t *testing.T) {
	resolver := &recordingResolver{}
	server := New(resolver)
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\nreject\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{})

	response, body := postVerdict(t, capabilityURL, "reject")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "Recorded — review complete") {
		t.Fatalf("reject response = %d %q", response.StatusCode, body)
	}
	calls := resolver.inputs()
	if len(calls) != 1 || calls[0].Verdict != "reject" {
		t.Fatalf("resolver calls = %+v", calls)
	}
}

func TestConcurrentVerdictWhileResolvingGetsRetryableConflictNotRecorded(t *testing.T) {
	resolver := &recordingResolver{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	server := New(resolver)
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\nconcurrent\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{})
	assertStatus(t, capabilityURL+"/file", http.StatusOK)

	// Drive the first submit directly (not through the postVerdict helper,
	// which calls t.Fatal — unsafe from a goroutine the test spawned) so it
	// can sit blocked inside ResolveReviewCAS while the test sends a second,
	// concurrent submit.
	type verdictResult struct {
		response *http.Response
		body     string
		err      error
	}
	firstDone := make(chan verdictResult, 1)
	go func() {
		response, err := http.Post(capabilityURL+"/verdict", "application/json", strings.NewReader(`{"verdict":"accept"}`))
		if err != nil {
			firstDone <- verdictResult{err: err}
			return
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		firstDone <- verdictResult{response: response, body: string(body), err: err}
	}()
	// Any assertion below exits via runtime.Goexit, which would otherwise
	// leave the first request parked inside ResolveReviewCAS forever and hang
	// the Shutdown in cleanup — turning a clear failure into a timeout panic
	// minutes later. Release the resolver on every exit path so a regression
	// reports its real assertion immediately. sync.Once because the happy
	// path closes the channel itself below.
	var releaseOnce sync.Once
	releaseResolver := func() { releaseOnce.Do(func() { close(resolver.release) }) }
	t.Cleanup(releaseResolver)

	<-resolver.entered // the first submit is now genuinely inside ResolveReviewCAS, not merely queued

	second, secondBody := postVerdict(t, capabilityURL, "reject")
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second response status = %d, want %d", second.StatusCode, http.StatusConflict)
	}
	if strings.Contains(secondBody, "Recorded — review complete") {
		t.Fatalf("second response falsely reported the review recorded before the first resolution committed: %q", secondBody)
	}
	if !strings.Contains(secondBody, "try again") {
		t.Fatalf("second response missing retry guidance: %q", secondBody)
	}
	// The recorded-shell response is text/html; the client script uses that,
	// not the status code, to decide whether to leave its buttons disabled.
	// A plain-text body here is what tells it this tab may still retry.
	if ct := second.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		t.Fatalf("second response Content-Type = %q, want plain text so the client script re-enables the buttons", ct)
	}

	releaseResolver() // let the first resolution actually commit
	first := <-firstDone
	if first.err != nil {
		t.Fatal(first.err)
	}
	defer first.response.Body.Close()
	if first.response.StatusCode != http.StatusOK || !strings.Contains(first.body, "Recorded — review complete") {
		t.Fatalf("first response = %d %q", first.response.StatusCode, first.body)
	}

	if calls := resolver.inputs(); len(calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1 (the concurrent submit must not invoke the resolver a second time)", len(calls))
	}
}

func TestFailedResolutionLeavesCapabilityRetryable(t *testing.T) {
	resolver := &recordingResolver{err: errors.New("resolver unavailable")}
	server := New(resolver)
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\nretry\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{})
	assertStatus(t, capabilityURL+"/file", http.StatusOK)

	failed, failedBody := postVerdict(t, capabilityURL, "accept")
	defer failed.Body.Close()
	if failed.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("failed response status = %d, want %d", failed.StatusCode, http.StatusServiceUnavailable)
	}
	if strings.Contains(failedBody, "Recorded — review complete") {
		t.Fatalf("failed resolution falsely reported recorded: %q", failedBody)
	}

	resolver.mu.Lock()
	resolver.err = nil
	resolver.mu.Unlock()

	// The capability must not be wedged as though a resolution were still in
	// flight: a retry after a failed attempt has to be accepted, and it must
	// actually reach the resolver rather than being told "still resolving".
	retry, retryBody := postVerdict(t, capabilityURL, "accept")
	defer retry.Body.Close()
	if retry.StatusCode != http.StatusOK || !strings.Contains(retryBody, "Recorded — review complete") {
		t.Fatalf("retry response = %d %q, want the resolution to actually succeed this time", retry.StatusCode, retryBody)
	}

	rendered, renderedBody := getResponse(t, capabilityURL)
	defer rendered.Body.Close()
	if rendered.StatusCode != http.StatusOK || !strings.Contains(renderedBody, "Recorded — review complete") {
		t.Fatalf("rendered shell after successful retry = %d %q", rendered.StatusCode, renderedBody)
	}

	if calls := resolver.inputs(); len(calls) != 2 {
		t.Fatalf("resolver calls = %d, want 2 (the failed attempt plus the accepted retry)", len(calls))
	}
}

func TestPDFSiblingRetainsHeadersAndRangeSupport(t *testing.T) {
	server := New(&recordingResolver{})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\npreview bytes\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{})

	request, err := http.NewRequest(http.MethodGet, capabilityURL+"/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=5-14")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want %d", response.StatusCode, http.StatusPartialContent)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, pdf[5:15]) {
		t.Fatalf("range body = %q, want %q", body, pdf[5:15])
	}
	assertHeader(t, response, "Content-Type", "application/pdf")
	assertHeader(t, response, "Content-Disposition", `inline; filename="preview.pdf"`)
	assertHeader(t, response, "X-Content-Type-Options", "nosniff")
	assertHeader(t, response, "Cache-Control", "no-store")
	assertHeader(t, response, "Referrer-Policy", "no-referrer")
	if value := response.Header.Get("Access-Control-Allow-Origin"); value != "" {
		t.Fatalf("CORS header = %q, want absent", value)
	}

	head, err := http.NewRequest(http.MethodHead, capabilityURL+"/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	headResponse, err := http.DefaultClient.Do(head)
	if err != nil {
		t.Fatal(err)
	}
	defer headResponse.Body.Close()
	if headResponse.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want %d", headResponse.StatusCode, http.StatusOK)
	}
	assertHeader(t, headResponse, "Content-Type", "application/pdf")
}

func TestShellWithoutCompleteMetadataUsesGenericQuestion(t *testing.T) {
	server := New(&recordingResolver{})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{Title: "Title without authors"})

	response, body := getResponse(t, capabilityURL)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "Is this the file you requested?") {
		t.Fatalf("generic shell = %d %q", response.StatusCode, body)
	}
}

func TestPreviewRejectsWrongMethodsAndHosts(t *testing.T) {
	server := New(&recordingResolver{})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{})

	post, err := http.NewRequest(http.MethodPost, capabilityURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	postResponse, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	defer postResponse.Body.Close()
	if postResponse.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST shell status = %d, want %d", postResponse.StatusCode, http.StatusMethodNotAllowed)
	}
	assertHeader(t, postResponse, "Allow", "GET, HEAD")

	wrongHost := httptest.NewRequest(http.MethodGet, capabilityURL, nil)
	wrongHost.Host = "localhost"
	wrongHostRecorder := httptest.NewRecorder()
	server.ServeHTTP(wrongHostRecorder, wrongHost)
	if wrongHostRecorder.Code != http.StatusForbidden {
		t.Fatalf("wrong Host status = %d, want %d", wrongHostRecorder.Code, http.StatusForbidden)
	}
}

func TestIssueRejectsDirectories(t *testing.T) {
	server := New(&recordingResolver{})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	_, err := server.Issue(IssueInput{
		ActionID: 1, Path: t.TempDir(), SHA256: strings.Repeat("0", 64), ExpectedRevision: 1, TTL: time.Minute,
	})
	if err == nil {
		t.Fatal("issued a preview capability for a directory")
	}
}

func TestPreviewExpiryMismatchAndRevocation(t *testing.T) {
	server := New(&recordingResolver{})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\noriginal\n%%EOF\n")
	path, digest := writePDF(t, pdf)

	expiredURL, err := server.Issue(IssueInput{
		ActionID: 1, Path: path, SHA256: digest, Size: int64(len(pdf)), ExpectedRevision: 1, TTL: -time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, expiredURL, http.StatusNotFound)

	mismatchURL, err := server.Issue(IssueInput{
		ActionID: 2, Path: path, SHA256: digest, Size: int64(len(pdf)), ExpectedRevision: 1, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), pdf...)
	changed[10] = 'X'
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mismatchURL+"/file", http.StatusGone)
	assertStatus(t, mismatchURL, http.StatusNotFound)

	newPath, newDigest := writePDF(t, pdf)
	revokedURL, err := server.Issue(IssueInput{
		ActionID: 3, Path: newPath, SHA256: newDigest, Size: int64(len(pdf)), ExpectedRevision: 1, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Revoke(3)
	assertStatus(t, revokedURL, http.StatusNotFound)
}

func TestPDFRepeatGETServesUnchangedFileEachTime(t *testing.T) {
	server := New(&recordingResolver{})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\nstable bytes\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{})

	for i := range 3 {
		response, body := getResponse(t, capabilityURL+"/file")
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK || body != string(pdf) {
			t.Fatalf("GET %d = %d %q, want 200 with unchanged bytes", i, response.StatusCode, body)
		}
	}
}

func TestPDFRepeatGETRefusesFileTamperedAfterFirstVerify(t *testing.T) {
	server := New(&recordingResolver{})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\noriginal bytes\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL := issuePreview(t, server, path, digest, len(pdf), Citation{})

	// The first GET verifies the hash and serves the bytes an operator is
	// about to look at.
	first, firstBody := getResponse(t, capabilityURL+"/file")
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK || firstBody != string(pdf) {
		t.Fatalf("first GET = %d %q, want 200 with original bytes", first.StatusCode, firstBody)
	}

	// The quarantine path is known to the process that produced the file,
	// and a quarantined file is untrusted by definition. Swap it for
	// different bytes of the SAME length (so a size-only check would miss
	// it) after the capability has already been verified once — this is the
	// TOCTOU window between what was verified and what a repeat GET of the
	// same capability URL would otherwise re-read from disk.
	swapped := append([]byte(nil), pdf...)
	swapped[10] = 'X'
	if err := os.WriteFile(path, swapped, 0o600); err != nil {
		t.Fatal(err)
	}

	second, secondBody := getResponse(t, capabilityURL+"/file")
	defer second.Body.Close()
	if second.StatusCode != http.StatusGone {
		t.Fatalf("second GET after tamper = %d %q, want %d (refused, not re-served)", second.StatusCode, secondBody, http.StatusGone)
	}
	if secondBody == string(swapped) {
		t.Fatal("second GET served the tampered bytes instead of refusing them")
	}

	// A failed re-verification revokes the capability outright, same as a
	// first-GET mismatch does.
	assertStatus(t, capabilityURL, http.StatusNotFound)
}

func TestPDFMissingRecordedHashFailsClosedWithClearError(t *testing.T) {
	server := New(&recordingResolver{})
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\nno hash\n%%EOF\n")
	path, _ := writePDF(t, pdf)
	if err := server.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Issue() itself already refuses an empty hash (validSHA256), so this
	// simulates the one way an entry could still carry one: a HumanAction
	// row created before the sha256 binding guard existed, sitting in a
	// long-running dev store (see AGENTS.md's "rows that predate a later
	// validation" footgun). servePDF must fail closed with a precise
	// message, not panic or silently serve unverified bytes.
	token, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.byToken[token] = &capability{
		actionID: 99, path: path, sha256: "", size: int64(len(pdf)),
		expectedRevision: 1, expires: time.Now().Add(time.Minute),
	}
	server.byAction[99] = token
	server.mu.Unlock()

	response, body := getResponse(t, "http://"+server.host()+"/p/"+token+"/file")
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("missing-hash GET status = %d, want %d", response.StatusCode, http.StatusInternalServerError)
	}
	if !strings.Contains(body, "no recorded hash") {
		t.Fatalf("missing-hash GET body = %q, want a message about the missing recorded hash", body)
	}
}

func issuePreview(t *testing.T, server *Server, path, digest string, size int, citation Citation) string {
	t.Helper()
	capabilityURL, err := server.Issue(IssueInput{
		ActionID: 42, Path: path, SHA256: digest, Size: int64(size), ExpectedRevision: 7, Citation: citation, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return capabilityURL
}

func postVerdict(t *testing.T, capabilityURL, verdict string) (*http.Response, string) {
	t.Helper()
	response, err := http.Post(capabilityURL+"/verdict", "application/json", strings.NewReader(`{"verdict":"`+verdict+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	return response, string(body)
}

func getResponse(t *testing.T, capabilityURL string) (*http.Response, string) {
	t.Helper()
	response, err := http.Get(capabilityURL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	return response, string(body)
}

func writePDF(t *testing.T, data []byte) (string, string) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "preview-*.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return file.Name(), hex.EncodeToString(sum[:])
}

func assertStatus(t *testing.T, capabilityURL string, want int) {
	t.Helper()
	response, err := http.Get(capabilityURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("GET %s status = %d, want %d", capabilityURL, response.StatusCode, want)
	}
}

func assertHeader(t *testing.T, response *http.Response, name, want string) {
	t.Helper()
	if value := response.Header.Get(name); value != want {
		t.Fatalf("%s = %q, want %q", name, value, want)
	}
}
