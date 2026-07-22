// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package institution recognizes library discovery URLs and derives supported
// OpenURL resolver bases without making network requests.
package institution

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Kind identifies the recognized library discovery service.
type Kind string

const (
	KindOpenURL  Kind = "openurl"
	KindPrimo    Kind = "primo"
	KindSFX      Kind = "sfx"
	KindWorldcat Kind = "worldcat"
	KindEBSCO    Kind = "ebsco"
	KindProQuest Kind = "proquest"
	KindUnknown  Kind = "unknown"
)

// Discovery describes the service identified from a user-provided URL.
type Discovery struct {
	Kind              Kind
	OpenURLBase       string
	ProquestAccountID string
	Note              string
}

var accountIDPattern = regexp.MustCompile(`(?i)[?&]accountid=([0-9]+)(?:[&#]|$)`)

// Discover classifies raw syntactically and, for known-safe URL shapes,
// derives an HTTPS OpenURL resolver base. It never contacts the service.
func Discover(raw string) (Discovery, error) {
	parsed, err := parseHTTPSURL(raw)
	if err != nil {
		return Discovery{}, err
	}

	accountID := accountID(strings.TrimSpace(raw))
	discovery := Discovery{ProquestAccountID: accountID}

	host := strings.ToLower(parsed.Hostname())
	switch {
	case strings.HasSuffix(host, ".primo.exlibrisgroup.com"):
		return discoverPrimo(parsed, discovery), nil
	case isSFX(parsed, host):
		return discoverSFX(parsed, discovery), nil
	case strings.HasSuffix(host, ".on.worldcat.org"):
		discovery.Kind = KindWorldcat
		discovery.OpenURLBase = "https://" + parsed.Host + "/atoztitles/link"
		discovery.Note = "Recognized WorldCat Discovery and derived its OpenURL link endpoint."
		return discovery, nil
	case host == "resolver.ebscohost.com":
		discovery.Kind = KindEBSCO
		query := retainedQuery(parsed.Query(), "custid", "groupid", "profile")
		discovery.OpenURLBase = baseURL(parsed, "/openurl", query)
		discovery.Note = derivedNote("EBSCO", query)
		return discovery, nil
	case strings.HasSuffix(host, "proquest.com"):
		return discoverProQuest(parsed, discovery), nil
	case hasPathSegment(parsed.Path, func(segment string) bool { return strings.EqualFold(segment, "openurl") }) || hasOpenURLVersion(parsed.Query()):
		discovery.Kind = KindOpenURL
		query := retainedQuery(parsed.Query(), "institution", "vid", "custid")
		discovery.OpenURLBase = baseURL(parsed, parsed.EscapedPath(), query)
		discovery.Note = derivedNote("an OpenURL resolver", query)
		return discovery, nil
	default:
		discovery.Kind = KindUnknown
		discovery.Note = "This URL was not recognized; paste your library's OpenURL resolver base or a Primo/SFX/WorldCat/EBSCO link — your library's Zotero setup page usually shows it."
		return discovery, nil
	}
}

func parseHTTPSURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed == nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("resolver URL must be an absolute https URL")
	}
	if strings.EqualFold(parsed.Scheme, "http") {
		return nil, fmt.Errorf("resolvers must be https")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, fmt.Errorf("resolver URL must be an absolute https URL")
	}
	return parsed, nil
}

func discoverPrimo(parsed *url.URL, discovery Discovery) Discovery {
	vid := parsed.Query().Get("vid")
	parts := strings.SplitN(vid, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		discovery.Kind = KindPrimo
		discovery.Note = "Recognized Primo; open your library search and paste a URL that includes vid=."
		return discovery
	}

	discovery.Kind = KindPrimo
	discovery.OpenURLBase = "https://" + parsed.Host + "/discovery/openurl?" + url.Values{
		"institution": {parts[0]},
		"vid":         {vid},
	}.Encode()
	discovery.Note = "Recognized Primo and derived its OpenURL endpoint from vid; citation parameters were dropped."
	return discovery
}

func discoverSFX(parsed *url.URL, discovery Discovery) Discovery {
	path := parsed.EscapedPath()
	segments := strings.Split(parsed.Path, "/")
	for index, segment := range segments {
		lower := strings.ToLower(segment)
		if lower == "sfx" || strings.HasPrefix(lower, "sfx_") {
			path = strings.Join(strings.Split(parsed.EscapedPath(), "/")[:index+1], "/")
			break
		}
	}

	discovery.Kind = KindSFX
	query := retainedQuery(parsed.Query(), "institution", "vid", "custid")
	discovery.OpenURLBase = baseURL(parsed, path, query)
	discovery.Note = derivedNote("SFX", query)
	return discovery
}

func discoverProQuest(parsed *url.URL, discovery Discovery) Discovery {
	discovery.Kind = KindProQuest
	if hasPathSegment(parsed.Path, func(segment string) bool { return strings.EqualFold(segment, "openurl") }) {
		query := retainedQuery(parsed.Query(), "institution", "vid", "custid")
		discovery.OpenURLBase = baseURL(parsed, parsed.EscapedPath(), query)
		discovery.Note = "Recognized a ProQuest OpenURL handler"
		if discovery.ProquestAccountID != "" {
			discovery.Note += "; captured accountid=" + discovery.ProquestAccountID
		}
		discovery.Note += "; citation parameters were dropped."
		return discovery
	}

	discovery.Note = "Recognized ProQuest"
	if discovery.ProquestAccountID != "" {
		discovery.Note += "; captured accountid=" + discovery.ProquestAccountID
	}
	discovery.Note += ". Paste your library's OpenURL resolver base to configure a resolver."
	return discovery
}

func isSFX(parsed *url.URL, host string) bool {
	return strings.HasPrefix(host, "sfx.") || hasPathSegment(parsed.Path, func(segment string) bool {
		lower := strings.ToLower(segment)
		return lower == "sfx" || strings.HasPrefix(lower, "sfx_")
	})
}

func hasPathSegment(path string, matches func(string) bool) bool {
	for _, segment := range strings.Split(path, "/") {
		if matches(segment) {
			return true
		}
	}
	return false
}

func hasOpenURLVersion(query url.Values) bool {
	for key, values := range query {
		if !strings.EqualFold(key, "url_ver") {
			continue
		}
		for _, value := range values {
			if strings.EqualFold(value, "Z39.88-2004") {
				return true
			}
		}
	}
	return false
}

func retainedQuery(query url.Values, names ...string) url.Values {
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	retained := make(url.Values)
	for key, values := range query {
		if !allowed[strings.ToLower(key)] {
			continue
		}
		retained[strings.ToLower(key)] = append(retained[strings.ToLower(key)], values...)
	}
	return retained
}

func baseURL(parsed *url.URL, path string, query url.Values) string {
	base := "https://" + parsed.Host + path
	if encoded := query.Encode(); encoded != "" {
		base += "?" + encoded
	}
	return base
}

func accountID(raw string) string {
	match := accountIDPattern.FindStringSubmatch(raw)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func derivedNote(service string, query url.Values) string {
	if encoded := query.Encode(); encoded != "" {
		return "Recognized " + service + ", derived an OpenURL base, and kept institutional query parameters (" + strings.Join(queryKeys(query), ", ") + ") while dropping citation parameters."
	}
	return "Recognized " + service + " and derived an OpenURL base; citation parameters were dropped."
}

func queryKeys(query url.Values) []string {
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	// url.Values.Encode sorts its keys; preserve that ordering in the note too.
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
