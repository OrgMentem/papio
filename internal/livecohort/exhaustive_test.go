// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package livecohort

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"papio/internal/api"
	"papio/internal/job"
)

// TestClassifyCoversEveryStoppedState parses internal/job/job.go for the job
// states and asserts classify maps every stopped one — terminal, plus the
// two parking states — onto an outcome.
//
// It parses the source rather than listing the states here for the reason
// TestTerminalReasonVocabularyIsExhaustive does the same: a hand-kept list
// silently covers less as the vocabulary grows. That failure mode is live
// here. settledOutcome decides "has this stopped" from job.Terminal, and
// classify decides what the stop means. Adding a terminal state to job.go
// and not to classify makes settledOutcome call it stopped while classify
// refuses it, so the job is polled to its budget and reported as timed_out —
// a measurement that looks like a slow daemon and is really a stale binary.
func TestClassifyCoversEveryStoppedState(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "job", "job.go"))
	if err != nil {
		t.Fatalf("reading internal/job/job.go: %v", err)
	}
	states := regexp.MustCompile(`State\w+\s+(?:State\s*)?=\s*"([a-z_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(states) == 0 {
		t.Fatal("parsed no job states from internal/job/job.go")
	}

	stopped := 0
	for _, match := range states {
		state := match[1]
		if !job.Terminal(state) && state != job.StateAwaitingHuman && state != job.StateNeedsReview {
			continue
		}
		stopped++
		detail := &api.JobDetailV3{Job: &api.JobRow{Row: job.Row{ID: "job_x", State: state}}}
		outcome, err := classify(detail)
		if err != nil {
			t.Errorf("classify(%q) = %v; settledOutcome calls this state stopped, so a job in it "+
				"is polled to its budget and reported as timed_out instead of measured", state, err)
			continue
		}
		if outcome == "" {
			t.Errorf("classify(%q) returned an empty outcome", state)
		}
	}
	// ready, imported, unavailable, failed, cancelled, awaiting_human, needs_review.
	if stopped != 7 {
		t.Fatalf("found %d stopped states, want 7 — job.go's state vocabulary changed and this "+
			"test's own reach changed with it; confirm classify and the Outcome enum still agree", stopped)
	}
}
