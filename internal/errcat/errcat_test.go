// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package errcat

import (
	"strings"
	"testing"

	"papio/internal/config"
	"papio/internal/job"
)

func TestExplainCategoriesAndGuidance(t *testing.T) {
	withInstitution := config.Config{
		AccessMode: config.ModeDelegated,
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
			accessMode: config.ModeDelegated, cfg: config.Config{AccessMode: config.ModeDelegated},
			wantCategory: "institution_not_configured",
		},
		{
			name: "institution configured but no entitlement", state: "unavailable", reason: "candidates_exhausted",
			accessMode: config.ModeDelegated, cfg: withInstitution,
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
			got := Explain(test.state, test.reason, test.resolver, test.accessMode, test.cfg)
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
	got := Explain("unavailable", "candidates_exhausted", "campus", config.ModeAssisted, cfg)
	if got.Category != "institution_not_configured" {
		t.Fatalf("named-profile miss category = %q, want institution_not_configured", got.Category)
	}
}

func TestWaitGuidance(t *testing.T) {
	cfg := config.Config{AccessMode: config.ModeDelegated}
	// Success and non-actionable states produce no guidance block.
	if g := WaitGuidanceWithOpenAction("ready", "", "", "", nil, cfg); g != "" {
		t.Fatalf("ready guidance = %q, want empty", g)
	}
	if g := WaitGuidanceWithOpenAction("resolving", "", "", "", nil, cfg); g != "" {
		t.Fatalf("resolving guidance = %q, want empty", g)
	}
	// A parked job renders a bracketed category and an arrow next-step.
	g := WaitGuidanceWithOpenAction("awaiting_human", "institutional_handoff", "", "", nil, cfg)
	if !strings.Contains(g, "[login_required]") || !strings.Contains(g, "\u2192") {
		t.Fatalf("awaiting_human guidance = %q", g)
	}
	// The config-aware no-access case reaches acquire --wait output too.
	g = WaitGuidanceWithOpenAction("unavailable", "no_legal_candidates", "", config.ModeDelegated, nil, cfg)
	if !strings.Contains(g, "[institution_not_configured]") {
		t.Fatalf("unavailable guidance = %q", g)
	}
}

func TestExplainWithOpenActionUsesReplacementManualDownload(t *testing.T) {
	cfg := config.Config{AccessMode: config.ModeDelegated}
	actions := []job.HumanAction{
		{ID: 227, Kind: "openurl_handoff", Status: "resolved", RequiresAuth: true},
		{ID: 228, Kind: "manual_download", Status: "open", RequiresAuth: true, BlockedBy: "landing_page"},
	}
	got := ExplainWithOpenAction("awaiting_human", "login_required", "", config.ModeDelegated, actions, cfg)

	if got.Category != "manual_download" {
		t.Fatalf("category = %q, want manual_download", got.Category)
	}
	for _, want := range []string{"Sign in at your institution", "download the PDF yourself"} {
		if !strings.Contains(got.Guidance, want) {
			t.Fatalf("guidance = %q, want %q", got.Guidance, want)
		}
	}
	if strings.Contains(got.Guidance, "papio actions open") {
		t.Fatalf("manual-download guidance names an inapplicable command: %q", got.Guidance)
	}

	wait := WaitGuidanceWithOpenAction("awaiting_human", "login_required", "", config.ModeDelegated, actions, cfg)
	if !strings.Contains(wait, "[manual_download]") || strings.Contains(wait, "papio actions open") {
		t.Fatalf("wait guidance = %q", wait)
	}
}

// TestExplainWithOpenActionNamesTheAdoptionRootForDownloadsAccess pins the
// downloads_access_required explanation: it must name the blocked adoption
// root (carried on the action's Detail) and point at the macOS grant, not
// offer a sign-in or `papio actions open` — there is nothing to open, the
// grant happens outside papio entirely.
func TestExplainWithOpenActionNamesTheAdoptionRootForDownloadsAccess(t *testing.T) {
	cfg := config.Config{AccessMode: config.ModeDelegated}
	actions := []job.HumanAction{
		{ID: 501, Kind: "downloads_access_required", Status: "open", Detail: "/Users/example/Downloads/papio"},
	}
	got := ExplainWithOpenAction("awaiting_human", "", "", config.ModeDelegated, actions, cfg)
	if got.Category != "downloads_access_required" {
		t.Fatalf("category = %q, want downloads_access_required", got.Category)
	}
	if !strings.Contains(got.Guidance, "/Users/example/Downloads/papio") {
		t.Fatalf("guidance omits the blocked adoption root: %q", got.Guidance)
	}
	if !strings.Contains(got.Guidance, "System Settings") {
		t.Fatalf("guidance omits the grant remediation: %q", got.Guidance)
	}
	for _, forbidden := range []string{"sign in", "papio actions open"} {
		if strings.Contains(got.Guidance, forbidden) {
			t.Fatalf("guidance offers inapplicable remedy %q: %q", forbidden, got.Guidance)
		}
	}
}

func TestExplainWithOpenActionKeepsTerminalExplanationsWithoutOpenAction(t *testing.T) {
	cfg := config.Config{
		AccessMode: config.ModeDelegated,
		Browser:    config.Browser{OpenURLBase: "https://library.example.edu/openurl"},
	}
	cases := []struct {
		name       string
		state      string
		reason     string
		accessMode string
	}{
		{name: "no identifier", state: "unavailable", reason: "no_identifier", accessMode: config.ModeDelegated},
		{name: "no entitlement", state: "unavailable", reason: "candidates_exhausted", accessMode: config.ModeDelegated},
		{name: "failed", state: "failed", reason: "unexpected_error"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			want := Explain(test.state, test.reason, "", test.accessMode, cfg)
			got := ExplainWithOpenAction(test.state, test.reason, "", test.accessMode,
				[]job.HumanAction{{ID: 228, Kind: "manual_download", Status: "resolved", RequiresAuth: true}}, cfg)
			if got != want {
				t.Fatalf("explanation = %#v, want %#v", got, want)
			}
		})
	}
}

