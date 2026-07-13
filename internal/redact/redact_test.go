// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package redact

import (
	"strings"
	"testing"
)

func TestURLStripsSecrets(t *testing.T) {
	signed := "https://content.example.com/cds/retrieve?content=AQICJOk_TOKEN&order=1#frag"
	got := URL(signed)
	if strings.Contains(got, "TOKEN") || strings.Contains(got, "AQIC") || strings.Contains(got, "frag") {
		t.Fatalf("redacted URL leaks secrets: %q", got)
	}
	if !strings.HasPrefix(got, "https://content.example.com/cds/retrieve") {
		t.Fatalf("redacted URL lost host/path evidence: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("redacted URL does not mark removal: %q", got)
	}
	if URL("https://user:pass@example.com/x") != "https://example.com/x" {
		t.Fatalf("userinfo survived: %q", URL("https://user:pass@example.com/x"))
	}
	if URL("::::not a url") != "<unparseable-url>" {
		t.Fatalf("unparseable input leaked: %q", URL("::::not a url"))
	}
	if URL("") != "" {
		t.Fatal("empty input should stay empty")
	}
}
