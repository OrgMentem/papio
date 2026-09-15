// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bench

import (
	"strings"
	"testing"
)

func validCohortJSON() string {
	return `{
		"schema_version": "papio-bench-cohort/1",
		"id": "test-cohort",
		"works": [
			{"key": "work-a", "request": {"doi": "10.1000/a"}, "expected_class": "autonomous_ready"}
		]
	}`
}

func TestDecodeCohortAcceptsAValidDocument(t *testing.T) {
	c, err := DecodeCohort(strings.NewReader(validCohortJSON()))
	if err != nil {
		t.Fatalf("DecodeCohort: %v", err)
	}
	if c.ID != "test-cohort" || len(c.Works) != 1 || c.Works[0].Key != "work-a" {
		t.Fatalf("decoded cohort = %+v", c)
	}
}

func TestDecodeCohortRejectsUnknownFields(t *testing.T) {
	doc := `{
		"schema_version": "papio-bench-cohort/1",
		"id": "test-cohort",
		"works": [{"key": "a", "request": {"doi": "10.1000/a"}, "expected_class": "autonomous_ready"}],
		"unexpected_field": true
	}`
	if _, err := DecodeCohort(strings.NewReader(doc)); err == nil {
		t.Fatal("DecodeCohort with an unknown top-level field succeeded, want an error")
	}
}

func TestDecodeCohortRejectsUnknownRequestField(t *testing.T) {
	doc := `{
		"schema_version": "papio-bench-cohort/1",
		"id": "test-cohort",
		"works": [{"key": "a", "request": {"doi": "10.1000/a", "expected_provider": "unpaywall"}, "expected_class": "autonomous_ready"}]
	}`
	if _, err := DecodeCohort(strings.NewReader(doc)); err == nil {
		t.Fatal("DecodeCohort with an expected_provider field succeeded, want an error — a request must never carry an expected provider or route")
	}
}

func TestDecodeCohortRejectsWrongSchemaVersion(t *testing.T) {
	doc := `{"schema_version": "papio-bench-cohort/2", "id": "x", "works": [{"key": "a", "request": {"doi": "10.1000/a"}, "expected_class": "autonomous_ready"}]}`
	if _, err := DecodeCohort(strings.NewReader(doc)); err == nil {
		t.Fatal("DecodeCohort with the wrong schema_version succeeded, want an error")
	}
}

func TestDecodeCohortRejectsEmptyID(t *testing.T) {
	doc := `{"schema_version": "papio-bench-cohort/1", "id": "", "works": [{"key": "a", "request": {"doi": "10.1000/a"}, "expected_class": "autonomous_ready"}]}`
	if _, err := DecodeCohort(strings.NewReader(doc)); err == nil {
		t.Fatal("DecodeCohort with an empty id succeeded, want an error")
	}
}

func TestDecodeCohortRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{
			name: "no works",
			doc:  `{"schema_version": "papio-bench-cohort/1", "id": "x", "works": []}`,
		},
		{
			name: "duplicate keys",
			doc: `{"schema_version": "papio-bench-cohort/1", "id": "x", "works": [
		{"key": "a", "request": {"doi": "10.1000/a"}, "expected_class": "autonomous_ready"},
		{"key": "a", "request": {"doi": "10.1000/b"}, "expected_class": "autonomous_ready"}
	]}`,
		},
		{
			name: "request with no identity",
			doc: `{"schema_version": "papio-bench-cohort/1", "id": "x", "works": [
		{"key": "a", "request": {}, "expected_class": "autonomous_ready"}
	]}`,
		},
		{
			name: "unknown expected class",
			doc: `{"schema_version": "papio-bench-cohort/1", "id": "x", "works": [
		{"key": "a", "request": {"doi": "10.1000/a"}, "expected_class": "probably_fine"}
	]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeCohort(strings.NewReader(tc.doc)); err == nil {
				t.Fatalf("DecodeCohort(%s) succeeded, want an error", tc.name)
			}
		})
	}
}

func TestDecodeCohortAcceptsEveryClosedExpectedClass(t *testing.T) {
	for _, class := range []ExpectedClass{AutonomousReady, ReadyAfterHumanBoundary, HonestUnavailable, IdentityReview} {
		doc := `{"schema_version": "papio-bench-cohort/1", "id": "x", "works": [
			{"key": "a", "request": {"doi": "10.1000/a"}, "expected_class": "` + string(class) + `"}
		]}`
		if _, err := DecodeCohort(strings.NewReader(doc)); err != nil {
			t.Fatalf("DecodeCohort with expected_class %q: %v", class, err)
		}
	}
}

func TestDecodeCohortRejectsTrailingContent(t *testing.T) {
	doc := validCohortJSON() + `{"garbage": true}`
	if _, err := DecodeCohort(strings.NewReader(doc)); err == nil {
		t.Fatal("DecodeCohort with trailing content succeeded, want an error")
	}
}

func TestLoadCohortRejectsMissingFile(t *testing.T) {
	if _, err := LoadCohort("testdata/does-not-exist.json"); err == nil {
		t.Fatal("LoadCohort of a missing file succeeded, want an error")
	}
}
