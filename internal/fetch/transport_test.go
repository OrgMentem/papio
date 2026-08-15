// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package fetch

import (
	"net/http"
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
