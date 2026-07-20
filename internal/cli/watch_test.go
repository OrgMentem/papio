// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/watch"
	"papio/internal/zotio"
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

func TestAcquireFromDigestRejectsIncompatibleInputs(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "positional identifier", args: []string{"acquire", "--from-digest", "7", "10.1000/example"}, want: "positional"},
		{name: "batch", args: []string{"acquire", "--from-digest", "7", "--batch", "works.jsonl"}, want: "--batch"},
		{name: "zotio", args: []string{"acquire", "--from-digest", "7", "--from-zotio"}, want: "--from-zotio"},
		{name: "empty keys", args: []string{"acquire", "--from-digest", "7", "--keys", ""}, want: "at least one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, _ any, _ any) error {
				t.Fatalf("unexpected RPC %q", method)
				return nil
			})
			root.SetArgs(test.args)
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("acquire %v error = %v, want message containing %q", test.args[1:], err, test.want)
			}
		})
	}
}

func TestAcquireFromDigestPreservesOpaqueRepeatedKeys(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		if method != "watch.digest_acquire" {
			t.Fatalf("method = %q, want watch.digest_acquire", method)
		}
		var request struct {
			ID   int64    `json:"id"`
			Keys []string `json:"keys"`
		}
		encoded, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		if err := json.Unmarshal(encoded, &request); err != nil {
			t.Fatalf("unmarshal params: %v", err)
		}
		if request.ID != 7 {
			t.Fatalf("digest ID = %d, want 7", request.ID)
		}
		if len(request.Keys) != 2 || request.Keys[0] != "a study, revisited" || request.Keys[1] != "another study" {
			t.Fatalf("digest keys = %#v, want opaque, trimmed repeated keys", request.Keys)
		}
		*result.(*api.WatchDigestAcquireResult) = api.WatchDigestAcquireResult{Queued: 2}
		return nil
	})
	root.SetArgs([]string{"acquire", "--from-digest", "7", "--keys", " a study, revisited ", "--keys", "\tanother study\n"})
	if err := root.Execute(); err != nil {
		t.Fatalf("acquire --from-digest --keys: %v (%s)", err, stderr.String())
	}
}

func TestAcquireFromZotioParsesLimit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := NewInProcessRoot(&stdout, &stderr, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		if method != "zotio.queue" {
			t.Fatalf("method = %q, want zotio.queue", method)
		}
		options, ok := params.(zotio.QueueOptions)
		if !ok {
			t.Fatalf("params = %T, want zotio.QueueOptions", params)
		}
		if options.Limit != 7 {
			t.Fatalf("queue limit = %d, want 7", options.Limit)
		}
		*result.(*zotio.QueueResult) = zotio.QueueResult{}
		return nil
	})
	root.SetArgs([]string{"acquire", "--from-zotio", "--limit", "7"})
	if err := root.Execute(); err != nil {
		t.Fatalf("acquire --from-zotio --limit: %v (%s)", err, stderr.String())
	}
}
