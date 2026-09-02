// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
)

// notifyAllowedPhases is the category/phase matrix stated as a literal, so it
// fails if PhasesFor gains, loses, or moves a phase. Reading it back out of
// PhasesFor would make the assertion tautological.
var notifyAllowedPhases = map[Category][]Phase{
	CategoryRequestOutcome:  {PhaseTerminal},
	CategoryDecisionOpened:  {PhaseOpened},
	CategoryDecisionPending: {PhaseReminder},
	CategoryCompletionBatch: {PhaseCheckpoint, PhaseFinal},
	CategoryDiscoveryNew:    {PhaseDigest},
	CategoryIntegrityNotice: {PhaseScan},
	CategorySystemDegraded:  {PhaseEpisode},
}

var notifyAllPhases = []Phase{PhaseTerminal, PhaseOpened, PhaseReminder, PhaseCheckpoint, PhaseFinal, PhaseDigest, PhaseScan, PhaseEpisode}

func notifyIntentFor(category Category, phase Phase, at time.Time) Intent {
	kind := "domain." + string(category)
	return Intent{
		EventKind:    kind,
		Category:     category,
		AggregateKey: "aggregate:" + string(category),
		Phase:        phase,
		WindowStart:  at,
		HappenedAt:   at,
		Message:      "something happened",
		Detail:       Event{Kind: kind, Message: "something happened", Count: 1},
	}
}

// TestValidateIntentEnforcesFullCategoryPhaseMatrix sweeps every category
// against every phase in the vocabulary. A silently widened matrix is a
// correctness bug, not a cosmetic one: completion_batch distinguishes
// checkpoint from final to drive supersession, so accepting a foreign phase
// there would persist a row that never supersedes and never finalizes.
func TestValidateIntentEnforcesFullCategoryPhaseMatrix(t *testing.T) {
	if len(notifyAllowedPhases) != len(Categories()) {
		t.Fatalf("matrix covers %d categories, Categories() has %d — extend the literal matrix", len(notifyAllowedPhases), len(Categories()))
	}
	for _, category := range Categories() {
		if _, ok := notifyAllowedPhases[category]; !ok {
			t.Fatalf("category %q is missing from the literal phase matrix", category)
		}
	}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	for _, category := range Categories() {
		for _, phase := range notifyAllPhases {
			want := false
			for _, allowed := range notifyAllowedPhases[category] {
				if allowed == phase {
					want = true
				}
			}
			t.Run(string(category)+"/"+string(phase), func(t *testing.T) {
				ledger := newRouterLedger()
				router := NewRouter(RouterOptions{Ledger: ledger, Now: func() time.Time { return now }})
				err := router.Route(context.Background(), notifyIntentFor(category, phase, now))
				if want {
					if err != nil {
						t.Fatalf("Route(%s/%s) = %v, want the pair accepted", category, phase, err)
					}
					if len(ledger.rows) != 1 {
						t.Fatalf("ledger rows = %d, want the accepted intent persisted once", len(ledger.rows))
					}
					for _, row := range ledger.rows {
						if row.Intent.Category != category || row.Intent.Phase != phase {
							t.Fatalf("persisted row = %s/%s, want %s/%s", row.Intent.Category, row.Intent.Phase, category, phase)
						}
					}
					return
				}
				wantErr := "phase \"" + string(phase) + "\" is not allowed for category \"" + string(category) + "\""
				if err == nil || !strings.Contains(err.Error(), wantErr) {
					t.Fatalf("Route(%s/%s) = %v, want an error containing %q", category, phase, err, wantErr)
				}
				if len(ledger.rows) != 0 {
					t.Fatalf("ledger rows = %d, want the rejection to happen before any insert", len(ledger.rows))
				}
			})
		}
	}
}

