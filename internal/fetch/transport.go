// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package fetch

import "net/http"

// MetadataTransport returns a replay-bounded RoundTripper for shared metadata
// and discovery GET clients. It negotiates HTTP/1 only (so Go's HTTP/2
// transport cannot silently retry a bodyless GET inside one RoundTrip) and
// disables connection reuse when disableKeepAlives is true.
func MetadataTransport(disableKeepAlives bool) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	var protos http.Protocols
	protos.SetHTTP1(true)
	protos.SetHTTP2(false)
	protos.SetUnencryptedHTTP2(false)
	transport.Protocols = &protos
	transport.DisableKeepAlives = disableKeepAlives
	return transport
}
