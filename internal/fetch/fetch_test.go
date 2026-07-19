package fetch

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func publicResolver(overrides map[string][]string) Resolver {
	return resolverFunc(func(_ context.Context, _ string, host string) ([]netip.Addr, error) {
		values := overrides[host]
		if values == nil {
			values = []string{"8.8.8.8"}
		}
		result := make([]netip.Addr, 0, len(values))
		for _, value := range values {
			result = append(result, netip.MustParseAddr(value))
		}
		return result, nil
	})
}

func testDownloader(t *testing.T, resolver Resolver, transport http.RoundTripper) *Downloader {
	t.Helper()
	p := DefaultPolicy()
	p.MaxBytes = 64
	p.Timeout = time.Second
	p.ConnectTimeout = time.Second
	p.HeaderTimeout = time.Second
	p.BodyTimeout = time.Second
	d, err := New(p, resolver, transport)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func response(status int, body string, headers map[string]string) *http.Response {
	h := make(http.Header)
	for key, value := range headers {
		h.Set(key, value)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func fetchError(t *testing.T, err error) *Error {
	t.Helper()
	var got *Error
	if !errors.As(err, &got) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	return got
}

func TestDownloadStreamsPDFToExclusiveQuarantinePath(t *testing.T) {
	const payload = "%PDF-1.7\nexample\n"
	d := testDownloader(t, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, payload, map[string]string{"Content-Type": "application/octet-stream"}), nil
	}))
	path := filepath.Join(t.TempDir(), "download.part")
	result, err := d.Download(context.Background(), "https://papers.example/file?token=secret", path)
	if err != nil {
		t.Fatal(err)
	}
	if result.TempPath != path || result.SizeBytes != int64(len(payload)) || result.SHA256 != "8f4ad115f0264bae2809eb61d604805f038425d460a9e5cac43b53fa8429542d" {
		t.Fatalf("unexpected result: %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != payload {
		t.Fatalf("quarantine contents = %q, %v", got, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("quarantine mode = %v, %v", info.Mode(), err)
	}
}

func TestRejectsExistingQuarantinePath(t *testing.T) {
	d := testDownloader(t, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "%PDF-1.7", nil), nil
	}))
	path := filepath.Join(t.TempDir(), "exists.part")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := d.Download(context.Background(), "https://papers.example/file", path)
	if got := fetchError(t, err); got.Class != ClassInvalid {
		t.Fatalf("class = %q", got.Class)
	}
	contents, _ := os.ReadFile(path)
	if string(contents) != "keep" {
		t.Fatalf("existing file was changed: %q", contents)
	}
}

func TestBlockedAddressRangesAndMixedDNS(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1", "10.0.0.1", "169.254.1.1", "100.64.0.1", "192.0.2.1",
		"198.18.0.1", "224.0.0.1", "240.0.0.1", "[::1]", "[fc00::1]",
		"[fe80::1]", "[ff02::1]", "[2001:db8::1]",
	} {
		t.Run(address, func(t *testing.T) {
			called := false
			d := testDownloader(t, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
				called = true
				return nil, errors.New("must not dial")
			}))
			_, err := d.Download(context.Background(), "https://"+address+"/file?signature=hidden", filepath.Join(t.TempDir(), "x"))
			if got := fetchError(t, err); got.Class != ClassBlocked {
				t.Fatalf("class = %q", got.Class)
			}
			if called || strings.Contains(err.Error(), "hidden") {
				t.Fatalf("blocked request leaked or dialed: called=%v err=%v", called, err)
			}
		})
	}

	called := false
	d := testDownloader(t, publicResolver(map[string][]string{"mixed.example": {"8.8.8.8", "10.1.2.3"}}), roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not dial")
	}))
	_, err := d.Download(context.Background(), "https://mixed.example/file", filepath.Join(t.TempDir(), "x"))
	if got := fetchError(t, err); got.Class != ClassBlocked || called {
		t.Fatalf("mixed DNS got class=%q called=%v", got.Class, called)
	}
}

