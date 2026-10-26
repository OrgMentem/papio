// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import "testing"

// TestInsufficientIdentityAuthorityBoundary pins the sparse-input boundary:
// title plus any ONE of year, authors, or a strong identifier is sufficient
// authority, while a bare title is not.
func TestInsufficientIdentityAuthorityBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		identity SubmittedIdentity
		want     bool
	}{
		{name: "title only", identity: SubmittedIdentity{Attested: true, Fields: map[string]bool{"title": true}}, want: true},
		{name: "title and year", identity: SubmittedIdentity{Attested: true, Fields: map[string]bool{"title": true, "year": true}}, want: false},
		{name: "title and authors", identity: SubmittedIdentity{Attested: true, Fields: map[string]bool{"title": true, "authors": true}}, want: false},
		{name: "title and doi field", identity: SubmittedIdentity{Attested: true, Fields: map[string]bool{"title": true, "doi": true}}, want: false},
		{
			name: "title with submitted doi identifier",
			identity: SubmittedIdentity{Attested: true, Fields: map[string]bool{"title": true},
				Identifiers: []Identifier{{Kind: "doi", Value: "10.1000/sufficient", Provenance: ProvenanceSubmitted}}},
			want: false,
		},
		{name: "unattested", identity: SubmittedIdentity{Fields: map[string]bool{"title": true}}, want: false},
		{name: "no title", identity: SubmittedIdentity{Attested: true, Fields: map[string]bool{"year": true}}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.identity.InsufficientIdentityAuthority(); got != tc.want {
				t.Fatalf("InsufficientIdentityAuthority() = %t, want %t (%+v)", got, tc.want, tc.identity)
			}
		})
	}
}
