// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package fetch

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMetadataTransportHTTP1OnlyNoKeepAlives(t *testing.T) {
	transport := MetadataTransport(true)
	rt, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if rt.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = true, want false")
	}
	if !rt.DisableKeepAlives {
		t.Fatal("DisableKeepAlives = false, want true")
	}
	if rt.Protocols == nil {
		t.Fatal("Protocols unset")
	}
	if !rt.Protocols.HTTP1() {
		t.Fatal("Protocols.HTTP1() = false, want true")
	}
	if rt.Protocols.HTTP2() || rt.Protocols.UnencryptedHTTP2() {
		t.Fatalf("Protocols = %v, want HTTP/1 only", rt.Protocols)
	}
}

func TestMetadataTransportAllowsKeepAlivesWhenDisabled(t *testing.T) {
	rt := MetadataTransport(false).(*http.Transport)
	if rt.DisableKeepAlives {
		t.Fatal("DisableKeepAlives = true, want false")
	}
}

// An HTTP/2-capable server must still be reachable. Restricting Protocols
// without pinning ALPN leaves "h2" advertised in the handshake: the server
// selects it, the HTTP/1 reader meets an HTTP/2 SETTINGS frame, and every
// request to that host fails with "malformed HTTP response". That shipped
// once and broke every metadata source in production, so this test makes a
// real request rather than asserting on fields.
func TestMetadataTransportReachesHTTP2CapableServer(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Errorf("handler write: %v", err)
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	rt := MetadataTransport(true).(*http.Transport)
	if rt.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig unset: ALPN cannot be pinned")
	}
	if got := rt.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("NextProtos = %v, want [http/1.1]", got)
	}
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	rt.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, NextProtos: rt.TLSClientConfig.NextProtos}

	resp, err := (&http.Client{Transport: rt}).Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("proto = %s, want HTTP/1.1", resp.Proto)
	}
}