// TestValidateIntentRejectsUnknownCategoriesAndMissingFields pins the four
// required fields and the closed category set. Each rejection must land before
// the ledger insert, because a persisted half-identified row would be routed
// forever without ever coalescing correctly.
func TestValidateIntentRejectsUnknownCategoriesAndMissingFields(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		mutate  func(*Intent)
		wantErr string
	}{
		{
			name:    "unknown category",
			mutate:  func(in *Intent) { in.Category = Category("paper_ready") },
			wantErr: "unknown notification category \"paper_ready\"",
		},
		{
			name:    "empty category",
			mutate:  func(in *Intent) { in.Category = Category("") },
			wantErr: "unknown notification category \"\"",
		},
		{
			name:    "category differing only in case",
			mutate:  func(in *Intent) { in.Category = Category("Request_Outcome") },
			wantErr: "unknown notification category \"Request_Outcome\"",
		},
		{
			name:    "empty phase",
			mutate:  func(in *Intent) { in.Phase = Phase("") },
			wantErr: "phase \"\" is not allowed for category \"request_outcome\"",
		},
		{
			name:    "missing event kind",
			mutate:  func(in *Intent) { in.EventKind = "" },
			wantErr: "notification event kind is required",
		},
		{
			name:    "missing aggregate key",
			mutate:  func(in *Intent) { in.AggregateKey = "" },
			wantErr: "notification aggregate key is required",
		},
		{
			name:    "missing window start",
			mutate:  func(in *Intent) { in.WindowStart = time.Time{} },
			wantErr: "notification window start is required",
		},
		{
			name:    "missing happened at",
			mutate:  func(in *Intent) { in.HappenedAt = time.Time{} },
			wantErr: "notification happened at is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ledger := newRouterLedger()
			router := NewRouter(RouterOptions{Ledger: ledger, Now: func() time.Time { return now }})
			intent := notifyIntentFor(CategoryRequestOutcome, PhaseTerminal, now)
			test.mutate(&intent)
			err := router.Route(context.Background(), intent)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Route = %v, want an error containing %q", err, test.wantErr)
			}
			if len(ledger.rows) != 0 {
				t.Fatalf("ledger rows = %d, want the rejection to happen before any insert", len(ledger.rows))
			}
		})
	}
}

