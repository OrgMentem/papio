// Copyright 2026 OrgMentem. Licensed under MIT.

package notify

import (
	"fmt"
	"strings"
	"time"

	"papio/internal/config"
)

type CategoryPolicy struct {
	Desktop string
	Webhook string
	Window  time.Duration
}

type QuietHours struct {
	Start    time.Duration
	End      time.Duration
	Location *time.Location
}

func (q QuietHours) Enabled() bool { return q.Location != nil }

// Contains uses local civil minutes. This intentionally compares the wall
// clock, not elapsed UTC time: a spring-forward gap is skipped naturally and
// a fall-back overlap is one civil interval, so its release occurs once.
func (q QuietHours) Contains(at time.Time) bool {
	if !q.Enabled() {
		return false
	}
	local := at.In(q.Location)
	minute := time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute
	if q.Start < q.End {
		return minute >= q.Start && minute < q.End
	}
	return minute >= q.Start || minute < q.End
}

func ParseQuietHours(value string, location *time.Location) (QuietHours, error) {
	if value == "" {
		return QuietHours{}, nil
	}
	if location == nil {
		location = time.Local
	}
	if len(value) != 11 || value[2] != ':' || value[5] != '-' || value[8] != ':' {
		return QuietHours{}, fmt.Errorf("quiet hours must use HH:MM-HH:MM")
	}
	parse := func(text string) (time.Duration, error) {
		if text[0] < '0' || text[0] > '9' || text[1] < '0' || text[1] > '9' || text[3] < '0' || text[3] > '9' || text[4] < '0' || text[4] > '9' {
			return 0, fmt.Errorf("quiet hours must use HH:MM-HH:MM")
		}
		hour := int(text[0]-'0')*10 + int(text[1]-'0')
		minute := int(text[3]-'0')*10 + int(text[4]-'0')
		if hour > 23 || minute > 59 {
			return 0, fmt.Errorf("quiet hours contain an invalid local time")
		}
		return time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute, nil
	}
	start, err := parse(value[:5])
	if err != nil {
		return QuietHours{}, err
	}
	end, err := parse(value[6:])
	if err != nil {
		return QuietHours{}, err
	}
	if start == end {
		return QuietHours{}, fmt.Errorf("quiet hours start and end must differ")
	}
	return QuietHours{Start: start, End: end, Location: location}, nil
}

type Policy struct {
	Preset            string
	Categories        map[Category]CategoryPolicy
	Sources           map[Category]string
	MaxPerHour        int
	QuietHours        QuietHours
	QuietMode         string
	DigestEvery       time.Duration
	CompletionQuiet   time.Duration
	CompletionMaxHold time.Duration
	StallAfter        time.Duration
}

func defaultPolicy(preset string) map[Category]CategoryPolicy {
	if preset == "" {
		preset = "milestones"
	}
	const min = time.Minute
	out := make(map[Category]CategoryPolicy, len(Categories()))
	for _, category := range Categories() {
		out[category] = CategoryPolicy{Desktop: "immediate", Webhook: "immediate", Window: 60 * min}
	}
	switch preset {
	case "quiet":
		out[CategoryRequestOutcome] = CategoryPolicy{Desktop: "digest", Webhook: "immediate", Window: 60 * min}
		out[CategoryDecisionOpened] = CategoryPolicy{Desktop: "digest", Webhook: "immediate", Window: 5 * min}
		out[CategoryDecisionPending] = CategoryPolicy{Desktop: "off", Webhook: "immediate", Window: 4 * time.Hour}
		batch := out[CategoryCompletionBatch]
		batch.Desktop = "off"
		out[CategoryCompletionBatch] = batch
		discovery := out[CategoryDiscoveryNew]
		discovery.Desktop = "off"
		out[CategoryDiscoveryNew] = discovery
	case "milestones":
		out[CategoryDecisionOpened] = CategoryPolicy{Desktop: "immediate", Webhook: "immediate", Window: 5 * min}
		out[CategoryDecisionPending] = CategoryPolicy{Desktop: "digest", Webhook: "immediate", Window: 4 * time.Hour}
		out[CategoryDiscoveryNew] = CategoryPolicy{Desktop: "digest", Webhook: "immediate", Window: 60 * min}
	case "verbose":
		out[CategoryDecisionOpened] = CategoryPolicy{Desktop: "immediate", Webhook: "immediate", Window: min}
		out[CategoryDecisionPending] = CategoryPolicy{Desktop: "immediate", Webhook: "immediate", Window: min}
		out[CategoryDiscoveryNew] = CategoryPolicy{Desktop: "immediate", Webhook: "immediate", Window: min}
	default:
		return nil
	}
	return out
}

