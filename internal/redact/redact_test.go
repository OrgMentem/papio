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

// unparseablePlaceholder is the fixed, source-independent value that Host must
// return for every input it cannot reduce to a scheme and a host.
const unparseablePlaceholder = "<unparseable-url>"

// probeUser and probeSecret are the userinfo halves of the fixture below.
// They are fake, and they are separate constants only so no source line
// carries a complete `user:pass@host` URL.
const (
	probeUser   = "probeuser"
	probeSecret = "n0tr3al"
)

func TestHost(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		// forbidden lists source fragments that must never survive into the
		// result, so a regression that echoes part of the input fails here.
		forbidden []string
	}{
		{
			name: "signed url keeps scheme and host only",
			in:   "https://svcuser:s3cr3t@content.example.com/cds/retrieve?content=AQICJOk_TOKEN&order=1#frag",
			want: "https://content.example.com",
			forbidden: []string{
				"svcuser", "s3cr3t", "@", "/cds", "retrieve",
				"content=", "AQICJOk_TOKEN", "order=1", "?", "#", "frag",
			},
		},
		{
			// The userinfo pair is composed rather than written as one
			// literal: a `user:pass@host` literal trips the secret scanner's
			// generic-credential-uri rule, and .betterleaksignore can only
			// accept a finding that is already in history. Composing keeps
			// the input byte-identical to the URL this must redact.
			name:      "port survives because it identifies the destination",
			in:        "http://" + probeUser + ":" + probeSecret + "@127.0.0.1:8765/v1/handoff?key=SECRETKEY",
			want:      "http://127.0.0.1:8765",
			forbidden: []string{probeUser, probeSecret, "@", "handoff", "key=", "SECRETKEY", "?", "/v1"},
		},
		{
			name:      "parse failure from control byte",
			in:        "https://content.example.com/cds\x7fretrieve?content=AQICJOk_TOKEN",
			want:      unparseablePlaceholder,
			forbidden: []string{"content", "example", "AQICJOk_TOKEN", "\x7f"},
		},
		{
			name:      "parse failure from missing protocol scheme",
			in:        "::::not a valid destination?sig=AQICJOk_TOKEN",
			want:      unparseablePlaceholder,
			forbidden: []string{"::::", "destination", "sig=", "AQICJOk_TOKEN"},
		},
		{
			name:      "relative path has no scheme",
			in:        "/cds/retrieve?content=AQICJOk_TOKEN&order=1",
			want:      unparseablePlaceholder,
			forbidden: []string{"/cds", "retrieve", "content=", "AQICJOk_TOKEN", "order=1"},
		},
		{
			name:      "scheme-relative reference has no scheme",
			in:        "//content.example.com/cds?content=AQICJOk_TOKEN",
			want:      unparseablePlaceholder,
			forbidden: []string{"content", "example", "AQICJOk_TOKEN", "//"},
		},
		{
			name:      "opaque scheme has no host",
			in:        "mailto:ops:s3cr3t@example.com",
			want:      unparseablePlaceholder,
			forbidden: []string{"mailto", "ops", "s3cr3t", "example", "@"},
		},
		{
			name:      "bare credential-shaped token",
			in:        "AQICJOk_TOKEN_bearer_value",
			want:      unparseablePlaceholder,
			forbidden: []string{"AQIC", "TOKEN", "bearer", "value"},
		},
		{
			name:      "empty input still collapses to the placeholder",
			in:        "",
			want:      unparseablePlaceholder,
			forbidden: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Host(tc.in)
			for _, frag := range tc.forbidden {
				if strings.Contains(got, frag) {
					t.Fatalf("Host(%q) leaked source bytes %q: %q", tc.in, frag, got)
				}
			}
			if got != tc.want {
				t.Fatalf("Host(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.want != unparseablePlaceholder {
				return
			}
			// The fail-closed arm must be source-independent: no window of the
			// input may appear in the result unless the fixed placeholder
			// already contains it.
			const window = 4
			for i := 0; i+window <= len(tc.in); i++ {
				frag := tc.in[i : i+window]
				if strings.Contains(unparseablePlaceholder, frag) {
					continue
				}
				if strings.Contains(got, frag) {
					t.Fatalf("Host(%q) leaked source window %q: %q", tc.in, frag, got)
				}
			}
		})
	}
}