// TestValidateIntentKeepsCompletionBatchTwoArmed states the checkpoint/final
// contract on its own: both arms are accepted, and the arms carry different
// ledger semantics, so the pair is not interchangeable with any other phase.
func TestValidateIntentKeepsCompletionBatchTwoArmed(t *testing.T) {
	if got := PhasesFor(CategoryCompletionBatch); len(got) != 2 || got[0] != PhaseCheckpoint || got[1] != PhaseFinal {
		t.Fatalf("PhasesFor(completion_batch) = %v, want [checkpoint final]", got)
	}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	ledger := newRouterLedger()
	policy := Policy{Categories: map[Category]CategoryPolicy{
		CategoryCompletionBatch: {Desktop: "immediate", Webhook: "off", Window: time.Minute},
	}}
	router := NewRouter(RouterOptions{Ledger: ledger, Policy: policy, Now: func() time.Time { return now }})
	checkpoint := notifyIntentFor(CategoryCompletionBatch, PhaseCheckpoint, now)
	checkpoint.Message, checkpoint.Detail = "Batch update · 2 papers", Event{Kind: "batch.checkpoint", Message: "Batch update · 2 papers", Count: 2}
	if err := router.Route(context.Background(), checkpoint); err != nil {
		t.Fatalf("checkpoint rejected: %v", err)
	}
	final := notifyIntentFor(CategoryCompletionBatch, PhaseFinal, now)
	final.Message, final.Detail = "Batch finished · 4 papers", Event{Kind: "batch.final", Message: "Batch finished · 4 papers", Count: 4}
	if err := router.Route(context.Background(), final); err != nil {
		t.Fatalf("final rejected: %v", err)
	}
	var checkpointState string
	var finals int
	for _, row := range ledger.rows {
		switch row.Intent.Phase {
		case PhaseCheckpoint:
			checkpointState = row.DesktopState
		case PhaseFinal:
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("final rows = %d, want exactly one", finals)
	}
	if checkpointState != "superseded" {
		t.Fatalf("checkpoint desktop state = %q, want the final arm to supersede it", checkpointState)
	}
}

func notifyPresetRoutes(t *testing.T, cfg config.Notify, want map[Category]CategoryPolicy) Policy {
	t.Helper()
	policy, err := ResolvePolicy(cfg)
	if err != nil {
		t.Fatalf("ResolvePolicy(%+v) = %v", cfg, err)
	}
	for _, category := range Categories() {
		expected, ok := want[category]
		if !ok {
			t.Fatalf("expectation table is missing category %q", category)
		}
		if got := policy.For(category); got != expected {
			t.Fatalf("%s route = %+v, want %+v", category, got, expected)
		}
	}
	return policy
}

// TestResolvePolicyPresetMatrices pins each preset's routing table. A preset is
// the only control most operators touch, and every cell here is the difference
// between a notification arriving now, arriving in a digest, or never arriving.
func TestResolvePolicyPresetMatrices(t *testing.T) {
	const hour = time.Hour
	quiet := map[Category]CategoryPolicy{
		CategoryRequestOutcome:  {Desktop: "digest", Webhook: "immediate", Window: hour},
		CategoryDecisionOpened:  {Desktop: "digest", Webhook: "immediate", Window: 5 * time.Minute},
		CategoryDecisionPending: {Desktop: "off", Webhook: "immediate", Window: 4 * hour},
		CategoryCompletionBatch: {Desktop: "off", Webhook: "immediate", Window: hour},
		CategoryDiscoveryNew:    {Desktop: "off", Webhook: "immediate", Window: hour},
		CategoryIntegrityNotice: {Desktop: "immediate", Webhook: "immediate", Window: hour},
		CategorySystemDegraded:  {Desktop: "immediate", Webhook: "immediate", Window: hour},
	}
	milestones := map[Category]CategoryPolicy{
		CategoryRequestOutcome:  {Desktop: "immediate", Webhook: "immediate", Window: hour},
		CategoryDecisionOpened:  {Desktop: "immediate", Webhook: "immediate", Window: 5 * time.Minute},
		CategoryDecisionPending: {Desktop: "digest", Webhook: "immediate", Window: 4 * hour},
		CategoryCompletionBatch: {Desktop: "immediate", Webhook: "immediate", Window: hour},
		CategoryDiscoveryNew:    {Desktop: "digest", Webhook: "immediate", Window: hour},
		CategoryIntegrityNotice: {Desktop: "immediate", Webhook: "immediate", Window: hour},
		CategorySystemDegraded:  {Desktop: "immediate", Webhook: "immediate", Window: hour},
	}
	verbose := map[Category]CategoryPolicy{
		CategoryRequestOutcome:  {Desktop: "immediate", Webhook: "immediate", Window: hour},
		CategoryDecisionOpened:  {Desktop: "immediate", Webhook: "immediate", Window: time.Minute},
		CategoryDecisionPending: {Desktop: "immediate", Webhook: "immediate", Window: time.Minute},
		CategoryCompletionBatch: {Desktop: "immediate", Webhook: "immediate", Window: hour},
		CategoryDiscoveryNew:    {Desktop: "immediate", Webhook: "immediate", Window: time.Minute},
		CategoryIntegrityNotice: {Desktop: "immediate", Webhook: "immediate", Window: hour},
		CategorySystemDegraded:  {Desktop: "immediate", Webhook: "immediate", Window: hour},
	}
	for _, test := range []struct {
		name        string
		preset      string
		wantPreset  string
		wantRoutes  map[Category]CategoryPolicy
		digestEvery int
		wantDigest  time.Duration
	}{
		{name: "quiet", preset: "quiet", wantPreset: "quiet", wantRoutes: quiet, digestEvery: 720, wantDigest: 12 * hour},
		{name: "milestones", preset: "milestones", wantPreset: "milestones", wantRoutes: milestones, digestEvery: 240, wantDigest: 4 * hour},
		{name: "verbose", preset: "verbose", wantPreset: "verbose", wantRoutes: verbose, digestEvery: 15, wantDigest: 15 * time.Minute},
		{name: "empty preset falls back to milestones", preset: "", wantPreset: "milestones", wantRoutes: milestones, digestEvery: 0, wantDigest: 4 * hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := notifyPresetRoutes(t, config.Notify{Preset: test.preset, DigestEveryMinutes: test.digestEvery}, test.wantRoutes)
			if policy.Preset != test.wantPreset {
				t.Fatalf("Preset = %q, want %q", policy.Preset, test.wantPreset)
			}
			if policy.DigestEvery != test.wantDigest {
				t.Fatalf("DigestEvery = %s, want %s", policy.DigestEvery, test.wantDigest)
			}
			for _, row := range policy.Table() {
				if row.Source != "preset" {
					t.Fatalf("%s source = %q, want preset when nothing is overridden", row.Category, row.Source)
				}
			}
		})
	}
}

