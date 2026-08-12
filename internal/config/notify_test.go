// Copyright 2026 OrgMentem. Licensed under MIT.

package config

import (
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
