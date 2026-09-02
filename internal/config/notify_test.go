// Copyright 2026 OrgMentem. Licensed under MIT.

package config

import (
	"math"
	"strings"
	"testing"
)

func TestNotifyDefaultsAndClosedCategoryValidation(t *testing.T) {
	cfg := Default()
	if cfg.Notify.Preset != "milestones" || cfg.Notify.MaxPerHour != 6 || cfg.Notify.DigestEveryMinutes != 240 || cfg.Notify.QuietMode != "hold" {
		t.Fatalf("defaults = %+v", cfg.Notify)
	}
	cfg.Notify.Categories = map[string]NotifyCategory{"typo": {Desktop: "immediate"}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "not a recognized category") {
		t.Fatalf("unknown category err = %v", err)
	}
	cfg.Notify.Categories = map[string]NotifyCategory{"decision_opened": {Desktop: "bogus"}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "desktop") {
		t.Fatalf("unknown desktop err = %v", err)
	}
}

func TestNotifyQuietHoursValidation(t *testing.T) {
	cfg := Default()
	cfg.Notify.QuietHours = "22:30-06:15"
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Notify.QuietHours = "25:00-06:15"
	if err := cfg.validate(); err == nil {
		t.Fatal("invalid quiet hours accepted")
	}
}

// TestNotifyValidationRejectsUnknownEnumsAndOutOfRangeBounds walks every rule
// in validateNotify. Each of these knobs decides whether a notification is
// delivered, held, or dropped, so an unvalidated value does not surface as a
// startup error: it silently loses or floods notifications at runtime.
func TestNotifyValidationRejectsUnknownEnumsAndOutOfRangeBounds(t *testing.T) {
	base := func() Config {
		cfg := Default()
		cfg.AccessMode = ModeConservative
		return cfg
	}
	mustAccept := func(t *testing.T, notify Notify) {
		t.Helper()
		cfg := base()
		cfg.Notify = notify
		if err := cfg.validate(); err != nil {
			t.Fatalf("expected valid notify config, got error: %v", err)
		}
	}
	mustReject := func(t *testing.T, notify Notify, wantErr string) {
		t.Helper()
		cfg := base()
		cfg.Notify = notify
		err := cfg.validate()
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("validate err = %v, want an error containing %q", err, wantErr)
		}
	}
	// with returns the default notify block with one field replaced, so each
	// case isolates exactly the rule it names.
	with := func(mutate func(*Notify)) Notify {
		notify := Default().Notify
		mutate(&notify)
		return notify
	}

	t.Run("notify.preset", func(t *testing.T) {
		for _, value := range []string{"", "quiet", "milestones", "verbose"} {
			mustAccept(t, with(func(n *Notify) { n.Preset = value }))
		}
		for _, value := range []string{"loud", "Quiet", "milestone", "off"} {
			mustReject(t, with(func(n *Notify) { n.Preset = value }), "notify.preset must be quiet, milestones, or verbose")
		}
	})

	t.Run("notify.max_per_hour", func(t *testing.T) {
		for _, value := range []int{0, 1, 10000} {
			mustAccept(t, with(func(n *Notify) { n.MaxPerHour = value }))
		}
		for _, value := range []int{-1, 10001, math.MaxInt} {
			mustReject(t, with(func(n *Notify) { n.MaxPerHour = value }), "notify.max_per_hour must be in 0..10000")
		}
	})

	t.Run("notify.quiet_mode", func(t *testing.T) {
		for _, value := range []string{"", "hold", "drop"} {
			mustAccept(t, with(func(n *Notify) { n.QuietMode = value }))
		}
		for _, value := range []string{"mute", "Hold", "defer"} {
			mustReject(t, with(func(n *Notify) { n.QuietMode = value }), "notify.quiet_mode must be hold or drop")
		}
	})

	t.Run("notify.digest_every_minutes", func(t *testing.T) {
		for _, value := range []int{0, 1, 10080} {
			mustAccept(t, with(func(n *Notify) { n.DigestEveryMinutes = value }))
		}
		for _, value := range []int{-1, 10081, math.MaxInt} {
			mustReject(t, with(func(n *Notify) { n.DigestEveryMinutes = value }), "notify.digest_every_minutes must be in 0..10080")
		}
	})

	t.Run("notify.completion_quiet_minutes", func(t *testing.T) {
		// The ceiling rises with the quiet window so this case tests the range
		// rule alone, not the quiet-exceeds-hold invariant below.
		for _, value := range []int{0, 1, 1440} {
			mustAccept(t, with(func(n *Notify) { n.CompletionQuietMinutes, n.CompletionMaxHoldMinutes = value, 1440 }))
		}
		for _, value := range []int{-1, 1441, math.MaxInt} {
			mustReject(t, with(func(n *Notify) { n.CompletionQuietMinutes, n.CompletionMaxHoldMinutes = value, 10080 }), "notify.completion_quiet_minutes must be in 0..1440")
		}
	})

	t.Run("notify.completion_max_hold_minutes", func(t *testing.T) {
		for _, value := range []int{0, 10, 10080} {
			mustAccept(t, with(func(n *Notify) { n.CompletionQuietMinutes, n.CompletionMaxHoldMinutes = 10, value }))
		}
		for _, value := range []int{-1, 10081, math.MaxInt} {
			mustReject(t, with(func(n *Notify) { n.CompletionMaxHoldMinutes = value }), "notify.completion_max_hold_minutes must be in 0..10080")
		}
	})

	t.Run("notify.stall_after_minutes", func(t *testing.T) {
		for _, value := range []int{0, 1, 10080} {
			mustAccept(t, with(func(n *Notify) { n.StallAfterMinutes = value }))
		}
		for _, value := range []int{-1, 10081, math.MaxInt} {
			mustReject(t, with(func(n *Notify) { n.StallAfterMinutes = value }), "notify.stall_after_minutes must be in 0..10080")
		}
	})

	t.Run("notify.categories.window_seconds", func(t *testing.T) {
		category := func(seconds int) Notify {
			return with(func(n *Notify) {
				n.Categories = map[string]NotifyCategory{"discovery_new": {Desktop: "digest", WindowSeconds: seconds}}
			})
		}
		for _, value := range []int{0, 1, 86400} {
			mustAccept(t, category(value))
		}
		for _, value := range []int{-1, 86401, math.MaxInt} {
			mustReject(t, category(value), "notify.categories.discovery_new.window_seconds must be in 0..86400")
		}
	})

	t.Run("notify.categories modes", func(t *testing.T) {
		mode := func(desktop, webhook string) Notify {
			return with(func(n *Notify) {
				n.Categories = map[string]NotifyCategory{"decision_pending": {Desktop: desktop, Webhook: webhook}}
			})
		}
		for _, value := range []string{"", "off", "digest", "immediate"} {
			mustAccept(t, mode(value, value))
		}
		for _, value := range []string{"loud", "Immediate", "on"} {
			mustReject(t, mode(value, "immediate"), "notify.categories.decision_pending.desktop must be off, digest, or immediate")
			mustReject(t, mode("immediate", value), "notify.categories.decision_pending.webhook must be off, digest, or immediate")
		}
	})

	t.Run("completion quiet cannot exceed max hold", func(t *testing.T) {
		mustAccept(t, with(func(n *Notify) { n.CompletionQuietMinutes, n.CompletionMaxHoldMinutes = 60, 60 }))
		// A zero ceiling disables the hold limit, so the invariant does not apply.
		mustAccept(t, with(func(n *Notify) { n.CompletionQuietMinutes, n.CompletionMaxHoldMinutes = 120, 0 }))
		mustReject(t, with(func(n *Notify) { n.CompletionQuietMinutes, n.CompletionMaxHoldMinutes = 61, 60 }),
			"notify.completion_quiet_minutes cannot exceed completion_max_hold_minutes")
		mustReject(t, with(func(n *Notify) { n.CompletionQuietMinutes, n.CompletionMaxHoldMinutes = 1440, 1 }),
			"notify.completion_quiet_minutes cannot exceed completion_max_hold_minutes")
	})
}