// TestResolvePolicyPresetsDifferWhereItMatters keeps the three presets from
// collapsing into one another. Without this, a preset could be edited into a
// duplicate of its neighbour and every per-preset table above would still pass.
func TestResolvePolicyPresetsDifferWhereItMatters(t *testing.T) {
	resolved := map[string]Policy{}
	for _, preset := range []string{"quiet", "milestones", "verbose"} {
		policy, err := ResolvePolicy(config.Notify{Preset: preset})
		if err != nil {
			t.Fatalf("ResolvePolicy(%q) = %v", preset, err)
		}
		resolved[preset] = policy
	}
	for _, test := range []struct {
		left, right string
		category    Category
	}{
		{left: "quiet", right: "milestones", category: CategoryRequestOutcome},
		{left: "quiet", right: "milestones", category: CategoryCompletionBatch},
		{left: "milestones", right: "verbose", category: CategoryDecisionPending},
		{left: "milestones", right: "verbose", category: CategoryDiscoveryNew},
		// The transitive pairs are not enough: quiet and verbose are the two
		// extremes, and they could be edited into each other while milestones
		// stayed distinct from both, passing every check above.
		{left: "quiet", right: "verbose", category: CategoryRequestOutcome},
		{left: "quiet", right: "verbose", category: CategoryDecisionPending},
		{left: "quiet", right: "verbose", category: CategoryDiscoveryNew},
	} {
		left := resolved[test.left].For(test.category)
		right := resolved[test.right].For(test.category)
		if left == right {
			t.Fatalf("%s route is identical in %s and %s (%+v) — the presets no longer differ there", test.category, test.left, test.right, left)
		}
	}
}

// TestResolvePolicyRejectsUnknownPresetAndCategory proves a typo fails closed at
// resolution instead of silently routing every category to the fallback.
func TestResolvePolicyRejectsUnknownPresetAndCategory(t *testing.T) {
	for _, test := range []struct {
		name    string
		cfg     config.Notify
		wantErr string
	}{
		{
			name:    "unknown preset",
			cfg:     config.Notify{Preset: "chatty"},
			wantErr: "unknown notification preset \"chatty\"",
		},
		{
			name:    "preset differing only in case",
			cfg:     config.Notify{Preset: "Quiet"},
			wantErr: "unknown notification preset \"Quiet\"",
		},
		{
			name:    "unknown category override",
			cfg:     config.Notify{Preset: "quiet", Categories: map[string]config.NotifyCategory{"paper_ready": {Desktop: "immediate"}}},
			wantErr: "unknown notification category \"paper_ready\"",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, err := ResolvePolicy(test.cfg)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ResolvePolicy = %v, want an error containing %q", err, test.wantErr)
			}
			if policy.Preset != "" || policy.Categories != nil {
				t.Fatalf("rejected config returned a usable policy: %+v", policy)
			}
		})
	}
}