func TestRedirectIsRevalidatedAndSensitiveHeadersAreStripped(t *testing.T) {
	var requests []*http.Request
	d := testDownloader(t, publicResolver(map[string][]string{"private.example": {"10.0.0.1"}}), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		switch req.URL.Hostname() {
		case "origin.example":
			return response(http.StatusFound, "", map[string]string{"Location": "https://other.example/file"}), nil
		case "other.example":
			return response(http.StatusOK, "%PDF-1.7", nil), nil
		default:
			return nil, errors.New("unexpected destination")
		}
	}))
	req, _ := http.NewRequest(http.MethodGet, "https://origin.example/file", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "session=secret")
	req.Header.Set("Proxy-Authorization", "Basic secret")
	if _, err := d.DownloadRequest(context.Background(), req, filepath.Join(t.TempDir(), "x")); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
		if requests[1].Header.Get(name) != "" {
			t.Fatalf("%s forwarded cross-host", name)
		}
	}

	privateCalls := 0
	d = testDownloader(t, publicResolver(map[string][]string{"private.example": {"10.0.0.1"}}), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		privateCalls++
		return response(http.StatusFound, "", map[string]string{"Location": "https://private.example/hidden?token=never"}), nil
	}))
	_, err := d.Download(context.Background(), "https://origin.example/start?token=secret", filepath.Join(t.TempDir(), "blocked"))
	if got := fetchError(t, err); got.Class != ClassBlocked || privateCalls != 1 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("redirect result class=%q calls=%d err=%v", got.Class, privateCalls, err)
	}
}

func TestDownloadWithHeadersDoesNotLeakCustomCredentialsAcrossHosts(t *testing.T) {
	var requests []*http.Request
	d := testDownloader(t, publicResolver(nil), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		if req.URL.Hostname() == "origin.example" {
			return response(http.StatusFound, "", map[string]string{"Location": "https://other.example/file"}), nil
		}
		return response(http.StatusOK, "%PDF-1.7", nil), nil
	}))
	headers := map[string]string{
		"Authorization": "Bearer transient-secret",
		"X-API-Key":     "transient-key",
		"Accept":        "application/pdf",
		"User-Agent":    "candidate-agent",
	}
	if _, err := d.DownloadWithHeaders(context.Background(), "https://origin.example/file", headers, filepath.Join(t.TempDir(), "x")); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	if got := requests[0].Header.Get("X-API-Key"); got != "transient-key" {
		t.Fatalf("first-hop custom credential = %q", got)
	}
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-API-Key", "Accept"} {
		if got := requests[1].Header.Get(name); got != "" {
			t.Fatalf("caller header %s forwarded across hosts: %q", name, got)
		}
	}
	if got := requests[1].Header.Get("User-Agent"); got == "candidate-agent" {
		t.Fatalf("caller User-Agent forwarded across hosts: %q", got)
	}
	if got := headers["Authorization"]; got != "Bearer transient-secret" {
		t.Fatalf("caller headers mutated: %q", got)
	}
}

func TestDownloadWithHeadersStripsCredentialsOnHTTPSDowngrade(t *testing.T) {
	var requests []*http.Request
	lookups := 0
	resolver := resolverFunc(func(_ context.Context, _ string, _ string) ([]netip.Addr, error) {
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	d := testDownloader(t, resolver, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		if req.URL.Scheme == "https" {
			return response(http.StatusFound, "", map[string]string{"Location": "http://origin.example/file"}), nil
		}
		return response(http.StatusOK, "%PDF-1.7", nil), nil
	}))
	d.policy.AllowHTTPLoopback = true

	headers := map[string]string{
		"Authorization": "Bearer transient-secret",
		"X-API-Key":     "transient-key",
	}
	if _, err := d.DownloadWithHeaders(context.Background(), "https://origin.example/file", headers, filepath.Join(t.TempDir(), "x")); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	if requests[1].URL.Scheme != "http" {
		t.Fatalf("second-hop scheme = %q", requests[1].URL.Scheme)
	}
	for _, name := range []string{"Authorization", "X-API-Key"} {
		if got := requests[1].Header.Get(name); got != "" {
			t.Fatalf("caller header %s forwarded to plaintext: %q", name, got)
		}
	}
}

