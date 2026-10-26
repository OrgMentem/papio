// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"papio/internal/resolver"
	"papio/internal/work"
)

func TestTitleLookupBodyFailureIsTemporary(t *testing.T) {
	for _, source := range []string{"crossref", "openalex"} {
		for _, truncated := range []bool{true, false} {
			name := source + "/invalid_json"
			if truncated {
				name = source + "/truncated_body"
			}
			t.Run(name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if truncated {
						w.Header().Set("Content-Length", "1000")
					}
					_, _ = io.WriteString(w, `{"message":`)
				}))
				defer server.Close()
				lookup := NewWithOptions(Options{BaseURL: server.URL}).Enrich
				if source == "openalex" {
					lookup = NewOpenAlexWithOptions(OpenAlexOptions{BaseURL: server.URL, ContactEmail: "test@example.org"}).Enrich
				}
				_, matched, err := lookup(context.Background(), work.Work{Title: "A paper", Authors: []string{"Smith"}, Year: 2024})
				if err == nil || matched {
					t.Fatalf("lookup = matched %v, error %v; want a failed lookup", matched, err)
				}
				if _, temporary := resolver.Temporary(err); temporary != truncated {
					t.Fatalf("temporary = %v, want %v for %v", temporary, truncated, err)
				}
			})
		}
	}
}
