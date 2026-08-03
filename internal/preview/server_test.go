// Copyright 2026 OrgMentem. Licensed under MIT.

package preview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
}

func (r *recordingResolver) ResolveReviewCAS(_ context.Context, input job.ResolveReviewInput) (job.ReviewResolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, input)
	outcome := r.outcome
	if outcome == "" {
		outcome = job.ReviewApplied
	}
	return job.ReviewResolution{Outcome: outcome}, r.err
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
		"Is this “A Useful Paper” (Ada Lovelace, Grace Hopper, 2026)?",
		`src="/p/` + token + `/file"`,
		">Yes, correct file</button>",
		">No, wrong file</button>",
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
	if strings.Contains(body, "<script>") {
		t.Fatal("shell contained an inline script")
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
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "Recorded — you can close this tab") {
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
	if second.StatusCode != http.StatusConflict || !strings.Contains(secondBody, "Recorded — you can close this tab") {
		t.Fatalf("second response = %d %q", second.StatusCode, secondBody)
	}
	if calls := resolver.inputs(); len(calls) != 1 {
		t.Fatalf("resolver calls after second POST = %d, want 1", len(calls))
	}

	rendered, renderedBody := getResponse(t, capabilityURL)
	defer rendered.Body.Close()
	if rendered.StatusCode != http.StatusOK || !strings.Contains(renderedBody, "Recorded — you can close this tab") || !strings.Contains(renderedBody, "disabled") {
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
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "Recorded — you can close this tab") {
		t.Fatalf("reject response = %d %q", response.StatusCode, body)
	}
	calls := resolver.inputs()
	if len(calls) != 1 || calls[0].Verdict != "reject" {
		t.Fatalf("resolver calls = %+v", calls)
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
