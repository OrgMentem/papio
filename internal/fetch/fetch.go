// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package fetch

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Resolver resolves a host immediately before each request hop. Implementations
// must return every address they would use; a single forbidden address refuses
// the whole host rather than choosing a convenient public address.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// Downloader downloads candidates with an injected resolver and transport.
// It owns neither injected dependency and never changes a supplied transport.
type requestSchemeKey struct{}

type Downloader struct {
	policy    Policy
	resolver  Resolver
	transport http.RoundTripper
}

// New constructs a bounded downloader. A nil resolver uses net.DefaultResolver
// and a nil transport uses a private clone of http.DefaultTransport.
func New(policy Policy, resolver Resolver, transport http.RoundTripper) (*Downloader, error) {
	if policy.HeaderTimeout == 0 {
		policy.HeaderTimeout = policy.Timeout
	}
	if policy.BodyTimeout == 0 {
		policy.BodyTimeout = policy.Timeout
	}
	if err := validatePolicy(policy); err != nil {
		return nil, err
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	d := &Downloader{policy: policy, resolver: resolver}
	d.transport = d.secureTransport(transport)
	return d, nil
}

// NewDownloader is an explicit spelling of New.
func NewDownloader(policy Policy, resolver Resolver, transport http.RoundTripper) (*Downloader, error) {
	return New(policy, resolver, transport)
}

func validatePolicy(p Policy) error {
	if p.MaxBytes <= 0 {
		return fmt.Errorf("fetch policy MaxBytes must be positive")
	}
	if p.Timeout <= 0 || p.ConnectTimeout <= 0 || p.HeaderTimeout <= 0 || p.BodyTimeout <= 0 {
		return fmt.Errorf("fetch policy timeouts must be positive")
	}
	if p.MaxRedirects < 0 {
		return fmt.Errorf("fetch policy MaxRedirects cannot be negative")
	}
	return nil
}

// Download fetches rawURL into quarantinePath. quarantinePath must not exist;
// it is opened once with O_EXCL and removed on every unsuccessful attempt.
func (d *Downloader) Download(ctx context.Context, rawURL, quarantinePath string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{}, invalid("invalid request URL")
	}
	return d.DownloadRequest(ctx, req, quarantinePath)
}

// Fetch is an alias for Download.
func (d *Downloader) Fetch(ctx context.Context, rawURL, quarantinePath string) (Result, error) {
	return d.Download(ctx, rawURL, quarantinePath)
}

// DownloadWithHeaders downloads rawURL with ephemeral request headers. Headers
// are copied into the request and are never retained in Result or Error. On a
// redirect outside the original origin, every caller-supplied header is removed
// before the next hop so source credentials and request metadata cannot leak.
func (d *Downloader) DownloadWithHeaders(ctx context.Context, rawURL string, headers map[string]string, quarantinePath string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Result{}, invalid("invalid request URL")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return d.DownloadRequest(ctx, req, quarantinePath)
}

// DownloadRequest downloads a GET request. It preserves request headers on the
// first hop only; on a redirect outside the original origin, it removes every
// caller-supplied header before the next hop.
func (d *Downloader) DownloadRequest(ctx context.Context, request *http.Request, quarantinePath string) (Result, error) {
	if request == nil || request.URL == nil {
		return Result{}, invalid("invalid request")
	}
	if request.Method != http.MethodGet {
		return Result{}, invalid("only GET requests are supported")
	}
	if quarantinePath == "" {
		return Result{}, invalid("quarantine path is required")
	}

	overall, cancelOverall := context.WithTimeout(ctx, d.policy.Timeout)
	defer cancelOverall()

	current := request.Clone(overall)
	current.Body = nil
	current.GetBody = nil
	current.Method = request.Method
	redirects := 0

	for {
		if err := d.validateURL(overall, current.URL); err != nil {
			return Result{}, err
		}
		resp, err := d.roundTrip(overall, current)
		if err != nil {
			return Result{}, classifyRequestError(err)
		}

		if isRedirect(resp.StatusCode) {
			location := resp.Header.Get("Location")
			closeBody(resp)
			if location == "" {
				return Result{}, invalidStatus(resp.StatusCode, "redirect missing location")
			}
			if redirects >= d.policy.MaxRedirects {
				return Result{}, invalidStatus(resp.StatusCode, "redirect limit exceeded")
			}
			nextURL, err := current.URL.Parse(location)
			if err != nil {
				return Result{}, invalidStatus(resp.StatusCode, "invalid redirect location")
			}
			next := current.Clone(overall)
			next.URL = nextURL
			next.Host = ""
			if !sameOrigin(current.URL, nextURL) {
				next.Header = crossHostHeaders(next.Header)
			}
			current = next
			redirects++
			continue
		}

		return d.saveResponse(overall, resp, current.URL, quarantinePath)
	}
}

