// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package publicationwatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"papio/internal/enrich"
)

func TestPublicationWatchReportsDOIMissingFromCrossref(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	id, _, _ := acquiredFixture(t, db, "preprint")
	const doi = "10.48550/arxiv.2301.08745"
	if _, err := db.DB().ExecContext(ctx, `UPDATE identifiers SET value=? WHERE kind='doi' AND work_request_id=(SELECT work_request_id FROM jobs WHERE id=?)`, doi, id); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/works/"+doi {
			t.Errorf("lookup path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	source := enrich.NewWithOptions(enrich.Options{BaseURL: server.URL + "/works"})
	s := NewService(db, source, nil, nil)
	watch, err := s.Add(ctx, AddInput{JobID: id, CadenceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	for run := range 2 {
		if _, err := s.Run(ctx, watch.ID); err == nil || !strings.Contains(err.Error(), "Crossref has no record") {
			t.Fatalf("run %d error = %v, want unsupported source", run, err)
		}
		var lastError string
		if err := db.DB().QueryRowContext(ctx, `SELECT last_error FROM publication_watches WHERE id=?`, watch.ID).Scan(&lastError); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(lastError, "Crossref has no record") {
			t.Fatalf("last_error = %q", lastError)
		}
	}
	// Acquisition sibling lookup remains optional: a missing record is not a
	// source outage and must not cause retries of an otherwise exhausted job.
	if siblings, err := source.VersionSiblings(ctx, doi); err != nil || len(siblings) != 0 {
		t.Fatalf("version siblings = %v, %v", siblings, err)
	}
}
