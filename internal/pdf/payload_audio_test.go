// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package pdf

import (
	"bytes"
	"testing"
)

func TestPayloadRejectsAudioClaimingPDF(t *testing.T) {
	body := append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0}, MinimumPayloadBytes)...)
	report := ValidatePayload(body, "application/pdf")
	if report.OK || report.HasHeader {
		t.Fatalf("audio accepted as PDF: %+v", report)
	}
}
