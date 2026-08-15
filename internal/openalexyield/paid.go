// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package openalexyield

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// HTTPClient is the injected HTTP dependency, matching the shape every other
// OpenAlex integration in this tree uses
// (internal/resolvers/openalex.HTTPClient, internal/enrich.HTTPClient) so the
// paid comparison is swappable and testable the same way its production
// counterparts are — never a bare, zero-value *http.Client of its own.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// MaxSample bounds the paid comparison's sample size. Every title is queried
// once per shape, so the total spend is len(AllShapes) * sample *
// CreditsPerTitleSearch; this bound keeps an operator's mistyped --sample
// from an unbounded credit spend even after they have already opted in.
const MaxSample = 200

// ErrConfirmationRequired is returned by Run when cfg.Confirm is false. The
// paid comparison must never spend a credit without an explicit, informed
// opt-in.
var ErrConfirmationRequired = errors.New("openalexyield: paid comparison requires explicit confirmation (run with --confirm-spend after reviewing the printed cost)")

// shape is one of the three query shapes dev/active/openalex-spend-remainders.md
// item 0 asks to compare, all priced identically (10 credits/call — pricing
// is per request, not per row).
type shape struct {
	// label is both the human-readable name and the QueryShape id used to key
	// ShapeResult.Shape.
	label string
	// query builds this shape's query parameters for one title.
	query func(title string) url.Values
}

// searchShape builds the current/legacy "search=<title>&per_page=<n>" shape:
// OpenAlex's broad relevance search over title+abstract+full text, exactly
// what internal/resolvers/openalex.go's lookupURL sends today (perPage=10) —
// see openalex.go's defaultSearchPageSize.
func searchShape(label string, perPage int) shape {
	return shape{
		label: label,
		query: func(title string) url.Values {
			v := url.Values{}
			v.Set("search", title)
			v.Set("per_page", strconv.Itoa(perPage))
			return v
		},
	}
}

// titleSearchShape builds the title-scoped "filter=title.search:<title>"
// shape: narrower than a bare search= (title only, not abstract/full text),
// currently marked deprecated by OpenAlex but still live.
func titleSearchShape(title string) url.Values {
	v := url.Values{}
	v.Set("filter", "title.search:"+title)
	return v
}

// AllShapes are the three shapes compared, in the order item 0 lists them:
// the current shape first (the baseline being questioned), then the two
// candidates.
var AllShapes = []shape{
	searchShape("search=<title>&per_page=10 (current shape)", 10),
	searchShape("search=<title>&per_page=100", 100),
	{label: "filter=title.search:<title> (deprecated, still live)", query: titleSearchShape},
}

// ComparisonPlan is what a caller MUST see, printed in full, before Run ever
// issues a request: the exact shapes, the sample size, and the total credit
// cost about to be spent.
type ComparisonPlan struct {
	Shapes         []string
	SampleSize     int
	CreditsPerCall int
	TotalCalls     int
	TotalCredits   int
}

// Plan computes the cost preview for sampleSize titles across every shape in
// AllShapes. It performs no I/O and is safe to call and print unconditionally
// — Run is the only function that can spend a credit, and only when
// cfg.Confirm is true.
func Plan(sampleSize int) ComparisonPlan {
	labels := make([]string, len(AllShapes))
	for i, s := range AllShapes {
		labels[i] = s.label
	}
	calls := len(AllShapes) * sampleSize
	return ComparisonPlan{
		Shapes: labels, SampleSize: sampleSize, CreditsPerCall: CreditsPerTitleSearch,
		TotalCalls: calls, TotalCredits: calls * CreditsPerTitleSearch,
	}
}

// Render prints the plan for the operator to read before deciding whether to
// pass --confirm-spend.
func (p ComparisonPlan) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "openalex-yield paid comparison — NOT YET RUN\n")
	fmt.Fprintf(&b, "query shapes to compare (%d):\n", len(p.Shapes))
	for _, s := range p.Shapes {
		fmt.Fprintf(&b, "  - %s\n", s)
	}
	fmt.Fprintf(&b, "sample size:   %d titles drawn from your local library\n", p.SampleSize)
	fmt.Fprintf(&b, "cost per call: %d credits (pricing is per request, identical across shapes)\n", p.CreditsPerCall)
	fmt.Fprintf(&b, "total calls:   %d (%d shapes x %d titles)\n", p.TotalCalls, len(p.Shapes), p.SampleSize)
	fmt.Fprintf(&b, "TOTAL CREDITS TO SPEND: %d\n", p.TotalCredits)
	fmt.Fprintf(&b, "\nRe-run with --confirm-spend to actually spend these credits.\n")
	return b.String()
}

// TitleSample is one work drawn from the local library's own submitted
// bibliography to test the three shapes against a real title, not a
// synthetic example.
type TitleSample struct {
	Title string
	Year  int
}

