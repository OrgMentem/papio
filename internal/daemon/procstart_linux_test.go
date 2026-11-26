// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package daemon

import "testing"

// The command name is parenthesized and can hold spaces and parentheses, so
// the start time is counted from the last ")".
func TestParseStatStartSurvivesAnAwkwardCommandName(t *testing.T) {
	const tail = " S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 12345 600 18446744073709551615"
	for _, name := range []string{"papio", "a (b) c", "x) S 1 2 (y", ""} {
		got, ok := parseStatStart([]byte("4242 (" + name + ")" + tail + "\n"))
		if !ok || got != 987654 {
			t.Errorf("command %q: start = %d, %v; want 987654", name, got, ok)
		}
	}
	for _, bad := range []string{"", "4242 papio S 1", "4242 (papio) S 1 2 3"} {
		if got, ok := parseStatStart([]byte(bad)); ok {
			t.Errorf("%q parsed as %d; want a refusal", bad, got)
		}
	}
}
