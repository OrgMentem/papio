// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"

	"papio/internal/config"
)

func TestExplainJobCategoriesAndGuidance(t *testing.T) {
	withInstitution := config.Config{
		AccessMode: config.ModeMaximal,
		Browser:    config.Browser{OpenURLBase: "https://library.example.edu/openurl"},
	}
	for _, test := range []struct {
		name         string
		state        string
		reason       string
		resolver     string
		accessMode   string
		cfg          config.Config
		wantCategory string
	}{
		{name: "login required", state: "awaiting_human", reason: "institutional_handoff", wantCategory: "login_required"},
		{name: "manual download", state: "awaiting_human", reason: "landing_page_only", wantCategory: "manual_download"},
		{name: "identity review", state: "needs_review", reason: "semantic_or_identity_review", wantCategory: "identity_review"},
		{name: "unsafe pdf", state: "needs_review", reason: "encrypted_or_active_content", wantCategory: "unsafe_pdf"},
		{name: "retrying", state: "retry_wait", reason: "resolver_temporarily_unavailable", wantCategory: "retrying"},
		{
			name: "no institution configured", state: "unavailable", reason: "no_legal_candidates",
			accessMode: config.ModeMaximal, cfg: config.Config{AccessMode: config.ModeMaximal},
			wantCategory: "institution_not_configured",
		},
		{
			name: "institution configured but no entitlement", state: "unavailable", reason: "candidates_exhausted",
			accessMode: config.ModeMaximal, cfg: withInstitution,
			wantCategory: "no_access",
		},
		{
			name: "conservative mode", state: "unavailable", reason: "candidates_exhausted",
			accessMode: config.ModeConservative, cfg: config.Config{AccessMode: config.ModeConservative},
			wantCategory: "no_access_conservative",
		},
		{name: "unknown reason falls back to state", state: "failed", reason: "some_future_reason", wantCategory: "failed"},
		{name: "cancelled", state: "cancelled", reason: "—", wantCategory: "cancelled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := explainJob(test.state, test.reason, test.resolver, test.accessMode, test.cfg)
			if got.Category != test.wantCategory {
				t.Fatalf("category = %q, want %q", got.Category, test.wantCategory)
			}
			if strings.TrimSpace(got.Guidance) == "" {
				t.Fatalf("category %q has empty guidance", got.Category)
			}
		})
	}
}

func TestExplainNoAccessNamedProfileNotConfigured(t *testing.T) {
	// A job snapshotted with a named resolver profile that no longer exists in
	// config must surface as institution_not_configured, not generic no_access —
	// the default institution's presence must not mask a missing named profile.
	cfg := config.Config{
		AccessMode: config.ModeAssisted,
		Browser:    config.Browser{OpenURLBase: "https://default.example.edu/openurl"},
	}
	got := explainJob("unavailable", "candidates_exhausted", "campus", config.ModeAssisted, cfg)
	if got.Category != "institution_not_configured" {
		t.Fatalf("named-profile miss category = %q, want institution_not_configured", got.Category)
	}
}

func TestWaitGuidance(t *testing.T) {
	cfg := config.Config{AccessMode: config.ModeMaximal}
	// Success and non-actionable states produce no guidance block.
	if g := waitGuidance("ready", "", "", "", cfg); g != "" {
		t.Fatalf("ready guidance = %q, want empty", g)
	}
	if g := waitGuidance("resolving", "", "", "", cfg); g != "" {
		t.Fatalf("resolving guidance = %q, want empty", g)
	}
	// A parked job renders a bracketed category and an arrow next-step.
	g := waitGuidance("awaiting_human", "institutional_handoff", "", "", cfg)
	if !strings.Contains(g, "[login_required]") || !strings.Contains(g, "\u2192") {
		t.Fatalf("awaiting_human guidance = %q", g)
	}
	// The config-aware no-access case reaches acquire --wait output too.
	g = waitGuidance("unavailable", "no_legal_candidates", "", config.ModeMaximal, cfg)
	if !strings.Contains(g, "[institution_not_configured]") {
		t.Fatalf("unavailable guidance = %q", g)
	}
}
