// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/bench"
)

// A cohort work whose fixture cannot be loaded never ran. The report still
// prints it as an error row, but the command must exit non-zero: it used to
// exit 0, so a CI gate reading the status passed a benchmark that measured
// fewer works than the cohort named.
func TestBenchFailsWhenACohortWorkCouldNotRun(t *testing.T) {
	dir := t.TempDir()
	cohort := `{"schema_version":"papio-bench-cohort/1","id":"broken","works":[` +
		`{"key":"broken-fixture","request":{"doi":"10.1000/broken"},"expected_class":"autonomous_ready"}]}`
	cohortPath := filepath.Join(dir, "cohort.json")
	if err := os.WriteFile(cohortPath, []byte(cohort), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "fixtures"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "broken-fixture.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, jsonOutput := range []bool{false, true} {
		var out, errOut bytes.Buffer
		root := NewRoot(&out, &errOut)
		args := []string{"bench", "--cohort", cohortPath}
		if jsonOutput {
			args = append([]string{"--json"}, args...)
		}
		root.SetArgs(args)
		err := root.ExecuteContext(context.Background())
		if !errors.Is(err, errBenchWorksFailed) || !strings.Contains(err.Error(), "1 of 1") {
			t.Fatalf("json=%t: err = %v, want errBenchWorksFailed counting 1 of 1", jsonOutput, err)
		}
		if !jsonOutput {
			if !strings.Contains(out.String(), "broken-fixture\terror: ") || !strings.Contains(out.String(), "/ 1 works (1 could not run)") {
				t.Fatalf("text report lost the error row or counted it as measured:\n%s", out.String())
			}
			continue
		}
		var page struct {
			Results []bench.WorkResult `json:"results"`
		}
		if err := json.Unmarshal(out.Bytes(), &page); err != nil || len(page.Results) != 1 || page.Results[0].Error == "" {
			t.Fatalf("json report = %s (%v), want the error row", out.String(), err)
		}
	}
}

// A work with no fixture is a measured fixture_missing class, not a failure to
// run, so the run still succeeds.
func TestBenchSucceedsWhenAFixtureIsMissing(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewRoot(&out, &errOut)
	root.SetArgs([]string{"bench", "--cohort", "testdata/bench-conformance-cohort.json"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("bench: %v\n%s", err, out.String())
	}
}
