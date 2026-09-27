// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package app

import (
	"context"
	"net/http"
	"testing"

	"papio/internal/config"
	"papio/internal/fetch"
	"papio/internal/resolver"
	"papio/internal/work"
)

// A failed daemon fetch must persist its enumerated class and host in
// attempts.detail, so a challenge reads differently from a plain 403 in the
// store (I11-obs). Both cases below run the production downloader through
// the real fetchCandidates recording path.
func TestFetchFailureDetailPersistsKindAndHost(t *testing.T) {
	const softChallenge = `<html><head><title>Challenge</title>` +
		`<script src="/_fs-ch-abc123.js"></script></head>` +
		`<body><p>challenge</p></body></html>`
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		outcome    string
		httpStatus int
		detail     string
	}{
		{"soft-challenge", http.StatusOK, softChallenge,
			fetch.ClassInvalid, http.StatusOK, "*fetch.Error(challenge) host=papers.example.org final=papers.example.org"},
		{"plain-403", http.StatusForbidden, "<html><body>forbidden</body></html>",
			fetch.ClassInvalid, http.StatusForbidden, "*fetch.Error(http-403) host=papers.example.org final=papers.example.org"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dl, err := fetch.New(fetch.DefaultPolicy(), publicHostResolver{}, handlerTransport{
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/html; charset=UTF-8")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			svc, jobs := newTestService(t)
			svc.Resolvers = []ResolverEntry{{
				Adapter: &fakeResolver{name: "fixture", cands: []resolver.Candidate{{
					Source: "fixture", URL: "https://papers.example.org/paper.pdf?token=secret",
					ResolvedWork: work.Work{DOI: "10.1002/example"},
					Version:      resolver.VersionPublished, AccessBasis: resolver.AccessOpen, ReuseLicense: "unknown",
					ExpectedMIME: "application/pdf", Direct: true, IdentityConfidence: 1,
				}}},
				Policy: config.Source{Enabled: true},
			}}
			svc.Fetch = func(ctx context.Context, c resolver.Candidate, path string) (fetch.Result, error) {
				return dl.DownloadWithHeaders(ctx, c.URL, c.RequestHeaders, path)
			}
			svc.Validate = passValidation()

			row := processToEnd(t, svc, jobs, "wr_fetch_detail_"+tc.name)
			var outcome, detail string
			var httpStatus int
			if err := jobs.S.DB().QueryRowContext(context.Background(),
				`SELECT outcome, detail, http_status FROM attempts WHERE job_id = ? AND stage = 'fetch' ORDER BY id DESC LIMIT 1`,
				row.ID,
			).Scan(&outcome, &detail, &httpStatus); err != nil {
				t.Fatal(err)
			}
			if outcome != tc.outcome || detail != tc.detail || httpStatus != tc.httpStatus {
				t.Fatalf("attempt = %q/%q/%d, want %q/%q/%d",
					outcome, detail, httpStatus, tc.outcome, tc.detail, tc.httpStatus)
			}
		})
	}
}
