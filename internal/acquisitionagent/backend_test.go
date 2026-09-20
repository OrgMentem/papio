// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package acquisitionagent_test

import (
	"context"
	"strings"
	"testing"

	"papio/internal/acquisitionagent"
)

// This independent implementation uses only the public interface: no cloud
// client, key, HTTP, or package-internal request and response types.
type localBackend struct{ choice string }

func (b localBackend) Decide(ctx context.Context, o acquisitionagent.Observation) (acquisitionagent.Decision, error) {
	if err := ctx.Err(); err != nil {
		return acquisitionagent.Decision{}, err
	}
	if err := o.Validate(); err != nil {
		return acquisitionagent.Decision{}, err
	}
	return acquisitionagent.Decision{Choice: b.choice, Model: "local"}, nil
}

var _ acquisitionagent.Backend = localBackend{}

func TestInjectedLocalBackendConformance(t *testing.T) {
	o := acquisitionagent.Observation{
		Revision: strings.Repeat("a", 64), DOI: "10.1234/article",
		Controls: []acquisitionagent.Control{
			{ID: "download", Role: "link", Label: "PDF"},
			{ID: "disabled", Role: "button", Disabled: true},
		},
	}
	for _, choice := range []string{"download", "WAIT", "BLOCKED", "disabled", "invented"} {
		t.Run(choice, func(t *testing.T) {
			var backend acquisitionagent.Backend = localBackend{choice: choice}
			d, err := backend.Decide(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			err = acquisitionagent.ValidateDecision(o, d)
			wantValid := choice == "download" || choice == "WAIT" || choice == "BLOCKED"
			if (err == nil) != wantValid {
				t.Fatalf("choice=%s validation error=%v", choice, err)
			}
		})
	}
}
