// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import "testing"

// A per-page watermark repeated many times is volume, not signal: distinct
// content must stay below the threshold that would skip OCR.
func TestUniqueTextCharsCollapsesRepeatedBoilerplate(t *testing.T) {
	line := "Reproduced with permission of the copyright owner. Further reproduction prohibited without permission.\n"
	var text string
	for i := 0; i < 13; i++ {
		text += line
	}
	if got := uniqueTextChars(text); got >= 400 {
		t.Fatalf("13 repeated watermark lines must not pass the 400-char signal gate: %d", got)
	}
	distinct := "Trust in Automation: Designing for Appropriate Reliance\nJohn D. Lee and Katrina A. See\n" + text
	if got, want := uniqueTextChars(distinct), uniqueTextChars(text); got <= want {
		t.Fatalf("distinct lines must add signal: %d <= %d", got, want)
	}
}
