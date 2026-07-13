// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package redact sanitizes values before they reach durable storage or logs.
// Invariant 11 of the stack plan: signed query values, cookies, API keys,
// credential fields, and page bodies are never persisted.
package redact

import (
	"net/url"
	"strings"
)

// URL strips userinfo, query, and fragment, keeping scheme://host/path. A
// bearer-signed URL therefore loses its token before persistence. Unparseable
// input collapses to a fixed placeholder rather than leaking raw bytes.
func URL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<unparseable-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	if u.RawQuery == "" && strings.Contains(raw, "?") {
		// Mark that something was removed so operators know evidence is partial.
		return u.String() + "?<redacted>"
	}
	return u.String()
}

// Host reduces a URL to scheme://host for error messages about untrusted
// destinations.
func Host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<unparseable-url>"
	}
	return u.Scheme + "://" + u.Host
}
