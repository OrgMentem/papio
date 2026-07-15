// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "testing"

func TestParseWatchCadence(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int
	}{
		{"daily", 24}, {"weekly", 168}, {"6h", 6}, {" 48H ", 48},
	} {
		got, err := parseWatchCadence(test.input)
		if err != nil || got != test.want {
			t.Fatalf("parseWatchCadence(%q) = %d, %v; want %d, nil", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"", "hourly", "0h", "-1h", "1d"} {
		if _, err := parseWatchCadence(input); err == nil {
			t.Fatalf("parseWatchCadence(%q) succeeded", input)
		}
	}
}

func TestWatchCommandExposesRequestedFlags(t *testing.T) {
	command := newWatchCommand(&options{})
	add, _, err := command.Find([]string{"add"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"label", "collection", "cadence", "limit-per-run", "year-from", "year-to", "oa-only"} {
		if add.Flags().Lookup(name) == nil {
			t.Fatalf("watch add missing --%s", name)
		}
	}
}