func ResolvePolicy(cfg config.Notify) (Policy, error) {
	preset := cfg.Preset
	if preset == "" {
		preset = "milestones"
	}
	categories := defaultPolicy(preset)
	if categories == nil {
		return Policy{}, fmt.Errorf("unknown notification preset %q", preset)
	}
	sources := make(map[Category]string, len(categories))
	for category := range categories {
		sources[category] = "preset"
	}
	for name, override := range cfg.Categories {
		category := Category(name)
		if !isKnownCategory(category) {
			return Policy{}, fmt.Errorf("unknown notification category %q", name)
		}
		p := categories[category]
		if override.Desktop != "" {
			p.Desktop = override.Desktop
		}
		if override.Webhook != "" {
			p.Webhook = override.Webhook
		}
		if override.WindowSeconds > 0 {
			p.Window = time.Duration(override.WindowSeconds) * time.Second
		}
		categories[category] = p
		sources[category] = "override"
	}
	quiet, err := ParseQuietHours(cfg.QuietHours, time.Local)
	if err != nil {
		return Policy{}, err
	}
	digest := time.Duration(cfg.DigestEveryMinutes) * time.Minute
	if digest <= 0 {
		digest = 4 * time.Hour
	}
	completionQuiet := time.Duration(cfg.CompletionQuietMinutes) * time.Minute
	completionMax := time.Duration(cfg.CompletionMaxHoldMinutes) * time.Minute
	stall := time.Duration(cfg.StallAfterMinutes) * time.Minute
	return Policy{Preset: preset, Categories: categories, Sources: sources, MaxPerHour: cfg.MaxPerHour, QuietHours: quiet, QuietMode: cfg.QuietMode, DigestEvery: digest, CompletionQuiet: completionQuiet, CompletionMaxHold: completionMax, StallAfter: stall}, nil
}

func NewPolicy(cfg config.Notify) (Policy, error)        { return ResolvePolicy(cfg) }
func PolicyFromConfig(cfg config.Notify) (Policy, error) { return ResolvePolicy(cfg) }

func (p Policy) For(category Category) CategoryPolicy {
	if value, ok := p.Categories[category]; ok {
		return value
	}
	return CategoryPolicy{Desktop: "off", Webhook: "off", Window: time.Minute}
}

func isKnownCategory(category Category) bool {
	for _, known := range Categories() {
		if known == category {
			return true
		}
	}
	return false
}

func (p Policy) Table() []RouteRow {
	rows := make([]RouteRow, 0, len(Categories()))
	for _, category := range Categories() {
		value := p.For(category)
		source := p.Sources[category]
		if source == "" {
			source = "preset"
		}
		rows = append(rows, RouteRow{Category: category, Desktop: value.Desktop, Webhook: value.Webhook, Window: value.Window, Source: source})
	}
	return rows
}

func (p Policy) String() string {
	parts := make([]string, 0, len(Categories()))
	for _, row := range p.Table() {
		parts = append(parts, fmt.Sprintf("%s desktop=%s webhook=%s window=%s", row.Category, row.Desktop, row.Webhook, row.Window))
	}
	return strings.Join(parts, "\n")
}
