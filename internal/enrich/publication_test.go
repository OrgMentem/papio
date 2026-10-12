// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPublicationRelationsRequireEchoedSourceAndDirectionalEdge(t *testing.T) {
	var source atomic.Value
	source.Store("10.1234/preprint")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"DOI":"` + source.Load().(string) + `","relation":{"is-preprint-of":[{"id-type":"doi","id":"10.1234/published"}],"has-version":[{"id-type":"doi","id":"10.1234/unrelated"}],"has-preprint":[{"id-type":"doi","id":"10.1234/earlier"}]}}}`))
	}))
	defer server.Close()
	e := NewWithOptions(Options{Client: server.Client(), BaseURL: server.URL})
	relations, err := e.PublicationRelations(context.Background(), "10.1234/preprint")
	if err != nil || len(relations) != 1 || relations[0].SourceDOI != "10.1234/preprint" || relations[0].TargetDOI != "10.1234/published" || relations[0].Type != "is-preprint-of" {
		t.Fatalf("relations: %+v %v", relations, err)
	}
	source.Store("10.1234/another")
	if _, err := e.PublicationRelations(context.Background(), "10.1234/preprint"); err == nil {
		t.Fatal("accepted another source DOI's publication relation")
	}
	source.Store("")
	if _, err := e.PublicationRelations(context.Background(), "10.1234/preprint"); err == nil {
		t.Fatal("accepted relation without source evidence")
	}
}
