// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package openalex

import (
	"errors"
	"net/url"
	"strings"

	"papio/internal/work"
)

// maxOpenAlexEntityRedirects bounds how many same-origin entity-merge 301 hops
// the resolver will follow. Each hop is a separate guarded client Do.
const maxOpenAlexEntityRedirects = 5

// entityMergeAlias records that the provider redirected Wold → Wnew; exact
// identity verification may accept the merged record at the new id.
type entityMergeAlias struct {
	from string
	to   string
}

func (a entityMergeAlias) accepts(record workRecord, requestedOpenAlex string) bool {
	if a.from == "" || a.to == "" || a.from == a.to {
		return echoesOpenAlex(record, requestedOpenAlex)
	}
	if requestedOpenAlex != a.from {
		return echoesOpenAlex(record, requestedOpenAlex)
	}
	return echoesOpenAlex(record, a.to)
}

func parseOpenAlexWorkPathID(endpoint *url.URL) (string, bool) {
	if endpoint == nil {
		return "", false
	}
	path := strings.Trim(endpoint.Path, "/")
	if path == "" {
		return "", false
	}
	parts := strings.Split(path, "/")
	// .../works/{id} or .../works/https://doi.org/...
	idx := -1
	for i, part := range parts {
		if strings.EqualFold(part, "works") {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(parts) {
		return "", false
	}
	tail := strings.Join(parts[idx+1:], "/")
	if strings.HasPrefix(strings.ToLower(tail), "https://doi.org/") || strings.HasPrefix(strings.ToLower(tail), "http://doi.org/") {
		return "", false
	}
	id, err := work.NormalizeOpenAlex(tail)
	if err != nil {
		return "", false
	}
	return id, true
}

func isOpenAlexEntitySingleton(endpoint *url.URL) bool {
	_, ok := parseOpenAlexWorkPathID(endpoint)
	return ok
}

func sameOpenAlexOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}
func resolveEntityMergeLocation(current *url.URL, location string) (*url.URL, error) {
	if current == nil {
		return nil, errors.New("openalex: entity redirect missing current URL")
	}
	if strings.TrimSpace(location) == "" {
		return nil, errors.New("openalex: entity redirect missing Location")
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return nil, errors.New("openalex: entity redirect Location is invalid")
	}
	resolved := current.ResolveReference(parsed)
	if !validHTTPURL(resolved.String()) {
		return nil, errors.New("openalex: entity redirect Location is invalid")
	}
	if !sameOpenAlexOrigin(current, resolved) {
		return nil, errors.New("openalex: refusing cross-host entity redirect")
	}
	if resolved.Query().Get("search") != "" {
		return nil, errors.New("openalex: refusing entity redirect to a search endpoint")
	}
	if _, ok := parseOpenAlexWorkPathID(resolved); !ok {
		return nil, errors.New("openalex: refusing entity redirect to a non-entity URL")
	}
	return resolved, nil
}

func mergeAliasFromRedirect(from, to *url.URL) entityMergeAlias {
	oldID, oldOK := parseOpenAlexWorkPathID(from)
	newID, newOK := parseOpenAlexWorkPathID(to)
	if !oldOK || !newOK || oldID == newID {
		return entityMergeAlias{}
	}
	return entityMergeAlias{from: oldID, to: newID}
}