// FetchRequest is an alias for DownloadRequest.
func (d *Downloader) FetchRequest(ctx context.Context, request *http.Request, quarantinePath string) (Result, error) {
	return d.DownloadRequest(ctx, request, quarantinePath)
}

func (d *Downloader) roundTrip(overall context.Context, request *http.Request) (*http.Response, error) {
	headerCtx, cancelHeader := context.WithCancel(overall)
	headerCtx = context.WithValue(headerCtx, requestSchemeKey{}, strings.ToLower(request.URL.Scheme))
	timer := time.AfterFunc(d.policy.HeaderTimeout, cancelHeader)
	defer timer.Stop()
	req := request.Clone(headerCtx)
	if req.Header.Get("User-Agent") == "" && d.policy.UserAgent != "" {
		req.Header.Set("User-Agent", d.policy.UserAgent)
	}
	type roundTripResult struct {
		response *http.Response
		err      error
	}
	result := make(chan roundTripResult, 1)
	go func() {
		response, err := d.transport.RoundTrip(req) //nolint:bodyclose // late responses are drained/closed via closeBody in the deadline branch below; the success path returns the body to the caller who closes it.
		result <- roundTripResult{response: response, err: err}
	}()

	select {
	case done := <-result:
		// A transport may complete at the same time as the header deadline.
		// Prefer the deadline so a late response can never bypass this bound.
		if err := headerCtx.Err(); err != nil {
			closeBody(done.response)
			return nil, err
		}
		if done.err != nil {
			cancelHeader()
			return nil, done.err
		}
		if done.response == nil || done.response.Body == nil {
			cancelHeader()
			return nil, errors.New("transport returned an empty response")
		}
		return done.response, nil
	case <-headerCtx.Done():
		// A non-conforming injected transport may ignore request contexts. Return
		// at the deadline anyway, then close a late response without surfacing it.
		go func() {
			done := <-result
			closeBody(done.response)
		}()
		return nil, headerCtx.Err()
	}
}

func (d *Downloader) saveResponse(overall context.Context, resp *http.Response, u *url.URL, quarantinePath string) (result Result, retErr error) {
	defer closeBody(resp)
	if err := statusError(resp); err != nil {
		return Result{}, err
	}
	if err := contentLengthAllowed(resp, d.policy.MaxBytes); err != nil {
		return Result{}, err
	}

	f, err := os.OpenFile(quarantinePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, invalid("unable to create quarantine file")
	}
	keep := false
	defer func() {
		closeErr := f.Close()
		if retErr == nil && closeErr != nil {
			result = Result{}
			retErr = retryable("unable to finalize quarantine file")
		}
		if !keep || retErr != nil {
			_ = os.Remove(quarantinePath)
		}
	}()

	bodyCtx, cancelBody := context.WithCancel(overall)
	bodyTimer := time.AfterFunc(d.policy.BodyTimeout, func() {
		cancelBody()
		_ = resp.Body.Close()
	})
	defer bodyTimer.Stop()
	defer cancelBody()

	hash := sha256.New()
	var sample []byte
	var written int64
	buf := make([]byte, 32*1024)
	for {
		if err := bodyCtx.Err(); err != nil {
			return Result{}, classifyContextError(err)
		}
		n, readErr := readBodyWithContext(bodyCtx, resp.Body, buf)
		if n > 0 {
			if int64(n) > d.policy.MaxBytes-written {
				return Result{}, invalid("response body exceeds configured size limit")
			}
			chunk := buf[:n]
			if len(sample) < 512 {
				need := 512 - len(sample)
				if need > len(chunk) {
					need = len(chunk)
				}
				sample = append(sample, chunk[:need]...)
			}
			if _, err := f.Write(chunk); err != nil {
				return Result{}, retryable("unable to write quarantine file")
			}
			if _, err := hash.Write(chunk); err != nil {
				return Result{}, retryable("unable to hash response")
			}
			written += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			if bodyCtx.Err() != nil {
				return Result{}, classifyContextError(bodyCtx.Err())
			}
			return Result{}, retryable("response body read failed")
		}
	}

	if err := bodyCtx.Err(); err != nil {
		return Result{}, classifyContextError(err)
	}
	sniffed := http.DetectContentType(sample)
	declared := mediaType(resp.Header.Get("Content-Type"))
	if unsafePayload(sample, sniffed, declared) {
		return Result{}, invalid("response payload is not an acceptable PDF")
	}

	keep = true
	return Result{
		TempPath:    quarantinePath,
		SHA256:      fmt.Sprintf("%x", hash.Sum(nil)),
		SizeBytes:   written,
		SniffedMIME: sniffed,
		ContentType: resp.Header.Get("Content-Type"),
		HTTPStatus:  resp.StatusCode,
		FinalHost:   u.Hostname(),
	}, nil
}