// SampleTitles draws up to n distinct, non-empty titles at random from
// work_requests in the local store. It makes no provider requests.
func SampleTitles(ctx context.Context, db *sql.DB, n int) ([]TitleSample, error) {
	if n <= 0 {
		return nil, fmt.Errorf("openalexyield: sample size must be positive, got %d", n)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT title, COALESCE(year, 0) FROM work_requests
		 WHERE title IS NOT NULL AND title != ''
		 ORDER BY RANDOM() LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("sampling titles from the local library: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TitleSample
	for rows.Next() {
		var t TitleSample
		if err := rows.Scan(&t.Title, &t.Year); err != nil {
			return nil, fmt.Errorf("scanning sampled title: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ComparisonConfig configures the opt-in, credit-spending shape comparison.
type ComparisonConfig struct {
	// Confirm is the explicit, required opt-in. Run refuses without it.
	Confirm bool
	// Sample bounds how many titles each shape is tried against; must be in
	// [1, MaxSample].
	Sample int
	// Client is the injected HTTP dependency — go through the same wiring
	// conventions as internal/resolvers/openalex and internal/enrich, never a
	// bare http.Client of this package's own construction.
	Client HTTPClient
	// ContactEmail is sent as OpenAlex's polite-pool "mailto" parameter.
	ContactEmail string
	// APIKey, when set, is sent as "api_key". Leave empty to sample under the
	// keyless tier.
	APIKey string
	// BaseURL overrides the works endpoint (tests only); defaults to
	// https://api.openalex.org/works.
	BaseURL string
}

const defaultWorksBaseURL = "https://api.openalex.org/works"

// searchResponse decodes only what Run needs: each result's own title, to
// decide whether the shape actually returned the requested work.
type searchResponse struct {
	Results []struct {
		Title string `json:"title"`
	} `json:"results"`
}

// ShapeResult tallies one shape's outcome across the sample.
type ShapeResult struct {
	Shape             string
	Requests          int
	Credits           int
	ExactTitleMatches int
	Errors            int
}

// ComparisonReport is the paid comparison's result: the plan that was
// executed, plus each shape's outcome.
type ComparisonReport struct {
	Plan    ComparisonPlan
	Results []ShapeResult
}

// exactNormalizedTitleMatch reports whether any result's own title matches
// requested, case-folded and whitespace-collapsed — the same comparison
// internal/resolvers/openalex.go's normalizeTitle uses for its acceptance
// predicate, so this measures the shape under the SAME acceptance rule
// production already applies (dev/active/openalex-spend-remainders.md item 0:
// "papio then applies its own exact-normalized-title test").
func exactNormalizedTitleMatch(resp searchResponse, requested string) bool {
	want := normalizeYieldTitle(requested)
	if want == "" {
		return false
	}
	for _, r := range resp.Results {
		if normalizeYieldTitle(r.Title) == want {
			return true
		}
	}
	return false
}

func normalizeYieldTitle(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

// Run executes the opt-in, credit-spending three-shape comparison. It refuses
// outright — no request, no credit spent — unless cfg.Confirm is true and
// cfg.Sample is within bounds.
func Run(ctx context.Context, cfg ComparisonConfig, titles []TitleSample) (ComparisonReport, error) {
	if !cfg.Confirm {
		return ComparisonReport{}, ErrConfirmationRequired
	}
	if cfg.Sample <= 0 || cfg.Sample > MaxSample {
		return ComparisonReport{}, fmt.Errorf("openalexyield: sample size must be between 1 and %d, got %d", MaxSample, cfg.Sample)
	}
	if cfg.Client == nil {
		return ComparisonReport{}, errors.New("openalexyield: no HTTP client configured for the paid comparison")
	}
	if strings.TrimSpace(cfg.ContactEmail) == "" {
		return ComparisonReport{}, errors.New("openalexyield: a contact email is required for OpenAlex's polite pool")
	}
	if len(titles) > cfg.Sample {
		titles = titles[:cfg.Sample]
	}
	if len(titles) == 0 {
		return ComparisonReport{}, errors.New("openalexyield: no titles available to sample from the local library")
	}

	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = defaultWorksBaseURL
	}

	plan := Plan(len(titles))
	report := ComparisonReport{Plan: plan}
	for _, s := range AllShapes {
		result := ShapeResult{Shape: s.label}
		for _, t := range titles {
			resp, err := runOneCall(ctx, cfg, base, s, t.Title)
			result.Requests++
			result.Credits += CreditsPerTitleSearch
			if err != nil {
				result.Errors++
				continue
			}
			if exactNormalizedTitleMatch(resp, t.Title) {
				result.ExactTitleMatches++
			}
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func runOneCall(ctx context.Context, cfg ComparisonConfig, base string, s shape, title string) (searchResponse, error) {
	u, err := url.Parse(base)
	if err != nil {
		return searchResponse{}, fmt.Errorf("invalid base URL %q: %w", base, err)
	}
	query := s.query(title)
	query.Set("mailto", cfg.ContactEmail)
	if cfg.APIKey != "" {
		query.Set("api_key", cfg.APIKey)
	}
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return searchResponse{}, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := cfg.Client.Do(req)
	if err != nil {
		return searchResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return searchResponse{}, fmt.Errorf("openalex: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return searchResponse{}, err
	}
	var out searchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return searchResponse{}, fmt.Errorf("openalex: invalid response: %w", err)
	}
	return out, nil
}

// Render renders the executed comparison as aligned plain text.
func (r ComparisonReport) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "openalex-yield paid comparison — %d titles sampled, %d total credits spent\n\n",
		r.Plan.SampleSize, r.totalCreditsSpent())
	fmt.Fprintf(&b, "shape\trequests\tcredits\texact-title matches\terrors\n")
	for _, res := range r.Results {
		fmt.Fprintf(&b, "%s\t%d\t%d\t%d\t%d\n", res.Shape, res.Requests, res.Credits, res.ExactTitleMatches, res.Errors)
	}
	return b.String()
}

func (r ComparisonReport) totalCreditsSpent() int {
	total := 0
	for _, res := range r.Results {
		total += res.Credits
	}
	return total
}
