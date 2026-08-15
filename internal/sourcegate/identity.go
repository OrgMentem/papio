// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package sourcegate

import (
	"net/http"
	"strings"

	"papio/internal/config"
)

func trimAPIKey(key string) string {
	return strings.TrimSpace(key)
}

// openAlexAPIKeyFromRequest reads the OpenAlex credential from the outgoing
// request. OpenAlex accepts bearer authentication as equivalent to api_key=;
// papio sends only the header so redirected Locations cannot strip the key.
func openAlexAPIKeyFromRequest(req *http.Request) string {
	if req == nil {
		return ""
	}
	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	if len(auth) >= 7 && strings.EqualFold(auth[:7], "bearer ") {
		return trimAPIKey(auth[7:])
	}
	if req.URL != nil {
		return trimAPIKey(req.URL.Query().Get("api_key"))
	}
	return ""
}

// ServedIdentity names the identity a request goes out under, read from the
// OUTGOING request rather than from configuration: the same client serves both
// the keyed and the keyless tier, and a gate earned by one credential must
// never be attributed to the other.
func ServedIdentity(req *http.Request, keyed config.Source) (config.Source, bool) {
	served := keyed
	served.APIKey = trimAPIKey(served.APIKey)
	sent := openAlexAPIKeyFromRequest(req)
	switch sent {
	case served.APIKey:
		// served as configured, keyed or keyless alike
	case "":
		served.APIKey = ""
	default:
		return config.Source{}, false
	}
	return served, true
}

// SetOpenAlexAuthorization attaches the OpenAlex API key as a bearer token and
// removes any api_key query parameter so credentials never ride on URLs.
func SetOpenAlexAuthorization(req *http.Request, apiKey string) {
	if req == nil || req.URL == nil {
		return
	}
	query := req.URL.Query()
	query.Del("api_key")
	req.URL.RawQuery = query.Encode()
	req.Header.Del("Authorization")
	apiKey = trimAPIKey(apiKey)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

// ClearOpenAlexAuthorization removes bearer and query credentials for an
// anonymous request.
func ClearOpenAlexAuthorization(req *http.Request) {
	SetOpenAlexAuthorization(req, "")
}
