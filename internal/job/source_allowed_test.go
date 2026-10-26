// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package job

import "testing"

// TestSourceAllowed pins the acquire --source/--deny-source contract: deny
// wins over allow, an empty allow admits everything, and a non-empty allow
// excludes anything it does not name.
func TestSourceAllowed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy Policy
		source string
		want   bool
	}{
		{name: "empty lists admit all", policy: Policy{}, source: "paid", want: true},
		{name: "allow names source", policy: Policy{SourcesAllow: []string{"a"}}, source: "a", want: true},
		{name: "allow excludes others", policy: Policy{SourcesAllow: []string{"a"}}, source: "b", want: false},
		{name: "deny excludes", policy: Policy{SourcesDeny: []string{"paid"}}, source: "paid", want: false},
		{name: "deny leaves others", policy: Policy{SourcesDeny: []string{"paid"}}, source: "free", want: true},
		{name: "deny wins over allow", policy: Policy{SourcesAllow: []string{"a", "b"}, SourcesDeny: []string{"a"}}, source: "a", want: false},
		{name: "allow survivor still admitted", policy: Policy{SourcesAllow: []string{"a", "b"}, SourcesDeny: []string{"a"}}, source: "b", want: true},
		{name: "named in neither with allow set", policy: Policy{SourcesAllow: []string{"a"}, SourcesDeny: []string{"b"}}, source: "c", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.SourceAllowed(tc.source); got != tc.want {
				t.Fatalf("SourceAllowed(%q) = %t, want %t (allow=%v deny=%v)", tc.source, got, tc.want, tc.policy.SourcesAllow, tc.policy.SourcesDeny)
			}
		})
	}
}
