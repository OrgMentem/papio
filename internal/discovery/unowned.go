// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package discovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const UnownedPageBudget = 10
const UnownedScanCeiling = 5000

// UnownedRequest keeps continuation separate from the shipped SearchParams wire contract.
type UnownedRequest struct {
	Search       SearchParams `json:"search"`
	Continuation string       `json:"continuation,omitempty"`
}
type UnownedResult struct {
	Works        []DiscoveredWork `json:"works"`
	Truncated    bool             `json:"truncated"`
	Continuation string           `json:"continuation,omitempty"`
	Pages        int              `json:"pages"`
	Scanned      int              `json:"scanned"`
	TotalScanned int              `json:"total_scanned"`
	PageBudget   int              `json:"page_budget"`
	ScanCeiling  int              `json:"scan_ceiling"`
	Partial      bool             `json:"partial"`
	Detail       string           `json:"detail,omitempty"`
	Sources      []SourcePage     `json:"sources"`
}
type unownedCursor struct {
	Fingerprint string   `json:"f"`
	Source      int      `json:"s"`
	Next        string   `json:"n,omitempty"`
	Seen        []string `json:"seen"`
	Scanned     int      `json:"scanned"`
	Incomplete  bool     `json:"incomplete,omitempty"`
}

// SearchUnowned walks bounded pages in source preference order. Page sizes never
// exceed the remaining output slots, so advancing a cursor cannot discard rows.
// The classifier must fail on incomplete ownership evidence, not call it unowned.
func SearchUnowned(ctx context.Context, source Source, request UnownedRequest, classify func(context.Context, []DiscoveredWork) error) (UnownedResult, error) {
	result := UnownedResult{Works: []DiscoveredWork{}, Sources: []SourcePage{}, PageBudget: UnownedPageBudget, ScanCeiling: UnownedScanCeiling}
	params := normalizeParams(request.Search)
	if strings.TrimSpace(params.Query) == "" && !params.HasCitationSnowball() {
		return result, errors.New("query or citation seed is required")
	}
	if source == nil {
		return result, errors.New("discovery is not configured")
	}
	sources := []Source{source}
	multi, isMulti := source.(*Multi)
	if isMulti {
		sources = multi.sources
	}
	selected := make([]Source, 0, len(sources))
	names := []string{}
	for _, candidate := range sources {
		if candidate != nil && (params.Source == "" || candidate.Name() == params.Source) {
			selected = append(selected, candidate)
			names = append(names, candidate.Name())
		}
	}
	if len(selected) == 0 {
		return result, errors.New("no matching discovery source is configured")
	}
	fingerprint := searchFingerprint(params) + ":" + strings.Join(names, ",")
	cursor := unownedCursor{Fingerprint: fingerprint}
	if request.Continuation != "" {
		if len(request.Continuation) > 240000 || !strings.HasPrefix(request.Continuation, "du1.") {
			return result, ErrInvalidPageToken
		}
		data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(request.Continuation, "du1."))
		if err != nil {
			return result, ErrInvalidPageToken
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cursor); err != nil {
			return result, ErrInvalidPageToken
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return result, ErrInvalidPageToken
		}
		if cursor.Fingerprint != fingerprint || cursor.Source < 0 || cursor.Source >= len(selected) || cursor.Scanned < 0 || cursor.Scanned >= UnownedScanCeiling || len(cursor.Seen) > UnownedScanCeiling || len(cursor.Next) > maxPageTokenBytes {
			return result, ErrInvalidPageToken
		}
	}
	seen := make(map[string]bool, len(cursor.Seen))
	for _, key := range cursor.Seen {
		if len(key) != 32 {
			return result, ErrInvalidPageToken
		}
		seen[key] = true
	}
	for cursor.Source < len(selected) && len(result.Works) < params.Limit && result.Pages < UnownedPageBudget && cursor.Scanned < UnownedScanCeiling {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		pageParams := params
		pageParams.Limit = min(params.Limit-len(result.Works), UnownedScanCeiling-cursor.Scanned)
		current := selected[cursor.Source]
		var page SourcePage
		if isMulti {
			page = multi.searchSourcePage(ctx, current, pageParams, cursor.Next)
		} else {
			pager, ok := current.(PageSearcher)
			if !ok {
				return result, errors.New("discovery backend does not support continuation")
			}
			value, err := pager.SearchPage(ctx, pageParams, cursor.Next)
			if err != nil {
				return result, err
			}
			if err := checkPage(value); err != nil {
				return result, err
			}
			page = SourcePage{Source: current.Name(), State: value.State, Works: withSource(value.Works, current.Name()), Next: value.Next}
		}
		result.Pages++
		summary := page
		summary.Works = nil
		summary.Err = nil
		result.Sources = append(result.Sources, summary)
		if page.State == PageFailed {
			if errors.Is(page.Err, ErrInvalidPageToken) {
				return result, page.Err
			}
			result.Partial = true
			result.Detail = "a source failed; continuation retries that page"
			break
		}
		if len(page.Works) > pageParams.Limit {
			return result, fmt.Errorf("discovery source %s exceeded its page limit", current.Name())
		}
		if classify != nil {
			if err := classify(ctx, page.Works); err != nil {
				return result, err
			}
		}
		for _, item := range page.Works {
			cursor.Scanned++
			result.Scanned++
			key := discoveredWorkKey(item)
			if key == "" {
				continue
			}
			sum := sha256.Sum256([]byte(key))
			key = hex.EncodeToString(sum[:16])
			if seen[key] {
				continue
			}
			seen[key] = true
			cursor.Seen = append(cursor.Seen, key)
			if !item.Owned {
				result.Works = append(result.Works, item)
			}
		}
		switch page.State {
		case PageMore:
			if page.Next == cursor.Next {
				return result, errors.New("discovery continuation did not advance")
			}
			cursor.Next = page.Next
		case PageExhausted:
			cursor.Source++
			cursor.Next = ""
		case PageTruncated, PageUnsupported:
			cursor.Incomplete = true
			cursor.Source++
			cursor.Next = ""
		default:
			return result, errors.New("invalid discovery page state")
		}
	}
	result.TotalScanned = cursor.Scanned
	result.Partial = result.Partial || cursor.Incomplete
	result.Truncated = cursor.Source < len(selected) || result.Partial
	if cursor.Scanned >= UnownedScanCeiling && cursor.Source < len(selected) {
		result.Partial = true
		result.Detail = "scan ceiling reached; refine the query"
	} else if cursor.Source < len(selected) {
		data, _ := json.Marshal(cursor)
		result.Continuation = "du1." + base64.RawURLEncoding.EncodeToString(data)
		if result.Detail == "" && result.Pages == UnownedPageBudget {
			result.Detail = "page budget reached; resume with continuation"
		}
	}
	if cursor.Incomplete && result.Detail == "" {
		result.Detail = "a source reached its paging window or cannot page"
	}
	return result, nil
}
