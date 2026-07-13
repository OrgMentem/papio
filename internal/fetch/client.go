// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// SecureHTTPClient is the resolver-facing HTTP client. It applies the same
// per-hop DNS/IP and redirect policy as artifact downloads while returning the
// response body to the caller for its source-specific bounded decoder.
type SecureHTTPClient struct {
	downloader *Downloader
}

// NewSecureHTTPClient constructs a direct, SSRF-resistant client. Policy bounds
// DNS, headers, body reads, redirects, total duration, and maximum body bytes.
func NewSecureHTTPClient(policy Policy, resolver Resolver, transport http.RoundTripper) (*SecureHTTPClient, error) {
	d, err := New(policy, resolver, transport)
	if err != nil {
		return nil, err
	}
	return &SecureHTTPClient{downloader: d}, nil
}

// Do implements the small HTTPClient interfaces used by metadata resolvers.
// Caller-supplied headers are removed on every cross-host redirect.
func (c *SecureHTTPClient) Do(request *http.Request) (*http.Response, error) {
	if c == nil || c.downloader == nil || request == nil || request.URL == nil {
		return nil, invalid("invalid request")
	}
	if request.Method != http.MethodGet {
		return nil, invalid("only GET requests are supported")
	}
	d := c.downloader
	overall, cancelOverall := context.WithTimeout(request.Context(), d.policy.Timeout)
	current := request.Clone(overall)
	current.Body = nil
	current.GetBody = nil
	redirects := 0
	for {
		if err := d.validateURL(overall, current.URL); err != nil {
			cancelOverall()
			return nil, err
		}
		resp, err := d.roundTrip(overall, current)
		if err != nil {
			cancelOverall()
			return nil, classifyRequestError(err)
		}
		if isRedirect(resp.StatusCode) {
			location := resp.Header.Get("Location")
			closeBody(resp)
			if location == "" {
				cancelOverall()
				return nil, invalidStatus(resp.StatusCode, "redirect missing location")
			}
			if redirects >= d.policy.MaxRedirects {
				cancelOverall()
				return nil, invalidStatus(resp.StatusCode, "redirect limit exceeded")
			}
			nextURL, err := current.URL.Parse(location)
			if err != nil {
				cancelOverall()
				return nil, invalidStatus(resp.StatusCode, "invalid redirect location")
			}
			next := current.Clone(overall)
			next.URL = nextURL
			next.Host = ""
			if !sameHost(current.URL, nextURL) {
				next.Header = crossHostHeaders(next.Header)
			}
			current = next
			redirects++
			continue
		}
		if resp.ContentLength > d.policy.MaxBytes {
			closeBody(resp)
			cancelOverall()
			return nil, invalid("response body exceeds configured size limit")
		}
		bodyCtx, cancelBody := context.WithTimeout(overall, d.policy.BodyTimeout)
		resp.Body = &secureResponseBody{
			body:      resp.Body,
			ctx:       bodyCtx,
			cancel:    func() { cancelBody(); cancelOverall() },
			remaining: d.policy.MaxBytes,
		}
		resp.Request = current
		return resp, nil
	}
}

type secureResponseBody struct {
	body      io.ReadCloser
	ctx       context.Context
	cancel    context.CancelFunc
	remaining int64
	once      sync.Once
	scratch   [32 << 10]byte
	failed    error
}

func (b *secureResponseBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.failed != nil {
		return 0, b.failed
	}
	type result struct {
		n   int
		err error
	}
	if b.remaining == 0 {
		var probe [1]byte
		done := make(chan result, 1)
		go func() {
			n, err := b.body.Read(probe[:])
			done <- result{n: n, err: err}
		}()
		select {
		case got := <-done:
			if got.n == 0 && errors.Is(got.err, io.EOF) {
				return 0, io.EOF
			}
			if got.n > 0 {
				b.failed = errors.New("response body exceeds configured size limit")
				_ = b.Close()
				return 0, b.failed
			}
			return 0, got.err
		case <-b.ctx.Done():
			b.failed = fmt.Errorf("response body deadline: %w", b.ctx.Err())
			_ = b.Close()
			return 0, b.failed
		}
	}
	size := int64(len(p))
	if size > int64(len(b.scratch)) {
		size = int64(len(b.scratch))
	}
	if size > b.remaining {
		size = b.remaining
	}
	buffer := b.scratch[:size]
	done := make(chan result, 1)
	go func() {
		n, err := b.body.Read(buffer)
		done <- result{n: n, err: err}
	}()
	select {
	case got := <-done:
		b.remaining -= int64(got.n)
		copy(p, buffer[:got.n])
		return got.n, got.err
	case <-b.ctx.Done():
		b.failed = fmt.Errorf("response body deadline: %w", b.ctx.Err())
		_ = b.Close()
		return 0, b.failed
	}
}

func (b *secureResponseBody) Close() error {
	var err error
	b.once.Do(func() {
		b.cancel()
		err = b.body.Close()
	})
	return err
}

var _ interface {
	Do(*http.Request) (*http.Response, error)
} = (*SecureHTTPClient)(nil)
