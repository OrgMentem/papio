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
	"math"
	"net/url"
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

// EvidenceAuthority names how strongly a candidate's own metadata is tied to
// the submitted canonical work. The zero value is the weakest, so an untagged
// resolver can create candidates and never promote identity.
type EvidenceAuthority string

const (
	AuthoritySearch        EvidenceAuthority = ""               // search/routing evidence: candidates only
	AuthorityTypedRelation EvidenceAuthority = "typed_relation" // a typed edge to a different work
	AuthorityExactEcho     EvidenceAuthority = "exact_echo"     // response echoes a requested identifier
)

// MayPromoteIdentity reports whether this authority class may mutate canonical
// identity metadata before artifact validation.
func (a EvidenceAuthority) MayPromoteIdentity() bool { return a == AuthorityExactEcho }

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
	// Authority names how strongly this candidate's metadata is tied to the
	// submitted canonical work. The zero value (AuthoritySearch) is fail-closed:
	// an untagged or search-derived candidate never promotes identity.
	Authority EvidenceAuthority
	// ISBNBinding is set only by a source whose own catalogue record for this
	// file lists the requested ISBN (open_books). It is in-memory evidence
	// for PDF validation, never persisted and never an identifier: it lets a
	// whole-book file be judged against the record's title when the requester
	// gave an ISBN and no title. Nil for every other source.
	ISBNBinding *ISBNBinding
}

// ISBNBinding is a repository's statement that its record for ONE file lists
// ISBN. The ISBN is the requested one (the source compares before it emits a
// candidate), Title is the record's own title, and URL is the file the record
// holds. The statement is about those bytes and no others: a candidate derived
// from this one (a different URL) does not inherit it, see BoundISBN.
type ISBNBinding struct {
	ISBN  string
	Title string
	URL   string
}

// BoundISBN returns the candidate's ISBN binding only while it still describes
// the candidate's own URL. A copy of the candidate that points somewhere else
// (landing-page recovery swaps in a citation_pdf_url) carries a binding about a
// file it no longer names, and gets none.
func (c Candidate) BoundISBN() *ISBNBinding {
	if c.ISBNBinding == nil || c.ISBNBinding.URL == "" || c.ISBNBinding.URL != c.URL {
		return nil
	}
	return c.ISBNBinding
}

// Key is a stable dedupe key for the candidate URL (hash, not the URL itself).
func (c Candidate) Key() string {
	sum := sha256.Sum256([]byte(c.Source + "\x00" + c.URL))
	return hex.EncodeToString(sum[:16])
}

// Upgrade evidence markers. UpgradeHTTP appends them to Candidate.Evidence,
// and candidate ingestion carries them into the persisted rank evidence, so
// the upgrade stays visible where candidate provenance is already recorded.
const (
	// UpgradeEvidenceURL marks a candidate URL upgraded http -> https.
	UpgradeEvidenceURL = "upgraded=url:http-to-https"
	// UpgradeEvidenceLanding marks a candidate landing URL upgraded http -> https.
	UpgradeEvidenceLanding = "upgraded=landing:http-to-https"
)

// UpgradeHTTP rewrites an http:// candidate URL (and landing URL) to its
// https:// form. The daemon refuses the http scheme before any request, so an
// http candidate as produced is unfetchable; the https form is tried instead
// and the http form is never fetched. Validation of the upgraded URL stays
// where it is: the identity gate still accepts or rejects the served bytes,
// and one https form in the measurement served a different work and was
// correctly rejected.
//
// Only absolute http URLs without userinfo are upgraded. Anything else
// (https, unparseable, userinfo, relative) is left untouched and keeps its
// existing fate. Key and redaction semantics follow automatically: both are
// computed from the final URL at ingestion. It returns the candidate and
// whether either field changed.
func UpgradeHTTP(c Candidate) (Candidate, bool) {
	upgraded := false
	if u, ok := upgradeHTTPScheme(c.URL); ok {
		c.URL = u
		c.Evidence = append(c.Evidence, UpgradeEvidenceURL)
		upgraded = true
	}
	if u, ok := upgradeHTTPScheme(c.Landing); ok {
		c.Landing = u
		c.Evidence = append(c.Evidence, UpgradeEvidenceLanding)
		upgraded = true
	}
	return c, upgraded
}

// upgradeHTTPScheme returns the https form of raw when raw is an absolute
// http URL without userinfo, and false otherwise. Scheme comparison is
// case-insensitive; the rewrite sets the canonical lowercase form.
func upgradeHTTPScheme(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "http") || u.Host == "" || u.User != nil {
		return "", false
	}
	u.Scheme = "https"
	return u.String(), true
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

// ErrNoSearchBasis reports a sibling lookup that made no request at all:
// the adapter had neither a cached canonical record nor caller-supplied
// metadata to search on. Callers must not charge it as a performed call.
var ErrNoSearchBasis = errors.New("resolver: sibling lookup has no title to search on")

// ErrNotApplicable reports an enricher that declined before making any
// request: the work is outside what it can act on (wrong identifiers,
// missing metadata). Callers must not charge it as a performed call.
var ErrNotApplicable = errors.New("resolver: enricher not applicable to this work")

type anonymousCredentialsKey struct{}

// WithAnonymousCredentials marks a call that must reach the provider without
// the configured API key: the keyed identity's own daily-quota signal says it
// is spent, and the keyless tier is a separate, separately-budgeted identity.
// Adapters read it once per entry point and thread it to URL construction.
func WithAnonymousCredentials(ctx context.Context) context.Context {
	return context.WithValue(ctx, anonymousCredentialsKey{}, true)
}

// AnonymousCredentials reports whether this call must omit its API key.
func AnonymousCredentials(ctx context.Context) bool {
	v, _ := ctx.Value(anonymousCredentialsKey{}).(bool)
	return v
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
	"cache":           0,
	"arxiv":           1,
	"europepmc":       2,
	"unpaywall":       3,
	"openalex":        4,
	"semanticscholar": 5,
	"core":            6,
	"crossref_tdm":    7,
	"openaire":        8,
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
	parsed, parseErr := url.Parse(c.URL)
	if parseErr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
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
	if math.IsNaN(c.IdentityConfidence) || math.IsInf(c.IdentityConfidence, 0) || c.IdentityConfidence < 0 || c.IdentityConfidence > 1 {
		return fmt.Errorf("candidate %s: identity confidence out of range", c.Source)
	}
	if math.IsNaN(c.CostUSD) || math.IsInf(c.CostUSD, 0) || c.CostUSD < 0 {
		return fmt.Errorf("candidate %s: negative cost", c.Source)
	}
	if b := c.ISBNBinding; b != nil && (strings.TrimSpace(b.ISBN) == "" || strings.TrimSpace(b.Title) == "" || b.URL != c.URL) {
		return fmt.Errorf("candidate %s: ISBN binding needs an ISBN and a title, and must name the candidate's own URL", c.Source)
	}
	return nil
}
