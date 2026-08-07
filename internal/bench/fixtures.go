// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bench

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
)

// FixtureSet resolves per-work HTTP/adoption fixtures for a hermetic run. A
// work absent from the set (the second return false) runs as
// ClassFixtureMissing: the runner never submits a job for it, so it never
// touches even the fixture-backed transport.
type FixtureSet interface {
	Lookup(workKey string) (WorkFixture, bool, error)
}

// SourceResponse is one canned HTTP response a fixture-backed resolver
// receives for every request it makes while resolving one work. Real
// resolver adapters run unmodified against it — this stands in only for
// their transport, the same seam internal/resolvers/*/*_test.go's httptest
// servers use — so the candidates a source produces come from parsing a
// real response wire format, never a hand-rolled candidate struct.
type SourceResponse struct {
	// Status defaults to 200 when zero.
	Status int    `json:"status,omitempty"`
	Body   string `json:"body"`
}

// WorkFixture is everything a hermetic run needs to resolve one cohort work
// without leaving the process. A source absent from Sources answers every
// request with HTTP 404 — every resolver bench wires treats 404 as "not
// found" (zero candidates, no error; see fixtures_test.go), so a fixture
// only needs to name the sources it wants to answer positively.
type WorkFixture struct {
	Sources map[string]SourceResponse `json:"sources"`
	// Adopt scripts the one human gesture this bench version simulates: if
	// the job parks on a blocking human action, the runner calls the same
	// Service.AdoptDownload entrypoint the browser bridge calls in
	// production, handing it fixture PDF bytes — simulating "a human
	// completed the handoff and reported a valid file" without a browser.
	// It is the ONLY human-boundary resolution the runner scripts; a job
	// that parks with Adopt false stays parked, and the run reports that as
	// an error for the work rather than guessing an outcome.
	Adopt bool `json:"adopt,omitempty"`
}

// FixturesDirFor returns the fixture directory bench uses by default for a
// cohort file: a sibling "fixtures" directory next to the cohort document.
func FixturesDirFor(cohortPath string) string {
	return filepath.Join(filepath.Dir(cohortPath), "fixtures")
}

// DirFixtureSet loads one WorkFixture per work from "<Dir>/<work key>.json".
// A missing file is "no fixture" (Lookup's ok return is false); a file that
// exists but fails to strictly decode is a fixture authoring error, surfaced
// as Lookup's error return rather than silently degrading to "missing" —
// fixture_missing must mean "nobody wrote one," never "somebody wrote one
// wrong."
type DirFixtureSet struct {
	Dir string
}

// Lookup implements FixtureSet.
func (d DirFixtureSet) Lookup(workKey string) (WorkFixture, bool, error) {
	path := filepath.Join(d.Dir, workKey+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return WorkFixture{}, false, nil
		}
		return WorkFixture{}, false, fmt.Errorf("bench: reading fixture %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var wf WorkFixture
	if err := dec.Decode(&wf); err != nil {
		return WorkFixture{}, false, fmt.Errorf("bench: decoding fixture %s: %w", path, err)
	}
	return wf, true, nil
}

// sourceRig serves one httptest.Server per resolver source name, answering
// every request with whatever the currently-selected work's fixture holds
// for that source name (or 404 when it holds nothing). One rig is reused
// across every work and both overlays of one Run: setWork swaps the
// currently-answered work between sequential, single-threaded calls, so a
// source's BaseURL never changes mid-process — only what it answers.
type sourceRig struct {
	mu      sync.Mutex
	current WorkFixture
	servers map[string]*httptest.Server
}

func newSourceRig(sources []string) *sourceRig {
	rig := &sourceRig{servers: make(map[string]*httptest.Server, len(sources))}
	for _, name := range sources {
		name := name
		rig.servers[name] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			rig.mu.Lock()
			resp, ok := rig.current.Sources[name]
			rig.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			status := resp.Status
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(resp.Body))
		}))
	}
	return rig
}

func (r *sourceRig) setWork(wf WorkFixture) {
	r.mu.Lock()
	r.current = wf
	r.mu.Unlock()
}

func (r *sourceRig) baseURL(source string) string {
	if s := r.servers[source]; s != nil {
		return s.URL
	}
	return ""
}

func (r *sourceRig) close() {
	for _, s := range r.servers {
		s.Close()
	}
}
