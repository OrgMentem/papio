// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"testing"

	"papio/internal/config"
	"papio/internal/watch"
)

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
	for _, name := range []string{"label", "collection", "kind", "mode", "cadence", "limit-per-run", "year-from", "year-to", "oa-only"} {
		if add.Flags().Lookup(name) == nil {
			t.Fatalf("watch add missing --%s", name)
		}
	}
}

func TestWatchCommandBackfillArgsAndDigestMetadata(t *testing.T) {
	command := newWatchCommand(&options{})
	add, _, err := command.Find([]string{"add"})
	if err != nil {
		t.Fatal(err)
	}
	if err := add.Flags().Set("kind", "backfill"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args    []string
		wantErr bool
	}{
		{args: nil},
		{args: []string{"query"}, wantErr: true},
	} {
		if err := add.Args(add, test.args); (err != nil) != test.wantErr {
			t.Fatalf("backfill Args(%q) error = %v, want error %v", test.args, err, test.wantErr)
		}
	}
	digest, _, err := command.Find([]string{"digest"})
	if err != nil {
		t.Fatal(err)
	}
	if digest.Annotations["mcp:read-only"] != "true" || digest.Flags().Lookup("limit") == nil {
		t.Fatalf("watch digest metadata = %#v, flags = %#v", digest.Annotations, digest.Flags())
	}
}

func TestWatchRunDisplaysReportedAlertWorks(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, _ any, result any) error {
		if method != "watch.run" {
			t.Fatalf("method = %q, want watch.run", method)
		}
		*result.(*watch.RunResult) = watch.RunResult{WatchID: 7, Reported: 2}
		return nil
	})
	root.SetArgs([]string{"watch", "run", "7"})
	if err := root.Execute(); err != nil {
		t.Fatalf("watch run: %v (%s)", err, stderr.String())
	}
	if got := stdout.String(); got != "Watch 7 reported 2 new work(s) — papio watch digest 7\n" {
		t.Fatalf("stdout = %q", got)
	}
}
