// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package bench runs papio's hermetic comparative acquisition benchmark: the
// same cohort of works acquired twice — once under a baseline resolver
// overlay, once under the current (full) resolver set — so a resolver
// change shows up as a measured delta instead of a single absolute number
// nobody has a baseline for. See dev/post-build-followups.md item 4.
//
// Every run is hermetic: an ephemeral temp-dir SQLite store and artifact
// cache, hooks/notifications/Zotio/browser left unconfigured (their nil
// zero values are "off"), and resolvers wired to injected HTTP fixtures —
// never the network, never the caller's real config or daemon. See runner.go.
package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// CohortSchemaVersion is the only schema_version DecodeCohort accepts.
const CohortSchemaVersion = "papio-bench-cohort/1"

// ExpectedClass is the closed set of outcomes a cohort work is graded
// against. It is deliberately never a provider or route name: a cohort
// records what a human judge expects the ACQUISITION to look like, not
// which mechanism should produce it, so a comparative run stays free to
// route through whichever source it wants.
type ExpectedClass string

// The closed expected_class enum. A cohort document with any other value
// fails DecodeCohort's validation rather than silently degrading grading.
const (
	AutonomousReady         ExpectedClass = "autonomous_ready"
	ReadyAfterHumanBoundary ExpectedClass = "ready_after_human_boundary"
	HonestUnavailable       ExpectedClass = "honest_unavailable"
	IdentityReview          ExpectedClass = "identity_review"
)

func (c ExpectedClass) valid() bool {
	switch c {
	case AutonomousReady, ReadyAfterHumanBoundary, HonestUnavailable, IdentityReview:
		return true
	}
	return false
}

// Request is the identity evidence a cohort work carries into acquisition —
// the same kind of evidence acquire/search already accept, and nothing
// else: no expected provider, no expected route.
type Request struct {
	DOI     string   `json:"doi,omitempty"`
	ArXiv   string   `json:"arxiv,omitempty"`
	PMID    string   `json:"pmid,omitempty"`
	Title   string   `json:"title,omitempty"`
	Authors []string `json:"authors,omitempty"`
	Year    int      `json:"year,omitempty"`
}

func (r Request) hasIdentity() bool {
	return r.DOI != "" || r.ArXiv != "" || r.PMID != "" || r.Title != ""
}

// Work is one cohort entry: a request plus the human-judged outcome it is
// graded against.
type Work struct {
	Key           string        `json:"key"`
	Request       Request       `json:"request"`
	ExpectedClass ExpectedClass `json:"expected_class"`
	// Source is free-text provenance for the judgement above — a field
	// report finding, a manual triage note. Bench never reads it; it exists
	// so the cohort file carries its own paper trail.
	Source string `json:"source,omitempty"`
}

// Cohort is one papio-bench-cohort/1 document.
type Cohort struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	// Description is free-text cohort-level provenance, parallel to
	// Work.Source.
	Description string `json:"description,omitempty"`
	Works       []Work `json:"works"`
}

// DecodeCohort strictly parses and validates one cohort document. Strict
// decode plus a closed expected_class enum: a cohort with a typo'd field or
// one this binary predates fails closed instead of silently running a
// weaker benchmark than the file claims.
func DecodeCohort(r io.Reader) (Cohort, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var c Cohort
	if err := dec.Decode(&c); err != nil {
		return Cohort{}, fmt.Errorf("bench: decode cohort: %w", err)
	}
	if dec.More() {
		return Cohort{}, fmt.Errorf("bench: decode cohort: trailing content after the document")
	}
	if err := c.Validate(); err != nil {
		return Cohort{}, err
	}
	return c, nil
}

// LoadCohort reads and decodes a cohort document from path.
func LoadCohort(path string) (Cohort, error) {
	f, err := os.Open(path)
	if err != nil {
		return Cohort{}, fmt.Errorf("bench: open cohort: %w", err)
	}
	defer f.Close()
	c, err := DecodeCohort(f)
	if err != nil {
		return Cohort{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Validate enforces the cohort schema's invariants: the exact schema
// version, a non-empty id, at least one work, unique non-empty keys, an
// identity-bearing request per work, and a closed expected_class.
func (c Cohort) Validate() error {
	if c.SchemaVersion != CohortSchemaVersion {
		return fmt.Errorf("bench: cohort schema_version %q, want %q", c.SchemaVersion, CohortSchemaVersion)
	}
	if c.ID == "" {
		return fmt.Errorf("bench: cohort id is required")
	}
	if len(c.Works) == 0 {
		return fmt.Errorf("bench: cohort %q has no works", c.ID)
	}
	seen := make(map[string]bool, len(c.Works))
	for i, w := range c.Works {
		if w.Key == "" {
			return fmt.Errorf("bench: cohort %q work[%d] has no key", c.ID, i)
		}
		if seen[w.Key] {
			return fmt.Errorf("bench: cohort %q work %q: duplicate key", c.ID, w.Key)
		}
		seen[w.Key] = true
		if !w.Request.hasIdentity() {
			return fmt.Errorf("bench: cohort %q work %q: request has no doi, arxiv, pmid, or title", c.ID, w.Key)
		}
		if !w.ExpectedClass.valid() {
			return fmt.Errorf("bench: cohort %q work %q: expected_class %q is not one of autonomous_ready, ready_after_human_boundary, honest_unavailable, identity_review", c.ID, w.Key, w.ExpectedClass)
		}
	}
	return nil
}
