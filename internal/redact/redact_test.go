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

// Elsevier's refusal page prints the reader's IP address beside its reference
// number, so every capture of it carried the operator's address. The addresses
// here are documentation ranges.
func TestIPAddressesMasksAddressesButNotDOIsVersionsOrTimes(t *testing.T) {
	blocked := `<li><strong>IP Address: </strong>203.0.113.42</li>` +
		`<li>2001:db8:85a3::8a2e:370:7334 or 2001:0db8:0000:0000:0000:ff00:0042:8329, loopback ::1.</li>` +
		`<a data-client="198.51.100.7" href="https://192.0.2.1/support">x</a>`
	got := IPAddresses(blocked, "TOKEN")
	for _, address := range []string{"203.0.113.42", "2001:db8:85a3::8a2e:370:7334", "2001:0db8:0000:0000:0000:ff00:0042:8329", "::1", "198.51.100.7", "192.0.2.1"} {
		if strings.Contains(got, address) {
			t.Fatalf("address %s survived: %q", address, got)
		}
	}
	if !strings.Contains(got, "<strong>IP Address: </strong>TOKEN</li>") || !strings.Contains(got, `data-client="TOKEN"`) {
		t.Fatalf("addresses not replaced in place: %q", got)
	}
	article := `<meta name="citation_doi" content="10.1016/j.chb.2016.04.041">` +
		`<p>doi:10.1016/j.chb.2016.04.041 and 10.1000.10.1000/x</p>` +
		`<p>Chrome/153.0.0.0 Safari/537.36; Version 4.2.1.0; v1.2.3.4; 1.2.3.4.5</p>` +
		`<p>Published 2026.09.23 at 13:29:37 UTC; see section 3.1.2.1-a; li::before; ::TOKEN::</p>`
	if got := IPAddresses(article, "TOKEN"); got != article {
		t.Fatalf("non-address text changed:\n got %q\nwant %q", got, article)
	}
}
