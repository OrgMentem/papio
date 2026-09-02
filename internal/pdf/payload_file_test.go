// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package pdf

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The file gate must reach byte-for-byte the same verdict as the in-memory
// gate, and both must reach the verdict named per case: a container that only
// claims to be a PDF (HTML, an ID3 audio stream) never acquires a header, and
// a short or late-EOF payload is rejected despite having one.
func TestValidatePayloadFileMatchesByteGate(t *testing.T) {
	cases := []struct {
		name       string
		body       []byte
		wantOK     bool
		wantHeader bool
	}{
		{"valid-with-eof", append(append([]byte("%PDF-1.7\n"), make([]byte, MinimumPayloadBytes)...), []byte("%%EOF")...), true, true},
		{"valid-without-eof", append([]byte("%PDF-1.7\n"), make([]byte, MinimumPayloadBytes)...), true, true},
		{"short", []byte("%PDF-1.7\nshort"), false, true},
		{"html", append([]byte("<html>"), make([]byte, MinimumPayloadBytes)...), false, false},
		{"early-eof", append(append([]byte("%PDF-1.7\n%%EOF"), make([]byte, eofSearchBytes+100)...), []byte("trailer")...), false, true},
		{"audio-stream", append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte{0}, MinimumPayloadBytes)...), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.pdf")
			if err := os.WriteFile(path, tc.body, 0o600); err != nil {
				t.Fatal(err)
			}
			fileReport, err := ValidatePayloadFile(path, "application/pdf")
			if err != nil {
				t.Fatal(err)
			}
			byteReport := ValidatePayload(tc.body, "application/pdf")
			if fileReport.OK != byteReport.OK || fileReport.HasHeader != byteReport.HasHeader ||
				fileReport.HasEOF != byteReport.HasEOF || fileReport.SizeBytes != byteReport.SizeBytes || fileReport.Reason != byteReport.Reason {
				t.Fatalf("file report %+v != byte report %+v", fileReport, byteReport)
			}
			if byteReport.OK != tc.wantOK || byteReport.HasHeader != tc.wantHeader {
				t.Fatalf("report = %+v, want OK=%v HasHeader=%v", byteReport, tc.wantOK, tc.wantHeader)
			}
		})
	}
}

func TestValidateUsesPathGateWithoutWorkerForInvalidPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-pdf.pdf")
	if err := os.WriteFile(path, append([]byte("<html>"), make([]byte, MinimumPayloadBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Validate(context.Background(), ValidationInput{Path: path, DeclaredMIME: "application/pdf"}, ValidationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Payload.OK || report.Structural.Valid {
		t.Fatalf("invalid file reached structural worker: %+v", report)
	}
}

// The only thing ValidateFile adds over Validate is refusing to start without
// a worker, so that a missing worker is a configuration error rather than a
// structural-stage failure on an otherwise valid file.
func TestValidateFileRequiresAWorkerBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "real.pdf")
	if err := os.WriteFile(path, append([]byte("%PDF-1.7\n"), make([]byte, MinimumPayloadBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateFile(context.Background(), ValidationInput{Path: path, DeclaredMIME: "application/pdf"}, ValidationOptions{}); err == nil {
		t.Fatal("validation ran without a worker binary")
	}
}

func TestValidatePayloadFileRejectsDirectories(t *testing.T) {
	if _, err := ValidatePayloadFile(t.TempDir(), "application/pdf"); err == nil {
		t.Fatal("directory accepted as PDF payload")
	}
}