func TestRedirectLimit(t *testing.T) {
	d := testDownloader(t, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusFound, "", map[string]string{"Location": "https://papers.example/again"}), nil
	}))
	d.policy.MaxRedirects = 1
	_, err := d.Download(context.Background(), "https://papers.example/start", filepath.Join(t.TempDir(), "x"))
	if got := fetchError(t, err); got.Class != ClassInvalid || got.HTTPStatus != http.StatusFound {
		t.Fatalf("unexpected error: %+v", got)
	}
}

func TestContentLengthAndStreamingLimitsCleanPartialFiles(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		body    string
	}{
		{"negative", map[string]string{"Content-Length": "-1"}, "%PDF-1.7"},
		{"oversized", map[string]string{"Content-Length": "65"}, "%PDF-1.7"},
		{"lying", map[string]string{"Content-Length": "1"}, "%PDF-" + strings.Repeat("x", 80)},
		{"missing", nil, "%PDF-" + strings.Repeat("x", 80)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := testDownloader(t, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, tc.body, tc.headers), nil
			}))
			path := filepath.Join(t.TempDir(), "partial")
			_, err := d.Download(context.Background(), "https://papers.example/file", path)
			if got := fetchError(t, err); got.Class != ClassInvalid {
				t.Fatalf("class = %q", got.Class)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("partial file remains: %v", statErr)
			}
		})
	}
}

func TestCancellationAndRejectedPayloadsCleanPartialFiles(t *testing.T) {
	var reads atomic.Int32
	d := testDownloader(t, publicResolver(nil), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &contextBody{ctx: req.Context(), reads: &reads}}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	path := filepath.Join(t.TempDir(), "cancelled")
	go func() {
		for reads.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	_, err := d.Download(ctx, "https://papers.example/file", path)
	if got := fetchError(t, err); got.Class != ClassRetryable {
		t.Fatalf("class = %q", got.Class)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled partial remains: %v", statErr)
	}

	for _, payload := range []string{"<!doctype html><title>Sign in</title>", "ID3\x04\x00\x00not a PDF", "\xff\xfb\x90\x64not a PDF"} {
		d := testDownloader(t, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(http.StatusOK, payload, map[string]string{"Content-Type": "application/pdf"}), nil
		}))
		path := filepath.Join(t.TempDir(), "rejected")
		_, err := d.Download(context.Background(), "https://papers.example/file", path)
		if got := fetchError(t, err); got.Class != ClassInvalid {
			t.Fatalf("payload class = %q", got.Class)
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rejected payload remains: %v", statErr)
		}
	}
}

func TestHTTPTestServerIsOnlyAllowedWithExplicitLocalOption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "%PDF-1.7\nlocal")
	}))
	defer server.Close()

	p := DefaultPolicy()
	p.MaxBytes = 64
	p.AllowHTTPLoopback = true
	d, err := New(p, netResolver{}, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Download(context.Background(), server.URL, filepath.Join(t.TempDir(), "ok")); err != nil {
		t.Fatal(err)
	}

	p.AllowHTTPLoopback = false
	d, err = New(p, netResolver{}, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Download(context.Background(), server.URL, filepath.Join(t.TempDir(), "blocked"))
	if got := fetchError(t, err); got.Class != ClassBlocked {
		t.Fatalf("class = %q", got.Class)
	}
}

type netResolver struct{}

func (netResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, network, host)
}

type contextBody struct {
	ctx   context.Context
	reads *atomic.Int32
}

func (b *contextBody) Read(_ []byte) (int, error) {
	b.reads.Add(1)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *contextBody) Close() error { return nil }