// readBodyWithContext closes a blocked response body and drains the worker
// before returning for cancellation, containing the worker's lifetime.
func readBodyWithContext(ctx context.Context, body io.ReadCloser, buffer []byte) (int, error) {
	type readResult struct {
		n   int
		err error
	}
	result := make(chan readResult, 1)
	go func() {
		n, err := body.Read(buffer)
		result <- readResult{n: n, err: err}
	}()
	select {
	case done := <-result:
		return done.n, done.err
	case <-ctx.Done():
		_ = body.Close()
		<-result
		return 0, ctx.Err()
	}
}

func (d *Downloader) validateURL(ctx context.Context, u *url.URL) error {
	if u == nil || u.Host == "" || u.User != nil {
		return blocked("destination is not permitted")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return blocked("destination scheme is not permitted")
	}
	if scheme == "http" && !d.policy.AllowHTTPLoopback {
		return blocked("destination scheme is not permitted")
	}
	host := u.Hostname()
	if host == "" {
		return blocked("destination is not permitted")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if (scheme == "http" && !ip.IsLoopback()) || d.addressBlocked(ip, scheme) {
			return blocked("destination address is not permitted")
		}
		return nil
	}
	lookupCtx, cancelLookup := context.WithTimeout(ctx, d.policy.ConnectTimeout)
	addresses, err := d.resolver.LookupNetIP(lookupCtx, "ip", host)
	lookupErr := lookupCtx.Err()
	cancelLookup()
	if err != nil || len(addresses) == 0 {
		if lookupErr != nil {
			return classifyContextError(lookupErr)
		}
		return retryable("destination lookup failed")
	}
	for _, ip := range addresses {
		if (scheme == "http" && !ip.IsLoopback()) || d.addressBlocked(ip, scheme) {
			return blocked("destination address is not permitted")
		}
	}
	return nil
}

func (d *Downloader) addressBlocked(addr netip.Addr, scheme string) bool {
	addr = addr.Unmap()
	if d.policy.AllowHTTPLoopback && scheme == "http" && addr.IsLoopback() {
		return false
	}
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsPrivate() || !addr.IsGlobalUnicast() {
		return true
	}
	if addr.Is4() {
		return inPrefixes(addr, blockedV4)
	}
	return inPrefixes(addr, blockedV6)
}

