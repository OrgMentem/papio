// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package bench

import (
	"context"
	"strconv"
	"testing"
)

func workByKey(t *testing.T, report Report, key string) WorkResult {
	t.Helper()
	for _, w := range report.Works {
		if w.Key == key {
			return w
		}
	}
	t.Fatalf("no work %q in report %+v", key, report)
	return WorkResult{}
}

// TestRunProducesIncrementalHeadlineOverTestdataCohort is the hermetic,
// end-to-end proof required by dev/post-build-followups.md item 4: the
// comparative machinery, run over a fully-fixtured 2-work cohort, asserts a
// nonzero incremental_autonomous_ready delta.
func TestRunProducesIncrementalHeadlineOverTestdataCohort(t *testing.T) {
	cohort, err := LoadCohort("testdata/cohort.json")
	if err != nil {
		t.Fatalf("LoadCohort: %v", err)
	}
	report, err := Run(context.Background(), cohort, DirFixtureSet{Dir: "testdata/fixtures"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Works) != 2 {
		t.Fatalf("report.Works = %+v, want 2 rows", report.Works)
	}
	for _, w := range report.Works {
		if w.Error != "" {
			t.Fatalf("work %s errored: %s", w.Key, w.Error)
		}
	}

	delta := workByKey(t, report, "delta-open-access")
	if delta.BaselineClass != ClassReadyAfterHumanBoundary {
		t.Fatalf("delta-open-access baseline class = %s, want ready_after_human_boundary", delta.BaselineClass)
	}
	if delta.BaselineHumanEpisodes != 1 {
		t.Fatalf("delta-open-access baseline human episodes = %d, want 1", delta.BaselineHumanEpisodes)
	}
	if delta.BaselineRouteClass != "openurl_handoff" {
		t.Fatalf("delta-open-access baseline route class = %q, want openurl_handoff", delta.BaselineRouteClass)
	}
	if delta.CurrentClass != ClassAutonomousReady {
		t.Fatalf("delta-open-access current class = %s, want autonomous_ready", delta.CurrentClass)
	}
	if delta.CurrentHumanEpisodes != 0 {
		t.Fatalf("delta-open-access current human episodes = %d, want 0", delta.CurrentHumanEpisodes)
	}
	if delta.CurrentAcceptedSource != "semanticscholar" {
		t.Fatalf("delta-open-access current accepted source = %q, want semanticscholar", delta.CurrentAcceptedSource)
	}

	stable := workByKey(t, report, "delta-stable-oa")
	if stable.BaselineClass != ClassAutonomousReady || stable.CurrentClass != ClassAutonomousReady {
		t.Fatalf("delta-stable-oa classes = baseline %s current %s, want autonomous_ready/autonomous_ready", stable.BaselineClass, stable.CurrentClass)
	}
	if stable.BaselineAcceptedSource != "unpaywall" || stable.CurrentAcceptedSource != "unpaywall" {
		t.Fatalf("delta-stable-oa accepted source = baseline %q current %q, want unpaywall/unpaywall", stable.BaselineAcceptedSource, stable.CurrentAcceptedSource)
	}

	if got := report.IncrementalAutonomousReady(); got != 1 {
		t.Fatalf("IncrementalAutonomousReady() = %d, want 1 (only delta-open-access moved into the zero-human-ready bucket)", got)
	}
	if got, want := report.Headline(), "+1 / 2 works"; got != want {
		t.Fatalf("Headline() = %q, want %q", got, want)
	}
}

// TestRunReportsFixtureMissingForAnUnfixturedWork proves fixture_missing is
// a distinct state from honest_unavailable: a work the fixture set has never
// heard of must never silently run and land in the honest-failure bucket.
func TestRunReportsFixtureMissingForAnUnfixturedWork(t *testing.T) {
	cohort := Cohort{
		SchemaVersion: CohortSchemaVersion,
		ID:            "missing-fixture-cohort",
		Works: []Work{
			{Key: "no-such-fixture", Request: Request{DOI: "10.1000/no-such-fixture"}, ExpectedClass: AutonomousReady},
		},
	}
	report, err := Run(context.Background(), cohort, DirFixtureSet{Dir: "testdata/fixtures"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	w := workByKey(t, report, "no-such-fixture")
	if w.Error != "" {
		t.Fatalf("work errored: %s, want a clean fixture_missing row", w.Error)
	}
	if w.BaselineClass != ClassFixtureMissing || w.CurrentClass != ClassFixtureMissing {
		t.Fatalf("classes = baseline %s current %s, want fixture_missing/fixture_missing", w.BaselineClass, w.CurrentClass)
	}
}

// TestFieldCohortReportsFixtureMissingForEveryWork is the acceptance-level
// regression for the real field cohort: as authored, dev/bench/field-2026-07-21.json
// ships with no fixtures, so every row must honestly report fixture_missing
// rather than a fabricated or accidentally-network-derived outcome.
func TestFieldCohortReportsFixtureMissingForEveryWork(t *testing.T) {
	const path = "../../dev/bench/field-2026-07-21.json"
	cohort, err := LoadCohort(path)
	if err != nil {
		t.Fatalf("LoadCohort(%s): %v", path, err)
	}
	if len(cohort.Works) == 0 {
		t.Fatalf("field cohort has no works")
	}
	report, err := Run(context.Background(), cohort, DirFixtureSet{Dir: FixturesDirFor(path)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Works) != len(cohort.Works) {
		t.Fatalf("report has %d rows, cohort has %d works", len(report.Works), len(cohort.Works))
	}
	for _, w := range report.Works {
		if w.Error != "" {
			t.Fatalf("work %s errored: %s", w.Key, w.Error)
		}
		if w.BaselineClass != ClassFixtureMissing || w.CurrentClass != ClassFixtureMissing {
			t.Fatalf("work %s classes = baseline %s current %s, want fixture_missing/fixture_missing (no fixtures exist for the field cohort yet)", w.Key, w.BaselineClass, w.CurrentClass)
		}
	}
	want := "+0 / " + strconv.Itoa(len(cohort.Works)) + " works"
	if got := report.Headline(); got != want {
		t.Fatalf("Headline() = %q, want %q", got, want)
	}
}
