// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package resolvers_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"papio/internal/resolver"
	"papio/internal/resolvers/arxiv"
	"papio/internal/resolvers/core"
	"papio/internal/resolvers/crossreftdm"
	"papio/internal/resolvers/europepmc"
	"papio/internal/resolvers/openaire"
	"papio/internal/resolvers/openalex"
	"papio/internal/resolvers/openbooks"
	"papio/internal/resolvers/semanticscholar"
	"papio/internal/resolvers/unpaywall"
	"papio/internal/work"
)

type failedBody struct{ err error }

func (r failedBody) Read([]byte) (int, error) { return 0, r.err }

type bodyReadTransport struct{ err error }

func (c bodyReadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body io.Reader = strings.NewReader("<")
	if c.err != nil {
		body = io.MultiReader(body, failedBody(c))
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(body),
		Request:    req,
	}, nil
}

func TestResolverBodyReadFailuresAreTemporary(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"timeout", context.DeadlineExceeded},
		{"reset", syscall.ECONNRESET},
		{"truncated transport", io.ErrUnexpectedEOF},
		{"complete malformed response", nil},
	} {
		t.Run(failure.name, func(t *testing.T) {
			client := &http.Client{Transport: bodyReadTransport{err: failure.err}}
			doi := work.Work{DOI: "10.1000/example"}
			for _, source := range []struct {
				adapter resolver.Resolver
				work    work.Work
			}{
				{arxiv.NewWithOptions(arxiv.Options{Client: client}), work.Work{ArXiv: "2301.08745"}},
				{core.New(client, "key"), doi},
				{crossreftdm.New(client, "key"), doi},
				{europepmc.NewWithOptions(europepmc.Options{Client: client}), doi},
				{openaire.NewWithOptions(openaire.Options{Client: client}), doi},
				{openalex.New(client, "contact@example.org"), doi},
				{openbooks.NewWithOptions(openbooks.Options{Client: client, DSpaceServers: []string{"https://repository.example/server"}, RequestGap: -1}), work.Work{ISBN: "9780262033848"}},
				{semanticscholar.NewWithOptions(semanticscholar.Options{Client: client}), doi},
				{unpaywall.NewWithOptions(unpaywall.Options{Client: client, ContactEmail: "contact@example.org"}), doi},
			} {
				t.Run(source.adapter.Name(), func(t *testing.T) {
					candidates, err := source.adapter.Resolve(context.Background(), source.work)
					if err == nil || len(candidates) != 0 {
						t.Fatalf("Resolve = (%+v, %v), want no candidates and an error", candidates, err)
					}
					if _, temporary := resolver.Temporary(err); temporary != (failure.err != nil) {
						t.Fatalf("Temporary(%v) = %v, want %v", err, temporary, failure.err != nil)
					}
					if failure.err != nil && !errors.Is(err, failure.err) {
						t.Fatalf("error %v does not preserve body failure %v", err, failure.err)
					}
				})
			}
		})
	}
}
