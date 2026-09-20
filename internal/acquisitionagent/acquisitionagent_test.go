// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package acquisitionagent

import (
	"fmt"
	"strings"
	"testing"
)

func observation() Observation {
	return Observation{
		Revision: strings.Repeat("0123456789abcdef", 4),
		DOI:      "10.1234/main-article",
		Title:    "An example article",
		Controls: []Control{
			{ID: "c_1", Role: "link", Label: "Download PDF"},
			{ID: "c_2", Role: "button", Label: "Unavailable", Disabled: true},
		},
	}
}

func TestObservationValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Observation)
	}{
		{"missing revision", func(o *Observation) { o.Revision = "" }},
		{"short revision", func(o *Observation) { o.Revision = strings.Repeat("a", 63) }},
		{"long revision", func(o *Observation) { o.Revision += "a" }},
		{"nonhex revision", func(o *Observation) { o.Revision = strings.Repeat("g", 64) }},
		{"uppercase revision", func(o *Observation) { o.Revision = strings.ToUpper(o.Revision) }},
		{"missing DOI", func(o *Observation) { o.DOI = "" }},
		{"invalid DOI", func(o *Observation) { o.DOI = "private-invalid-doi" }},
		{"DOI URL", func(o *Observation) { o.DOI = "https://doi.org/" + o.DOI }},
		{"DOI prefix", func(o *Observation) { o.DOI = "doi:" + o.DOI }},
		{"uppercase DOI", func(o *Observation) { o.DOI = strings.ToUpper(o.DOI) }},
		{"DOI whitespace", func(o *Observation) { o.DOI += " " }},
		{"DOI punctuation", func(o *Observation) { o.DOI += "." }},
		{"encoded DOI slash", func(o *Observation) { o.DOI = "10.1234/foo%2fbar" }},
		{"long DOI", func(o *Observation) { o.DOI = "10.1234/" + strings.Repeat("界", 169) }},
		{"long title", func(o *Observation) { o.Title = strings.Repeat("界", 401) }},
		{"invalid UTF8 title", func(o *Observation) { o.Title = "\xff" }},
		{"empty ID", func(o *Observation) { o.Controls[0].ID = "" }},
		{"long ID", func(o *Observation) { o.Controls[0].ID = strings.Repeat("a", 65) }},
		{"ID URL", func(o *Observation) { o.Controls[0].ID = "https://example.test/pdf" }},
		{"ID space", func(o *Observation) { o.Controls[0].ID = "control id" }},
		{"ID unicode", func(o *Observation) { o.Controls[0].ID = "界" }},
		{"duplicate ID", func(o *Observation) { o.Controls[1].ID = o.Controls[0].ID }},
		{"long label", func(o *Observation) { o.Controls[0].Label = strings.Repeat("界", 241) }},
		{"invalid UTF8 label", func(o *Observation) { o.Controls[0].Label = "\xff" }},
		{"too many controls", func(o *Observation) { o.Controls = manyControls(81) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := observation()
			tt.change(&o)
			if err := o.Validate(); err == nil {
				t.Fatal("invalid observation accepted")
			} else if o.DOI != "" && strings.Contains(err.Error(), o.DOI) {
				t.Fatal("validation error disclosed DOI")
			}
		})
	}
	for _, id := range []string{"WAIT", "BLOCKED", "__proto__", "constructor", "prototype"} {
		t.Run("reserved "+id, func(t *testing.T) {
			o := observation()
			o.Controls[0].ID = id
			if err := o.Validate(); err == nil {
				t.Fatal("reserved ID accepted")
			}
		})
	}
	for _, role := range []string{"", "script", "BUTTON", "option", "checkbox", "radio", "switch", "textbox"} {
		t.Run("role "+role, func(t *testing.T) {
			o := observation()
			o.Controls[0].Role = role
			if err := o.Validate(); err == nil {
				t.Fatal("unknown role accepted")
			}
		})
	}
}

func manyControls(n int) []Control {
	controls := make([]Control, n)
	for i := range controls {
		controls[i] = Control{ID: fmt.Sprintf("c%d", i), Role: "button"}
	}
	return controls
}

func TestObservationValidateBoundaries(t *testing.T) {
	o := observation()
	o.Title = strings.Repeat("界", 400)
	o.Controls = manyControls(80)
	o.Controls[0].ID = strings.Repeat("a", 64)
	o.Controls[0].Label = strings.Repeat("界", 240)
	o.DOI = "10.48612//monograph-2025-2"
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"button", "link", "menuitem", "tab"} {
		o.Controls[0].Role = role
		if err := o.Validate(); err != nil {
			t.Fatalf("role %s: %v", role, err)
		}
	}
	// 8-byte prefix + 168 three-byte runes: valid under work.NormalizeDOI
	// and exactly the explicit cloud observation byte limit.
	o.DOI = "10.1234/" + strings.Repeat("界", 168)
	if len(o.DOI) != 512 {
		t.Fatal("bad DOI boundary fixture")
	}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	o.Title, o.Controls = "", nil
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestObservationDoesNotGuessPIIFromProse(t *testing.T) {
	o := observation()
	o.Title = "Email alice@example.test: a study of identifiers"
	o.Controls[0].Label = "Report on password and credit card terminology"
	if err := o.Validate(); err != nil {
		t.Fatalf("schema validation guessed at prose contents: %v", err)
	}
}

func TestValidateDecision(t *testing.T) {
	o := observation()
	for _, choice := range []string{"WAIT", "BLOCKED", "c_1"} {
		if err := ValidateDecision(o, Decision{Choice: choice}); err != nil {
			t.Fatalf("%s: %v", choice, err)
		}
	}
	for _, choice := range []string{"", "c_2", "c_3", "wait", "c_1 ", "https://example.test/pdf"} {
		if err := ValidateDecision(o, Decision{Choice: choice}); err == nil {
			t.Fatalf("accepted %q", choice)
		}
	}
	o.Revision = ""
	if err := ValidateDecision(o, Decision{Choice: "WAIT"}); err == nil {
		t.Fatal("accepted invalid observation for WAIT")
	}
}
