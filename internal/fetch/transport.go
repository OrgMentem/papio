// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package fetch

import (
	"crypto/tls"
	"net/http"
)

// MetadataTransport returns a replay-bounded RoundTripper for shared metadata
// and discovery GET clients. It negotiates HTTP/1 only (so Go's HTTP/2
// transport cannot silently retry a bodyless GET inside one RoundTrip) and
// disables connection reuse when disableKeepAlives is true.
//
// The ALPN list must be pinned alongside Protocols. A transport cloned from
// http.DefaultTransport still advertises "h2" in the TLS handshake, so an
// HTTP/2-capable server selects h2 while this transport can only speak HTTP/1
// — the HTTP/1 reader then sees an HTTP/2 SETTINGS frame and every request to
// that host fails with "malformed HTTP response". Restricting Protocols alone
// silently broke every metadata source in production.
func MetadataTransport(disableKeepAlives bool) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetHTTP2(false)
	protos.SetUnencryptedHTTP2(false)
	transport.Protocols = &protos
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	transport.DisableKeepAlives = disableKeepAlives
	return transport
}
