// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"papio/internal/config"
)

const reviewedSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestActionsResolveBindsTheVerdictToTheInspectedReview: a verdict must reach
// the daemon's compare-and-swap branch — the action revision always, and for
// --accept the digest of the quarantined file the user inspected — so a
// review that changed since it was listed cannot be accepted on stale
// evidence. The legacy call carried neither and bypassed both guards.
func TestActionsResolveBindsTheVerdictToTheInspectedReview(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want map[string]any
	}{
		{
			name: "accept",
			args: []string{"actions", "resolve", "9", "--accept", "--revision", "3", "--sha256", strings.ToUpper(reviewedSHA256)},
			want: map[string]any{"action_id": int64(9), "verdict": "accept", "expected_revision": int64(3), "expected_sha256": reviewedSHA256},
		},
		{
			name: "reject",
			args: []string{"actions", "resolve", "9", "--reject", "--revision", "3"},
			want: map[string]any{"action_id": int64(9), "verdict": "reject", "expected_revision": int64(3)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			var got map[string]any
			root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
				if method != "actions.resolve" {
					t.Fatalf("method = %q", method)
				}
				got = params.(map[string]any)
				*result.(*map[string]any) = map[string]any{"outcome": "applied", "job_id": "job_review", "state": "resolving"}
				return nil
			})
			root.SetArgs(tc.args)
			if err := root.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("resolve: %v (%s)", err, errOut.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("params = %#v, want %#v", got, tc.want)
			}
			if out.String() != "job_review\tresolving\n" {
				t.Fatalf("stdout = %q", out.String())
			}
		})
	}
}

// TestActionsResolveRefusesAnUnboundVerdict: without the binding the daemon
// would take its legacy path, so the CLI must refuse before calling it.
func TestActionsResolveRefusesAnUnboundVerdict(t *testing.T) {
	for _, args := range [][]string{
		{"actions", "resolve", "9", "--accept"},
		{"actions", "resolve", "9", "--accept", "--revision", "3"},
		{"actions", "resolve", "9", "--accept", "--revision", "3", "--sha256", "not-a-digest"},
		{"actions", "resolve", "9", "--reject"},
	} {
		var out, errOut bytes.Buffer
		root := NewInProcessRoot(&out, &errOut, config.Config{}, func(context.Context, string, any, any) error {
			t.Fatalf("%v: reached the daemon without a review binding", args)
			return nil
		})
		root.SetArgs(args)
		if err := root.ExecuteContext(context.Background()); err == nil {
			t.Fatalf("%v: want an error", args)
		}
	}
}

// TestActionsResolveConflictExitsNonzero: a review that changed since it was
// listed is not applied, and the exit status must say so.
func TestActionsResolveConflictExitsNonzero(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*map[string]any) = map[string]any{"outcome": "conflict"}
		return nil
	})
	root.SetArgs([]string{"actions", "resolve", "9", "--accept", "--revision", "3", "--sha256", reviewedSHA256})
	err := root.ExecuteContext(context.Background())
	if !errors.Is(err, errReviewChanged) {
		t.Fatalf("err = %v, want errReviewChanged", err)
	}
	if !strings.Contains(out.String(), "not resolved") {
		t.Fatalf("stdout = %q, want the conflict explained", out.String())
	}
}