func TestHTTPStatusClassificationAndRetryAfter(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone, http.StatusUnauthorized, http.StatusForbidden} {
		d := testDownloader(t, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
			return response(status, "", nil), nil
		}))
		_, err := d.Download(context.Background(), "https://papers.example/file", filepath.Join(t.TempDir(), "x"))
		if got := fetchError(t, err); got.Class != ClassInvalid || got.HTTPStatus != status {
			t.Fatalf("status %d: %+v", status, got)
		}
	}
	d := testDownloader(t, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusTooManyRequests, "", map[string]string{"Retry-After": "3"}), nil
	}))
	_, err := d.Download(context.Background(), "https://papers.example/file", filepath.Join(t.TempDir(), "x"))
	if got := fetchError(t, err); got.Class != ClassRetryable || got.RetryAfter != 3*time.Second {
		t.Fatalf("retry-after: %+v", got)
	}
}

func TestResolverFailureAndNetworkFailureAreRetryable(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) { return nil, errors.New("dns down") })
	d := testDownloader(t, resolver, roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unreachable") }))
	_, err := d.Download(context.Background(), "https://papers.example/file", filepath.Join(t.TempDir(), "x"))
	if got := fetchError(t, err); got.Class != ClassRetryable || strings.Contains(err.Error(), "dns down") {
		t.Fatalf("error: %+v", got)
	}
}

func TestOnlyLoopbackHTTPIsAllowedByLocalDevelopmentOption(t *testing.T) {
	p := DefaultPolicy()
	p.MaxBytes = 64
	p.AllowHTTPLoopback = true
	called := false
	d, err := New(p, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return response(http.StatusOK, "%PDF-1.7", nil), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Download(context.Background(), "http://public.example/file", filepath.Join(t.TempDir(), "x"))
	if got := fetchError(t, err); got.Class != ClassBlocked || called {
		t.Fatalf("class=%q called=%v", got.Class, called)
	}
}

func TestCredentialsAreStrippedWhenOnlyPortChanges(t *testing.T) {
	var second *http.Request
	d := testDownloader(t, publicResolver(nil), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Port() == "" {
			return response(http.StatusFound, "", map[string]string{"Location": "https://papers.example:8443/file"}), nil
		}
		second = req.Clone(req.Context())
		return response(http.StatusOK, "%PDF-1.7", nil), nil
	}))
	req, _ := http.NewRequest(http.MethodGet, "https://papers.example/file", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if _, err := d.DownloadRequest(context.Background(), req, filepath.Join(t.TempDir(), "x")); err != nil {
		t.Fatal(err)
	}
	if second == nil || second.Header.Get("Authorization") != "" {
		t.Fatal("authorization survived a cross-port redirect")
	}
}

func TestReadBodyWithContextClosesAndDrainsBlockedReader(t *testing.T) {
	body := newCloseBlockingBody()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readDone := make(chan error, 1)
	go func() {
		_, err := readBodyWithContext(ctx, body, make([]byte, 1))
		readDone <- err
	}()
	select {
	case <-body.started:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}

	cancel()
	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not finish after cancellation")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("cancellation did not close response body")
	}
}

func TestHeaderBodyAndOverallTimeoutsAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name      string
		set       func(*Policy)
		transport http.RoundTripper
	}{
		{
			name: "header",
			set:  func(p *Policy) { p.HeaderTimeout = 10 * time.Millisecond },
			transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}),
		},
		{
			name: "body",
			set:  func(p *Policy) { p.BodyTimeout = 10 * time.Millisecond },
			transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: newCloseBlockingBody()}, nil
			}),
		},
		{
			name: "overall",
			set: func(p *Policy) {
				p.Timeout = 10 * time.Millisecond
				p.HeaderTimeout = time.Second
			},
			transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPolicy()
			p.MaxBytes = 64
			tc.set(&p)
			d, err := New(p, publicResolver(nil), tc.transport)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "partial")
			_, err = d.Download(context.Background(), "https://papers.example/file", path)
			if got := fetchError(t, err); got.Class != ClassRetryable {
				t.Fatalf("class = %q", got.Class)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("timeout partial remains: %v", statErr)
			}
		})
	}
}

type closeBlockingBody struct {
	closed  chan struct{}
	started chan struct{}
}

