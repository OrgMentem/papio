// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package resolver defines the source contract and deterministic candidate
// ranking. A resolver returns observations, never authority: candidates carry
// live URLs in memory only; persistence stores redacted forms.
package resolver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"papio/internal/work"
)

// Version preference values.
const (
	VersionPublished = "published"
	VersionAccepted  = "accepted"
	VersionPreprint  = "preprint"
	VersionUnknown   = "unknown"
)

// Access bases (stack plan candidate semantics).
const (
	AccessOpen          = "open_access"
	AccessLicensedAPI   = "licensed_api"
	AccessInstitutional = "institutional"
	AccessManual        = "manual"
)

// Candidate is one acquisition option for a work. URL may be bearer-signed and
// MUST NOT be persisted or logged; use Redacted()/Key() for durable forms.
type Candidate struct {
	Source       string
	URL          string
	Landing      string
	Version      string
	AccessBasis  string
	ReuseLicense string // "unknown" when the source does not state one
	ExpectedMIME string
	// RequestHeaders are ephemeral source credentials or content negotiation.
	// They follow the live URL in memory only and are never persisted/events.
	RequestHeaders map[string]string
	// ResolvedWork carries source metadata discovered while resolving. The app
	// may fill fields missing from the request after identity-consistency checks.
	ResolvedWork       work.Work
	CostUSD            float64
	Direct             bool    // a direct file URL rather than a landing page
	IdentityConfidence float64 // 0..1 resolver-side confidence
	Evidence           []string
}

// Key is a stable dedupe key for the candidate URL (hash, not the URL itself).
func (c Candidate) Key() string {
	sum := sha256.Sum256([]byte(c.Source + "\x00" + c.URL))
	return hex.EncodeToString(sum[:16])
}

// Resolver is one source adapter.
type Resolver interface {
	Name() string
	// Resolve returns zero or more candidates for the work. Absence of results
	// is (nil, nil). Failures are classified: wrap retryable conditions
	// (timeouts, 429, 5xx) in *TemporaryError; anything else marks the source
	// failed for this pass but does not abort the job.
	Resolve(ctx context.Context, w work.Work) ([]Candidate, error)
}

// TemporaryError marks a retryable resolver failure and optionally carries the
// server-requested wait.
type TemporaryError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *TemporaryError) Error() string { return e.Err.Error() }
func (e *TemporaryError) Unwrap() error { return e.Err }

// Temporary reports whether err is retryable and its suggested wait.
func Temporary(err error) (time.Duration, bool) {
	var te *TemporaryError
	if errors.As(err, &te) {
		return te.RetryAfter, true
	}
	return 0, false
}

// accessRank orders access bases: OA first, then licensed APIs, then
// institutional, then manual (stack plan resolver order).
var accessRank = map[string]int{
	AccessOpen:          0,
	AccessLicensedAPI:   1,
	AccessInstitutional: 2,
	AccessManual:        3,
}

// versionRank orders versions under a preference. desired "any" prefers
// published, then accepted, then preprint.
func versionRank(desired, v string) int {
	order := []string{VersionPublished, VersionAccepted, VersionPreprint, VersionUnknown}
	switch desired {
	case VersionAccepted:
		order = []string{VersionAccepted, VersionPublished, VersionPreprint, VersionUnknown}
	case VersionPreprint:
		order = []string{VersionPreprint, VersionPublished, VersionAccepted, VersionUnknown}
	}
	for i, o := range order {
		if v == o {
			return i
		}
	}
	return len(order)
}

// sourceReliability orders sources by demonstrated payload quality; lower is
// better. Unlisted sources rank after listed ones, alphabetically stable.
var sourceReliability = map[string]int{
	"cache":            0,
	"arxiv":            1,
	"europepmc":        2,
	"unpaywall":        3,
	"openalex":         4,
	"openalex_content": 5,
	"core":             6,
	"crossref_tdm":     7,
}

// licenseRank prefers explicit licenses over unknown.
func licenseRank(l string) int {
	if l == "" || l == "unknown" {
		return 1
	}
	return 0
}

// confidenceBucket coarsens identity confidence so tiny float differences do
// not reorder candidates nondeterministically.
func confidenceBucket(c float64) int {
	switch {
	case c >= 0.9:
		return 0
	case c >= 0.7:
		return 1
	case c >= 0.5:
		return 2
	default:
		return 3
	}
}

// Rank sorts candidates deterministically per the plan's ranking tuple:
// identity confidence, access basis, version preference, directness+source
// reliability, license clarity, cost, then stable source/key tie-breakers.
// The returned evidence strings explain each candidate's tuple.
func Rank(desiredVersion string, cands []Candidate) ([]Candidate, []string) {
	type keyed struct {
		c     Candidate
		tuple [8]int
		cost  float64
	}
	ks := make([]keyed, 0, len(cands))
	for _, c := range cands {
		direct := 1
		if c.Direct {
			direct = 0
		}
		rel, ok := sourceReliability[c.Source]
		if !ok {
			rel = 100
		}
		access, ok := accessRank[c.AccessBasis]
		if !ok {
			access = 100
		}
		ks = append(ks, keyed{
			c: c,
			tuple: [8]int{
				confidenceBucket(c.IdentityConfidence),
				access,
				versionRank(desiredVersion, c.Version),
				direct,
				rel,
				licenseRank(c.ReuseLicense),
			},
			cost: c.CostUSD,
		})
	}
	sort.SliceStable(ks, func(i, j int) bool {
		a, b := ks[i], ks[j]
		for t := range 6 {
			if a.tuple[t] != b.tuple[t] {
				return a.tuple[t] < b.tuple[t]
			}
		}
		if a.cost != b.cost {
			return a.cost < b.cost
		}
		if a.c.Source != b.c.Source {
			return a.c.Source < b.c.Source
		}
		return a.c.Key() < b.c.Key()
	})
	out := make([]Candidate, len(ks))
	evidence := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.c
		evidence[i] = fmt.Sprintf("conf=%d access=%d version=%d direct=%d source=%d license=%d cost=%.2f",
			k.tuple[0], k.tuple[1], k.tuple[2], k.tuple[3], k.tuple[4], k.tuple[5], k.cost)
	}
	return out, evidence
}

// ValidateCandidate rejects malformed resolver output before it enters the
// pipeline (fail closed at the source boundary).
func ValidateCandidate(c Candidate) error {
	if c.Source == "" {
		return fmt.Errorf("candidate missing source")
	}
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
		return fmt.Errorf("candidate %s: URL must be absolute http(s)", c.Source)
	}
	for name, value := range c.RequestHeaders {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name+value, "\r\n") {
			return fmt.Errorf("candidate %s: invalid request header", c.Source)
		}
	}
	switch c.Version {
	case VersionPublished, VersionAccepted, VersionPreprint, VersionUnknown:
	default:
		return fmt.Errorf("candidate %s: invalid version %q", c.Source, c.Version)
	}
	if _, ok := accessRank[c.AccessBasis]; !ok {
		return fmt.Errorf("candidate %s: invalid access basis %q", c.Source, c.AccessBasis)
	}
	if c.ReuseLicense == "" {
		return fmt.Errorf("candidate %s: reuse license required (use \"unknown\")", c.Source)
	}
	if c.IdentityConfidence < 0 || c.IdentityConfidence > 1 {
		return fmt.Errorf("candidate %s: identity confidence out of range", c.Source)
	}
	if c.CostUSD < 0 {
		return fmt.Errorf("candidate %s: negative cost", c.Source)
	}
	return nil
}