// TestResolvePolicyPartialCategoryOverride pins the merge rule: an override
// replaces only the fields it supplies, and the untouched fields keep their
// preset values. quiet/decision_pending is the discriminating row because its
// three fields are all different (off, immediate, 4h).
func TestResolvePolicyPartialCategoryOverride(t *testing.T) {
	quietDecisionPendingPreset := CategoryPolicy{Desktop: "off", Webhook: "immediate", Window: 4 * time.Hour}
	for _, test := range []struct {
		name     string
		override config.NotifyCategory
		want     CategoryPolicy
	}{
		{
			name:     "desktop only",
			override: config.NotifyCategory{Desktop: "immediate"},
			want:     CategoryPolicy{Desktop: "immediate", Webhook: "immediate", Window: 4 * time.Hour},
		},
		{
			name:     "webhook only",
			override: config.NotifyCategory{Webhook: "off"},
			want:     CategoryPolicy{Desktop: "off", Webhook: "off", Window: 4 * time.Hour},
		},
		{
			name:     "window only",
			override: config.NotifyCategory{WindowSeconds: 900},
			want:     CategoryPolicy{Desktop: "off", Webhook: "immediate", Window: 15 * time.Minute},
		},
		{
			name:     "zero window keeps the preset window",
			override: config.NotifyCategory{Desktop: "digest", WindowSeconds: 0},
			want:     CategoryPolicy{Desktop: "digest", Webhook: "immediate", Window: 4 * time.Hour},
		},
		{
			name:     "every field",
			override: config.NotifyCategory{Desktop: "digest", Webhook: "digest", WindowSeconds: 60},
			want:     CategoryPolicy{Desktop: "digest", Webhook: "digest", Window: time.Minute},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Notify{Preset: "quiet", Categories: map[string]config.NotifyCategory{
				string(CategoryDecisionPending): test.override,
			}}
			policy, err := ResolvePolicy(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := policy.For(CategoryDecisionPending); got != test.want {
				t.Fatalf("decision_pending route = %+v, want %+v", got, test.want)
			}
			for _, row := range policy.Table() {
				wantSource := "preset"
				if row.Category == CategoryDecisionPending {
					wantSource = "override"
				}
				if row.Source != wantSource {
					t.Fatalf("%s source = %q, want %q", row.Category, row.Source, wantSource)
				}
			}
			// Overriding one category must not disturb its neighbours.
			if got := policy.For(CategoryRequestOutcome); got != (CategoryPolicy{Desktop: "digest", Webhook: "immediate", Window: time.Hour}) {
				t.Fatalf("request_outcome route = %+v, want the untouched quiet preset row", got)
			}
			if test.want == quietDecisionPendingPreset {
				t.Fatalf("expectation %+v matches the preset row, so this case cannot detect a dropped override", test.want)
			}
		})
	}
}

// TestResolvePolicyCarriesScalarKnobs checks the non-category fields survive
// resolution unscaled, and that only digest_every has a fallback.
func TestResolvePolicyCarriesScalarKnobs(t *testing.T) {
	cfg := config.Notify{
		Preset:                   "milestones",
		MaxPerHour:               42,
		QuietMode:                "drop",
		DigestEveryMinutes:       90,
		CompletionQuietMinutes:   7,
		CompletionMaxHoldMinutes: 180,
		StallAfterMinutes:        25,
	}
	policy, err := ResolvePolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if policy.MaxPerHour != 42 || policy.QuietMode != "drop" {
		t.Fatalf("MaxPerHour/QuietMode = %d/%q, want 42/drop", policy.MaxPerHour, policy.QuietMode)
	}
	if policy.DigestEvery != 90*time.Minute || policy.CompletionQuiet != 7*time.Minute || policy.CompletionMaxHold != 180*time.Minute || policy.StallAfter != 25*time.Minute {
		t.Fatalf("durations = %s/%s/%s/%s, want 1h30m/7m/3h/25m", policy.DigestEvery, policy.CompletionQuiet, policy.CompletionMaxHold, policy.StallAfter)
	}
	zero, err := ResolvePolicy(config.Notify{})
	if err != nil {
		t.Fatal(err)
	}
	if zero.CompletionQuiet != 0 || zero.CompletionMaxHold != 0 || zero.StallAfter != 0 {
		t.Fatalf("unset holds = %s/%s/%s, want zero — only digest_every has a fallback", zero.CompletionQuiet, zero.CompletionMaxHold, zero.StallAfter)
	}
}
