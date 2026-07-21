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
	"testing"
	"time"
)

func TestIssueServesBoundPDFWithRequiredHeadersAndRanges(t *testing.T) {
	server := New()
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown preview server: %v", err)
		}
	})
	pdf := []byte("%PDF-1.7\npreview bytes\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL, err := server.Issue(42, path, digest, int64(len(pdf)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(capabilityURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" {
		t.Fatalf("capability URL = %q, want literal loopback on an ephemeral port", capabilityURL)
	}
	token := strings.TrimPrefix(parsed.Path, "/p/")
	if len(token) != 43 {
		t.Fatalf("token length = %d, want 43 for 256 random bits in raw base64url", len(token))
	}

	request, err := http.NewRequest(http.MethodGet, capabilityURL, nil)
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
	assertHeader(t, response, "X-Content-Type-Options", "nosniff")
	assertHeader(t, response, "Cache-Control", "no-store")
	assertHeader(t, response, "Referrer-Policy", "no-referrer")
	if value := response.Header.Get("Access-Control-Allow-Origin"); value != "" {
		t.Fatalf("CORS header = %q, want absent", value)
	}

	head, err := http.NewRequest(http.MethodHead, capabilityURL, nil)
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

func TestPreviewRejectsWrongMethodsAndHosts(t *testing.T) {
	server := New()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\n%%EOF\n")
	path, digest := writePDF(t, pdf)
	capabilityURL, err := server.Issue(1, path, digest, int64(len(pdf)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

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
		t.Fatalf("POST status = %d, want %d", postResponse.StatusCode, http.StatusMethodNotAllowed)
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
	server := New()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	if _, err := server.Issue(1, t.TempDir(), strings.Repeat("0", 64), 0, time.Minute); err == nil {
		t.Fatal("issued a preview capability for a directory")
	}
}

func TestPreviewExpiryMismatchAndRevocation(t *testing.T) {
	server := New()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	pdf := []byte("%PDF-1.7\noriginal\n%%EOF\n")
	path, digest := writePDF(t, pdf)

	expiredURL, err := server.Issue(1, path, digest, int64(len(pdf)), -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, expiredURL, http.StatusNotFound)

	mismatchURL, err := server.Issue(2, path, digest, int64(len(pdf)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), pdf...)
	changed[10] = 'X'
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, mismatchURL, http.StatusGone)
	assertStatus(t, mismatchURL, http.StatusNotFound)

	newPath, newDigest := writePDF(t, pdf)
	revokedURL, err := server.Issue(3, newPath, newDigest, int64(len(pdf)), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server.Revoke(3)
	assertStatus(t, revokedURL, http.StatusNotFound)
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
