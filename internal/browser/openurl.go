// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package browser

import (
	"net/url"

	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/work"
)

const libKeyHost = "libkey.io"

// OpenURL delegates to the shared application-layer resolver implementation.
func OpenURL(base string, w work.Work) string {
	return app.OpenURL(base, w)
}

// LibKeyURL delegates to the shared application-layer resolver implementation.
func LibKeyURL(inst config.Institution, w work.Work) string {
	return app.LibKeyURL(inst, w)
}

// RouteURL delegates to the shared application-layer resolver implementation.
func RouteURL(inst config.Institution, w work.Work) string {
	return app.RouteURL(inst, w)
}

// verifiedProviderHosts are the registrable domains of providers with
// declarative adapters (or adapters in progress). They ride on every offer so
// the extension can recognize a post-SSO landing on an entitled provider: the
// resolver host alone goes blind the moment it routes the tab onward. Matching
// is exact-or-dot-suffix on the extension side.
//
// The browser protocol caps provider_hosts at 20 entries and extensions
// before 0.4.1 fail-closed on longer lists, so this list must stay within the
// cap and cannot simply grow with the adapter registry. Extensions from 0.4.1
// also recognize any host in their own adapter registry (the registry is the
// authoritative adapter-host source); this list only needs the families whose
// entitled landings predate that behavior.
var verifiedProviderHosts = []string{
	"jstor.org",
	"proquest.com",
	"ebscohost.com",
	"ebsco.com",
	"springer.com",
	"sciencedirect.com",
	"elsevier.com",
	"acm.org",
	"wiley.com",
	"tandfonline.com",
	"sagepub.com",
	"apa.org",
	"oup.com",
	"cell.com",
}

// resolverHost returns the hostname of the OpenURL base; it joins the verified
// provider hosts on an offer (the resolver host is the tab papio opens; the
// entitled provider host is where the resolver lands it).
func resolverHost(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