func newCloseBlockingBody() *closeBlockingBody {
	return &closeBlockingBody{closed: make(chan struct{}), started: make(chan struct{})}
}
func (b *closeBlockingBody) Read(_ []byte) (int, error) {
	close(b.started)
	<-b.closed
	return 0, errors.New("body closed")
}
func (b *closeBlockingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestSecureTransportDisablesProxyAndCustomTLSDialers(t *testing.T) {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = http.ProxyFromEnvironment
	base.DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("custom DialTLSContext must not be used")
	}

	d, err := New(DefaultPolicy(), publicResolver(nil), base)
	if err != nil {
		t.Fatal(err)
	}
	secured, ok := d.transport.(*http.Transport)
	if !ok {
		t.Fatalf("secured transport type = %T", d.transport)
	}
	// Assert the non-deprecated surface: the proxy and the context-cancellable
	// custom TLS dialer are cleared, and HTTPS is forced through the validated
	// DialContext so every connection stays cancellable and IP-checked.
	if secured.Proxy != nil || secured.DialTLSContext != nil {
		t.Fatal("secure transport retained a proxy or custom TLS dialer")
	}
	if secured.DialContext == nil {
		t.Fatal("secure transport must route dials through the validated DialContext")
	}
}

func TestLoopbackHTTPRebindIsBlockedAtDial(t *testing.T) {
	serverHit := atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		serverHit.Store(true)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	lookups := atomic.Int32{}
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	})
	p := DefaultPolicy()
	p.MaxBytes = 64
	p.AllowHTTPLoopback = true
	d, err := New(p, resolver, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Download(context.Background(), "http://rebind.test:"+port+"/file", filepath.Join(t.TempDir(), "x"))
	if got := fetchError(t, err); got.Class != ClassBlocked {
		t.Fatalf("class = %q", got.Class)
	}
	if lookups.Load() < 2 || serverHit.Load() {
		t.Fatalf("lookups=%d serverHit=%v", lookups.Load(), serverHit.Load())
	}
}

func TestHeaderTimeoutWinsResponseRace(t *testing.T) {
	p := DefaultPolicy()
	p.MaxBytes = 64
	p.HeaderTimeout = 10 * time.Millisecond
	p.BodyTimeout = time.Second
	p.Timeout = time.Second
	d, err := New(p, publicResolver(nil), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return response(http.StatusOK, "%PDF-1.7", nil), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Download(context.Background(), "https://papers.example/file", filepath.Join(t.TempDir(), "x"))
	if got := fetchError(t, err); got.Class != ClassRetryable {
		t.Fatalf("class = %q", got.Class)
	}
}

func TestInvalidRequestAndRetryAfterOverflow(t *testing.T) {
	d := testDownloader(t, publicResolver(nil), roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, "%PDF-1.7", nil), nil
	}))
	if got := fetchError(t, func() error {
		_, err := d.DownloadRequest(context.Background(), nil, filepath.Join(t.TempDir(), "x"))
		return err
	}()); got.Class != ClassInvalid {
		t.Fatalf("nil request class = %q", got.Class)
	}
	head, err := http.NewRequest(http.MethodHead, "https://papers.example/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fetchError(t, func() error {
		_, err := d.DownloadRequest(context.Background(), head, filepath.Join(t.TempDir(), "x"))
		return err
	}()); got.Class != ClassInvalid {
		t.Fatalf("HEAD request class = %q", got.Class)
	}
	if got := retryAfter("9223372036854775807"); got != 0 {
		t.Fatalf("overflow Retry-After = %v", got)
	}
}

func TestConnectTimeoutBoundsResolution(t *testing.T) {
	p := DefaultPolicy()
	p.MaxBytes = 64
	p.ConnectTimeout = 10 * time.Millisecond
	p.HeaderTimeout = time.Second
	p.Timeout = time.Second
	resolver := resolverFunc(func(ctx context.Context, _ string, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	d, err := New(p, resolver, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("must not reach transport")
	}))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = d.Download(context.Background(), "https://papers.example/file", filepath.Join(t.TempDir(), "x"))
	if got := fetchError(t, err); got.Class != ClassRetryable {
		t.Fatalf("class=%q", got.Class)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("resolution exceeded connect timeout: %v", elapsed)
	}
}
