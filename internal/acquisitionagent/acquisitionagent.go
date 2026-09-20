// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package acquisitionagent selects a bounded next action from a redacted browser
// observation. It does not execute actions, establish consent, or own a session.
// Callers must check observation freshness before applying any decision.
package acquisitionagent

import (
	"context"
	"errors"
	"regexp"
	"unicode/utf8"

	"papio/internal/work"
)

// Control is an observed control, identified by an opaque producer-assigned ID.
// Labels must already be redacted by the observation producer.
type Control struct {
	ID       string `json:"id"`
	Role     string `json:"role"`
	Label    string `json:"label"`
	Disabled bool   `json:"disabled"`
}

// Observation carries only the allowed decision evidence, never page URLs,
// bodies, input values, selectors, or credentials. The producer owns redaction
// of title and label prose; Validate is a schema check, not a PII detector.
type Observation struct {
	Revision string    `json:"revision"`
	DOI      string    `json:"doi"`
	Title    string    `json:"title"`
	Controls []Control `json:"controls"`
}

// Decision selects WAIT, BLOCKED, or one enabled observed control ID.
// Model and token counts describe the backend's actual response, if applicable.
type Decision struct {
	Choice       string
	Model        string
	InputTokens  int64
	OutputTokens int64
}

// Backend makes one decision. Implementations must honor context cancellation.
// A caller may inject a local implementation without configuring a cloud client.
type Backend interface {
	Decide(context.Context, Observation) (Decision, error)
}

var (
	controlIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	revisionPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Validate requires an explicit 64-lowercase-hex revision, a DOI of at most 512
// bytes already normalized by work.NormalizeDOI, at most 400 Unicode code
// points in the title, and at most
// 80 controls. Each label allows at most 240 Unicode code points. Empty titles,
// labels, and control lists are permitted. IDs are unique ASCII opaque tokens.
// Allowed roles are button, link, menuitem, tab. WAIT, BLOCKED, __proto__,
// constructor, and prototype are reserved IDs.
func (o Observation) Validate() error {
	if !revisionPattern.MatchString(o.Revision) {
		return errors.New("acquisitionagent: invalid observation revision")
	}
	if len(o.DOI) > 512 {
		return errors.New("acquisitionagent: observation DOI exceeds 512 bytes")
	}
	doi, err := work.NormalizeDOI(o.DOI)
	if err != nil || doi != o.DOI || !utf8.ValidString(o.DOI) {
		// NormalizeDOI errors quote the input; never propagate them here.
		return errors.New("acquisitionagent: observation DOI must be valid and normalized")
	}
	if !boundedText(o.Title, 400) {
		return errors.New("acquisitionagent: invalid observation title")
	}
	if len(o.Controls) > 80 {
		return errors.New("acquisitionagent: too many observation controls")
	}
	seen := make(map[string]bool, len(o.Controls))
	for _, c := range o.Controls {
		if !controlIDPattern.MatchString(c.ID) || reservedID(c.ID) || seen[c.ID] {
			return errors.New("acquisitionagent: invalid, reserved, or duplicate control ID")
		}
		seen[c.ID] = true
		switch c.Role {
		case "button", "link", "menuitem", "tab":
		default:
			return errors.New("acquisitionagent: invalid control role")
		}
		if !boundedText(c.Label, 240) {
			return errors.New("acquisitionagent: invalid control label")
		}
	}
	return nil
}

func reservedID(id string) bool {
	switch id {
	case "WAIT", "BLOCKED", "__proto__", "constructor", "prototype":
		return true
	default:
		return false
	}
}

func boundedText(s string, limit int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= limit
}

// ValidateDecision checks the observation and ensures the decision is WAIT,
// BLOCKED, or an enabled ID in that observation. It is backend-independent:
// local implementations need not report a cloud model or token usage.
// It cannot establish that the browser still has this observation's revision.
func ValidateDecision(o Observation, d Decision) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if d.Choice == "WAIT" || d.Choice == "BLOCKED" {
		return nil
	}
	for _, c := range o.Controls {
		if c.ID == d.Choice && !c.Disabled {
			return nil
		}
	}
	return errors.New("acquisitionagent: decision must name an enabled observed control, WAIT, or BLOCKED")
}
