// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package openalex

import (
	"context"
	"net/http"
	"testing"

	"papio/internal/resolver"
	"papio/internal/work"
)

func TestCandidateAuthority(t *testing.T) {
	// doiRecord echoes the requested DOI; openalexRecord echoes the requested OpenAlex ID.
	const doiRecord = `{"id":"https://openalex.org/W1234567890","doi":"https://doi.org/10.1000/example","ids":{"openalex":"https://openalex.org/W1234567890","doi":"https://doi.org/10.1000/example"},"title":"Example","publication_year":2024,"authorships":[{"author":{"display_name":"A Author"}}],"open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/a.pdf","version":"publishedVersion","license":"cc-by"}}`
	const openalexRecord = `{"id":"https://openalex.org/W2741809807","ids":{"openalex":"https://openalex.org/W2741809807"},"title":"Shape Trust","publication_year":2024,"authorships":[{"author":{"display_name":"A Author"}}],"open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/b.pdf","version":"publishedVersion","license":"cc-by"}}`
	const titleRecord = `{"id":"https://openalex.org/W9999999999","doi":"https://doi.org/10.1000/other","ids":{"openalex":"https://openalex.org/W9999999999","doi":"https://doi.org/10.1000/other"},"title":"A Precise Title","publication_year":2020,"authorships":[{"author":{"display_name":"A Author"}}],"open_access":{"is_oa":true,"oa_status":"gold"},"best_oa_location":{"is_oa":true,"pdf_url":"https://example.org/c.pdf","version":"publishedVersion","license":"cc-by"}}`

	tests := []struct {
		name       string
		work       work.Work
		body       string
		isSearch   bool
		wantAuth   resolver.EvidenceAuthority
		wantProm   bool
		useSibling bool
	}{
		{
			name:     "exact DOI hit",
			work:     work.Work{DOI: "10.1000/example"},
			body:     doiRecord,
			wantAuth: resolver.AuthorityExactEcho,
			wantProm: true,
		},
		{
			name:     "exact OpenAlex ID hit",
			work:     work.Work{OpenAlex: "W2741809807"},
			body:     openalexRecord,
			wantAuth: resolver.AuthorityExactEcho,
			wantProm: true,
		},
		{
			name:     "title search hit",
			work:     work.Work{Title: "A Precise Title", Year: 2020, Authors: []string{"A Author"}},
			body:     `{"results":[` + titleRecord + `]}`,
			isSearch: true,
			wantAuth: resolver.AuthoritySearch,
			wantProm: false,
		},
		{
			name:       "sibling result",
			work:       canonicalWork(),
			body:       siblingSearchBody("How Explanations Shape Trust", 2022, "10.2139/ssrn.4020557", "Andrea Ferrario", "https://ssrn.example/paper.pdf"),
			useSibling: true,
			wantAuth:   resolver.AuthorityTypedRelation,
			wantProm:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.useSibling {
				r := siblingResolver(t, tc.body)
				// Seed memo exactly as the pipeline does: Resolve the canonical DOI first.
				if _, err := r.Resolve(context.Background(), work.Work{DOI: "10.1145/3531146.3533202"}); err != nil {
					t.Fatalf("seed Resolve: %v", err)
				}
				cands, err := r.ResolveSiblings(context.Background(), tc.work)
				if err != nil {
					t.Fatalf("ResolveSiblings: %v", err)
				}
				if len(cands) == 0 {
					t.Fatalf("no candidates")
				}
				for _, c := range cands {
					if c.Authority != tc.wantAuth {
						t.Fatalf("Authority = %q, want %q", c.Authority, tc.wantAuth)
					}
					if c.Authority.MayPromoteIdentity() != tc.wantProm {
						t.Fatalf("MayPromoteIdentity() = %v, want %v for %q", c.Authority.MayPromoteIdentity(), tc.wantProm, c.Authority)
					}
				}
				return
			}
			// Non-sibling paths use a direct Resolve.
			var capturedLookup string
			client := clientFunc(func(req *http.Request) (*http.Response, error) {
				// Record lookup type from URL.
				if req.URL.Query().Get("search") != "" {
					capturedLookup = "search"
				} else {
					capturedLookup = "singleton"
				}
				return responseFor(200, tc.body, nil), nil
			})
			r := NewWithOptions(Options{Client: client, ContactEmail: "contact@example.org", APIKey: "private-key", BaseURL: "https://api.test/works"})
			cands, err := r.Resolve(context.Background(), tc.work)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if len(cands) == 0 {
				t.Fatalf("no candidates (lookup=%s)", capturedLookup)
			}
			c := cands[0]
			if tc.isSearch && capturedLookup != "search" {
				t.Fatalf("expected title search request, got %q", capturedLookup)
			}
			if !tc.isSearch && capturedLookup == "search" {
				t.Fatalf("expected singleton request, got search")
			}
			if c.Authority != tc.wantAuth {
				t.Fatalf("Authority = %q, want %q", c.Authority, tc.wantAuth)
			}
			if c.Authority.MayPromoteIdentity() != tc.wantProm {
				t.Fatalf("MayPromoteIdentity() = %v, want %v for %q", c.Authority.MayPromoteIdentity(), tc.wantProm, c.Authority)
			}
		})
	}
}
