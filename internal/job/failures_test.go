// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package job

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestFailures(t *testing.T) {
	ctx := context.Background()
	longReason := strings.Repeat("x", failureReasonLimit+7)

	tests := []struct {
		name      string
		setup     func(t *testing.T, js *Store) []string
		since     time.Time
		limit     int
		want      []FailureGroup
		wantCount int
	}{
		{
			name: "groups failures by state provider and transition reason",
			setup: func(t *testing.T, js *Store) []string {
				first := createFailure(t, js, "failures-first", StateFailed, "timeout", []string{"https://api.example.test/first.pdf"}, false)
				second := createFailure(t, js, "failures-second", StateFailed, "timeout", []string{"https://api.example.test/second.pdf"}, false)
				selected := createFailure(t, js, "failures-selected", StateUnavailable, longReason,
					[]string{"https://selected.example.test/paper.pdf", "https://newest.example.test/paper.pdf"}, true)
				newest := createFailure(t, js, "failures-newest", StateUnavailable, "newest candidate",
					[]string{"https://older.example.test/paper.pdf", "https://newest.example.test/paper.pdf"}, false)
				fallback := createFailure(t, js, "failures-fallback", StateNeedsReview, "inspect identity", nil, false)
				awaiting := createFailure(t, js, "failures-awaiting", StateAwaitingHuman, "", []string{"https://handoff.example.test/"}, false)
				setFailureUpdatedAt(t, js, first, "2026-01-01T00:00:00Z")
				setFailureUpdatedAt(t, js, second, "2026-01-02T00:00:00Z")
				setFailureUpdatedAt(t, js, selected, "2026-01-03T00:00:00Z")
				setFailureUpdatedAt(t, js, newest, "2026-01-03T12:00:00Z")
				setFailureUpdatedAt(t, js, fallback, "2026-01-04T00:00:00Z")
				setFailureUpdatedAt(t, js, awaiting, "2026-01-05T00:00:00Z")
				return []string{second}
			},
			want: []FailureGroup{
				{State: StateFailed, Provider: "api.example.test", Reason: "timeout", Count: 2},
				{State: StateAwaitingHuman, Provider: "handoff.example.test", Reason: "-", Count: 1},
				{State: StateNeedsReview, Provider: "-", Reason: "inspect identity", Count: 1},
				{State: StateUnavailable, Provider: "newest.example.test", Reason: "newest candidate", Count: 1},
				{State: StateUnavailable, Provider: "selected.example.test", Reason: strings.Repeat("x", failureReasonLimit), Count: 1},
			},
		},
		{
			name: "filters by updated at",
			setup: func(t *testing.T, js *Store) []string {
				old := createFailure(t, js, "failures-old", StateFailed, "old", nil, false)
				newer := createFailure(t, js, "failures-new", StateUnavailable, "new", nil, false)
				setFailureUpdatedAt(t, js, old, "2026-01-01T00:00:00Z")
				setFailureUpdatedAt(t, js, newer, "2026-02-01T00:00:00Z")
				return []string{newer}
			},
			since: time.Date(2026, time.January, 15, 0, 0, 0, 0, time.UTC),
			want:  []FailureGroup{{State: StateUnavailable, Provider: "-", Reason: "new", Count: 1}},
		},
		{
			name: "excludes failures well before the cutoff",
			setup: func(t *testing.T, js *Store) []string {
				old := createFailure(t, js, "failures-well-before-cutoff", StateFailed, "old", nil, false)
				setFailureUpdatedAt(t, js, old, "2026-01-01T00:00:00Z")
				return nil
			},
			since: time.Date(2026, time.January, 15, 0, 0, 0, 0, time.UTC),
			want:  []FailureGroup{},
		},
		{
			name: "uses terminal reason when transition detail omits it",
			setup: func(t *testing.T, js *Store) []string {
				createFailureWithTerminalReason(t, js, "failures-terminal-reason", StateUnavailable, "no viable candidates")
				return nil
			},
			want: []FailureGroup{{State: StateUnavailable, Provider: "-", Reason: "no viable candidates", Count: 1}},
		},
		{
			name: "filters and samples fractional timestamps chronologically",
			setup: func(t *testing.T, js *Store) []string {
				exact := createFailure(t, js, "failures-time-exact", StateFailed, "timing", []string{"https://time.example.test/a"}, false)
				fractional := createFailure(t, js, "failures-time-fractional", StateFailed, "timing", []string{"https://time.example.test/a"}, false)
				setFailureUpdatedAt(t, js, exact, "2026-03-01T12:00:00Z")
				setFailureUpdatedAt(t, js, fractional, "2026-03-01T12:00:00.5Z")
				return []string{fractional}
			},
			since: time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
			want:  []FailureGroup{{State: StateFailed, Provider: "time.example.test", Reason: "timing", Count: 2}},
		},
		{
			name: "filters and samples nanosecond timestamps exactly",
			setup: func(t *testing.T, js *Store) []string {
				before := createFailure(t, js, "failures-nanos-before", StateFailed, "nanotiming", []string{"https://nanos.example.test/a"}, false)
				atCutoff := createFailure(t, js, "failures-nanos-at-cutoff", StateFailed, "nanotiming", []string{"https://nanos.example.test/a"}, false)
				newest := createFailure(t, js, "failures-nanos-newest", StateFailed, "nanotiming", []string{"https://nanos.example.test/a"}, false)
				setFailureUpdatedAt(t, js, before, "2026-03-02T12:00:00.123456788Z")
				setFailureUpdatedAt(t, js, atCutoff, "2026-03-02T12:00:00.123456789Z")
				setFailureUpdatedAt(t, js, newest, "2026-03-02T12:00:00.123456790Z")
				return []string{newest}
			},
			since: time.Date(2026, time.March, 2, 12, 0, 0, 123456789, time.UTC),
			want:  []FailureGroup{{State: StateFailed, Provider: "nanos.example.test", Reason: "nanotiming", Count: 2}},
		},
		{
			name: "groups provider hostnames case insensitively without root dot",
			setup: func(t *testing.T, js *Store) []string {
				createFailure(t, js, "failures-host-uppercase", StateFailed, "network", []string{"https://API.Example.Test/a"}, false)
				createFailure(t, js, "failures-host-root-dot", StateFailed, "network", []string{"https://api.example.test./b"}, false)
				return nil
			},
			want: []FailureGroup{{State: StateFailed, Provider: "api.example.test", Reason: "network", Count: 2}},
		},
		{
			name: "clamps the result limit",
			setup: func(t *testing.T, js *Store) []string {
				for i := range failureMaxLimit + 1 {
					createFailure(t, js, fmt.Sprintf("failures-limit-%03d", i), StateFailed, fmt.Sprintf("reason-%03d", i), nil, false)
				}
				return nil
			},
			limit:     failureMaxLimit + 1,
			wantCount: failureMaxLimit,
		},
		{
			name: "defaults an omitted limit to fifty groups",
			setup: func(t *testing.T, js *Store) []string {
				for i := range failureDefaultLimit + 1 {
					createFailure(t, js, fmt.Sprintf("failures-default-%03d", i), StateFailed, fmt.Sprintf("reason-%03d", i), nil, false)
				}
				return nil
			},
			wantCount: failureDefaultLimit,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			js := testStore(t)
			samples := tc.setup(t, js)
			got, err := js.Failures(ctx, tc.since, tc.limit)
			if err != nil {
				t.Fatalf("failures: %v", err)
			}
			wantCount := tc.wantCount
			if wantCount == 0 {
				wantCount = len(tc.want)
			}
			if len(got) != wantCount {
				t.Fatalf("group count = %d, want %d: %+v", len(got), wantCount, got)
			}
			for i := range tc.want {
				if got[i].State != tc.want[i].State || got[i].Provider != tc.want[i].Provider || got[i].Reason != tc.want[i].Reason || got[i].Count != tc.want[i].Count {
					t.Fatalf("group %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
			if len(samples) != 0 && got[0].Sample != samples[0] {
				t.Fatalf("sample = %q, want most recent %q", got[0].Sample, samples[0])
			}
		})
	}

	js := testStore(t)
	createFailure(t, js, "failures-negative", StateFailed, "first", nil, false)
	createFailure(t, js, "failures-negative-second", StateUnavailable, "second", nil, false)
	got, err := js.Failures(ctx, time.Time{}, -1)
	if err != nil {
		t.Fatalf("negative limit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("negative limit groups = %d, want 1", len(got))
	}
}

func createFailure(t *testing.T, js *Store, requestID, state, reason string, urls []string, selectFirst bool) string {
	t.Helper()
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, requestID, testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatalf("create %s: %v", requestID, err)
	}
	if len(urls) > 0 {
		candidates := make([]Candidate, 0, len(urls))
		for i, rawURL := range urls {
			candidates = append(candidates, Candidate{
				JobID: id, Source: "fixture", URLRedacted: rawURL, URLKey: fmt.Sprintf("%s-%d", requestID, i),
				Version: "published", AccessBasis: "open_access", ReuseLicense: "unknown", Rank: i,
			})
		}
		if _, err := js.InsertCandidates(ctx, id, candidates); err != nil {
			t.Fatalf("insert candidates: %v", err)
		}
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("to resolving: %v", err)
	}
	options := []TransitionOpt{}
	if selectFirst {
		candidate, err := js.NextPendingCandidate(ctx, id)
		if err != nil {
			t.Fatalf("selected candidate: %v", err)
		}
		options = append(options, WithCandidate(candidate.ID))
	}
	detail := map[string]any{}
	if reason != "" {
		detail["reason"] = reason
	}
	if err := js.Transition(ctx, id, StateResolving, state, detail, options...); err != nil {
		t.Fatalf("to %s: %v", state, err)
	}
	return id
}

func createFailureWithTerminalReason(t *testing.T, js *Store, requestID, state, reason string) {
	t.Helper()
	ctx := context.Background()
	id, err := js.CreateRequest(ctx, requestID, testWork(), "", "", testPolicy(), nil)
	if err != nil {
		t.Fatalf("create %s: %v", requestID, err)
	}
	if err := js.Transition(ctx, id, StateQueued, StateResolving, nil); err != nil {
		t.Fatalf("to resolving: %v", err)
	}
	if err := js.Transition(ctx, id, StateResolving, state, nil, WithTerminalReason(reason)); err != nil {
		t.Fatalf("to %s: %v", state, err)
	}
}

func setFailureUpdatedAt(t *testing.T, js *Store, id, at string) {
	t.Helper()
	if _, err := js.S.DB().ExecContext(context.Background(), "UPDATE jobs SET updated_at = ? WHERE id = ?", at, id); err != nil {
		t.Fatalf("set updated_at: %v", err)
	}
}