// The most expensive wrong answer papio can give is "sign in" for a work no
// login can deliver: the user spends an SSO round trip, the job parks forever,
// and the action reminder pass then nags them about it on a schedule. This
// pins that the no-identifier explanation is distinct from the handoff one and
// never asks for authentication.
func TestNoIdentifierGuidanceNeverAsksForASignIn(t *testing.T) {
	cfg := config.Config{AccessMode: config.ModeDelegated}
	got := Explain("unavailable", "no_identifier", "", config.ModeDelegated, cfg)

	if got.Category != "no_identifier" {
		t.Fatalf("category = %q, want no_identifier", got.Category)
	}
	lower := strings.ToLower(got.Guidance)
	for _, forbidden := range []string{"sign in", "log in", "login", "actions open"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("guidance offers %q for a work no login can deliver: %q", forbidden, got.Guidance)
		}
	}
	// It must give both a direct re-submit path and the Zotero queue path, and
	// enrichment must apply metadata rather than only preview its proposed DOI.
	for _, remedy := range []string{
		"DOI",
		"`papio acquire --doi <doi>`",
		"`zotio --yes items enrich --missing-doi`",
		"`papio acquire --from-zotio`",
	} {
		if !strings.Contains(got.Guidance, remedy) {
			t.Fatalf("guidance omits working remedy %q: %q", remedy, got.Guidance)
		}
	}
	// And it must be reachable from acquire --wait, not just the dashboard.
	if g := WaitGuidanceWithOpenAction("unavailable", "no_identifier", "", config.ModeDelegated, nil, cfg); !strings.Contains(g, "[no_identifier]") {
		t.Fatalf("wait guidance = %q", g)
	}
}

func TestExplainZotioImportErrorFileStorageRefused(t *testing.T) {
	got, ok := ExplainZotioImportError("zotero_file_storage_refused")
	if !ok {
		t.Fatal("ExplainZotioImportError returned false for zotero_file_storage_refused")
	}
	if got.Category != "zotero_file_storage_refused" {
		t.Fatalf("category = %q, want zotero_file_storage_refused", got.Category)
	}
	for _, want := range []string{
		"Papio has the paper",
		"PDF is safe",
		"nothing is corrupted",
		"HTTP 413",
		"WebDAV",
		"3 MB upload succeeded",
		"428 KB",
		"Sync pane",
		"attachment_mode = \"linked-file\"",
		"[zotio]",
		"do not sync to other devices",
		"break if the file moves",
	} {
		if !strings.Contains(got.Guidance, want) {
			t.Fatalf("guidance missing %q: %q", want, got.Guidance)
		}
	}
	if _, ok := ExplainZotioImportError("zotero_http_4xx"); ok {
		t.Fatal("unknown class should not match")
	}
}