var blockedV4 = mustPrefixes(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
	"192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.175.48.0/24",
	"198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

var blockedV6 = mustPrefixes(
	"::/96", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
	"100:0:0:1::/64", "2001::/32", "2001:2::/48", "2001:10::/28", "2001:20::/28", "2001:db8::/32",
	"2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

func inPrefixes(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// secureTransport pins direct dials to addresses resolved and validated by this
// downloader. Non-standard RoundTrippers remain injectable for deterministic
// tests, while URL validation still occurs before every request they receive.
func (d *Downloader) secureTransport(base http.RoundTripper) http.RoundTripper {
	t, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	clone := t.Clone()
	// A proxy chooses its own destination and could therefore bypass the
	// resolver/IP policy. All fetches use the validated direct dialer below.
	clone.Proxy = nil
	dialer := &net.Dialer{Timeout: d.policy.ConnectTimeout}
	// Disable every custom TLS dialer so HTTPS also flows through the validated,
	// context-cancellable DialContext below. DialTLS is deprecated precisely
	// because it cannot cancel a dial via context; a value carried over from the
	// injected base would otherwise satisfy hasCustomTLSDialer and bypass
	// DialContext, so it is cleared even though referencing the field warns.
	clone.DialTLS = nil //nolint:staticcheck // clearing the deprecated non-cancellable TLS dialer preserves the security invariant
	clone.DialTLSContext = nil
	clone.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		var addresses []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			addresses = []netip.Addr{ip}
		} else {
			lookupCtx, cancelLookup := context.WithTimeout(ctx, d.policy.ConnectTimeout)
			addresses, err = d.resolver.LookupNetIP(lookupCtx, "ip", host)
			lookupErr := lookupCtx.Err()
			cancelLookup()
			if err != nil || len(addresses) == 0 || lookupErr != nil {
				return nil, errors.New("destination lookup failed")
			}
		}
		scheme, _ := ctx.Value(requestSchemeKey{}).(string)
		if scheme == "" {
			scheme = "https"
		}
		for _, ip := range addresses {
			if (scheme == "http" && !ip.IsLoopback()) || d.addressBlocked(ip, scheme) {
				return nil, errDialDestinationBlocked
			}
		}
		var lastErr error
		for _, ip := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	clone.TLSHandshakeTimeout = d.policy.ConnectTimeout
	clone.ResponseHeaderTimeout = d.policy.HeaderTimeout
	return clone
}

func contentLengthAllowed(resp *http.Response, max int64) error {
	for _, value := range resp.Header.Values("Content-Length") {
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 0 {
			return invalidStatus(resp.StatusCode, "invalid Content-Length")
		}
		if n > max {
			return invalidStatus(resp.StatusCode, "response exceeds configured size limit")
		}
	}
	if resp.ContentLength < -1 {
		return invalidStatus(resp.StatusCode, "invalid Content-Length")
	}
	if resp.ContentLength > max {
		return invalidStatus(resp.StatusCode, "response exceeds configured size limit")
	}
	return nil
}

func statusError(resp *http.Response) error {
	status := resp.StatusCode
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusNotFound || status == http.StatusGone || status == http.StatusUnauthorized || status == http.StatusForbidden:
		return invalidStatus(status, "permanent HTTP response")
	case status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500:
		return &Error{Class: ClassRetryable, HTTPStatus: status, RetryAfter: retryAfter(resp.Header.Get("Retry-After")), Msg: "temporary HTTP response"}
	default:
		return invalidStatus(status, "HTTP response rejected")
	}
}

func retryAfter(value string) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		const maxRetrySeconds = int64((1<<63 - 1) / int64(time.Second))
		if seconds <= maxRetrySeconds {
			return time.Duration(seconds) * time.Second
		}
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

func unsafePayload(sample []byte, sniffed, declared string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(string(sample)))
	if strings.HasPrefix(trimmed, "<!doctype html") || strings.HasPrefix(trimmed, "<html") ||
		strings.HasPrefix(trimmed, "<head") || strings.HasPrefix(trimmed, "<body") ||
		strings.HasPrefix(trimmed, "<form") {
		return true
	}
	if len(sample) >= 3 && string(sample[:3]) == "ID3" {
		return true
	}
	if len(sample) >= 2 && sample[0] == 0xff && (sample[1]&0xe0) == 0xe0 {
		return true
	}
	if strings.HasPrefix(strings.ToLower(sniffed), "text/html") || strings.HasPrefix(strings.ToLower(sniffed), "audio/") {
		return true
	}
	if declared == "text/html" || strings.HasPrefix(declared, "audio/") {
		return true
	}
	// A claimed application/pdf is not evidence: a valid candidate must still
	// have the PDF signature. This admits generic downloads only when their
	// bytes independently look like a PDF.
	return !pdfLike(sample)
}

func pdfLike(sample []byte) bool {
	return len(sample) >= 5 && string(sample[:5]) == "%PDF-"
}

func mediaType(value string) string {
	if value == "" {
		return ""
	}
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	}
	return strings.ToLower(parsed)
}

func isRedirect(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusFound ||
		status == http.StatusSeeOther || status == http.StatusTemporaryRedirect ||
		status == http.StatusPermanentRedirect
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// crossHostHeaders creates an empty header set for another origin. Candidate
// headers are entirely ephemeral and may contain credentials under arbitrary
// names, so keeping even apparently benign caller headers is unsafe.
func crossHostHeaders(http.Header) http.Header {
	return make(http.Header)
}

func closeBody(resp *http.Response) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

func blocked(msg string) *Error { return &Error{Class: ClassBlocked, Msg: msg} }
func invalid(msg string) *Error { return &Error{Class: ClassInvalid, Msg: msg} }
func invalidStatus(status int, msg string) *Error {
	return &Error{Class: ClassInvalid, HTTPStatus: status, Msg: msg}
}
func retryable(msg string) *Error { return &Error{Class: ClassRetryable, Msg: msg} }

var errDialDestinationBlocked = errors.New("destination address is not permitted")

func classifyRequestError(err error) error {
	if errors.Is(err, errDialDestinationBlocked) {
		return blocked("destination address is not permitted")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return classifyContextError(err)
	}
	return retryable("network request failed")
}

func classifyContextError(err error) error {
	if errors.Is(err, context.Canceled) {
		return retryable("request cancelled")
	}
	return retryable("request timed out")
}
