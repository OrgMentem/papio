// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type readerFunc func(context.Context, string) (Record, error)

func (f readerFunc) Load(ctx context.Context, ref string) (Record, error) { return f(ctx, ref) }

func TestResolveAuthoritativeReferencesNeverFallBack(t *testing.T) {
	legacy := Record{Kind: KindCORE, APIKey: "synthetic-legacy"}
	for _, tc := range []struct {
		name   string
		record Record
		err    error
		state  string
	}{
		{"missing", Record{}, ErrNotFound, "missing"},
		{"unavailable", Record{}, errors.New("private vendor error"), "unavailable"},
		{"busy", Record{}, ErrBusy, "unavailable"},
		{"deadline", Record{}, context.DeadlineExceeded, "unavailable"},
		{"invalid", Record{}, ErrInvalidRecord, "invalid"},
		{"wrong kind", Record{Kind: KindTypeSafe, APIKey: "synthetic-other"}, nil, "invalid"},
		{"malformed from reader", Record{Kind: KindCORE, APIKey: "unsafe\nheader"}, nil, "invalid"},
		{"ready", Record{Kind: KindCORE, APIKey: "synthetic-selected"}, nil, "ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Resolve(context.Background(), []Binding{{Target: "sources.core", Reference: testReference, Kinds: []Kind{KindCORE}, Legacy: legacy}}, readerFunc(func(context.Context, string) (Record, error) { return tc.record, tc.err }), nil)
			statuses := r.Statuses()
			if len(statuses) != 1 || statuses[0] != (Status{"sources.core", testReference, "keyring", tc.state}) {
				t.Fatalf("incorrect diagnostic: %#v", statuses)
			}
			record, ok := r.Record("sources.core")
			if tc.state == "ready" {
				if !ok || record != tc.record {
					t.Fatal("selected credential was not used")
				}
			} else if ok || record != (Record{}) {
				t.Fatal("failed reference selected a legacy or invalid credential")
			}
		})
	}
}

func TestResolveSharedReferenceSnapshotAndConsumerKinds(t *testing.T) {
	calls := 0
	record := Record{Kind: KindILLiad, APIKey: "synthetic-shared"}
	r := Resolve(context.Background(), []Binding{
		{Target: "default", Reference: testReference, Kinds: []Kind{KindILLiad}},
		{Target: "institution-a", Reference: testReference, Kinds: []Kind{KindILLiad}},
		{Target: "incompatible", Reference: testReference, Kinds: []Kind{KindCORE}},
		{Target: "keyless", Kinds: []Kind{KindOpenAlex}},
	}, readerFunc(func(context.Context, string) (Record, error) {
		calls++
		if calls > 1 {
			return Record{}, ErrUnavailable
		}
		return record, nil
	}), nil)
	if calls != 1 {
		t.Fatal("shared reference loaded repeatedly")
	}
	for _, target := range []string{"default", "institution-a"} {
		got, ok := r.Record(target)
		if !ok || got != record {
			t.Fatal("shared binding did not resolve")
		}
		got.APIKey = "mutated-copy"
		if again, _ := r.Record(target); again != record {
			t.Fatal("returned record mutates resolution")
		}
	}
	if _, ok := r.Record("incompatible"); ok {
		t.Fatal("shared reference bypassed kind validation")
	}
	states := r.Statuses()
	if states[2].State != "invalid" || states[3].State != "not_configured" || states[3].Source != "none" {
		t.Fatal("unrelated consumers were not resolved independently")
	}
	states[0].State = "mutated-copy"
	if r.Statuses()[0].State != "ready" {
		t.Fatal("returned status slice mutates resolution")
	}
	if got, ok := r.Record("unbound-profile"); ok || got != (Record{}) {
		t.Fatal("unbound profile inherited a stored credential")
	}
}

func TestResolveEnvironmentIsExplicitAndDoesNotScrub(t *testing.T) {
	lookups := map[string]int{}
	values := map[string]string{"PAPIO_TEST_CORE": "literal-密钥", "PAPIO_TEST_EMPTY": ""}
	lookup := func(name string) (string, bool) {
		lookups[name]++
		value, ok := values[name]
		return value, ok
	}
	storeCalls := 0
	r := Resolve(context.Background(), []Binding{
		{Target: "one", Reference: "env:PAPIO_TEST_CORE", Kinds: []Kind{KindCORE}},
		{Target: "two", Reference: "env:PAPIO_TEST_CORE", Kinds: []Kind{KindCORE}},
		{Target: "missing", Reference: "env:PAPIO_TEST_MISSING", Kinds: []Kind{KindCORE}, Legacy: Record{Kind: KindCORE, APIKey: "legacy"}},
		{Target: "empty", Reference: "env:PAPIO_TEST_EMPTY", Kinds: []Kind{KindCORE}},
		{Target: "unconfigured", Kinds: []Kind{KindCORE}},
	}, readerFunc(func(context.Context, string) (Record, error) { storeCalls++; return Record{}, ErrUnavailable }), lookup)
	if storeCalls != 0 || lookups["PAPIO_TEST_CORE"] != 1 || len(lookups) != 3 || values["PAPIO_TEST_CORE"] != "literal-密钥" {
		t.Fatal("environment lookup was implicit, repeated, scrubbed or sent to store")
	}
	for _, target := range []string{"one", "two"} {
		if got, ok := r.Record(target); !ok || got.APIKey != values["PAPIO_TEST_CORE"] {
			t.Fatal("environment value was transformed")
		}
	}
	states := r.Statuses()
	if states[0].Source != "environment" || states[2].State != "missing" || states[3].State != "invalid" || states[4].State != "not_configured" {
		t.Fatal("environment status semantics changed")
	}
}

func TestResolveLegacyValidationAndNoExternalLookup(t *testing.T) {
	pair := Record{Kind: KindOpenAIREClient, ClientID: "id", ClientSecret: "secret"}
	r := Resolve(context.Background(), []Binding{
		{Target: "pair", Kinds: []Kind{KindOpenAIREClient, KindOpenAIREToken}, Legacy: pair},
		{Target: "wrong", Kinds: []Kind{KindCORE}, Legacy: pair},
		{Target: "partial", Kinds: []Kind{KindOpenAIREClient}, Legacy: Record{Kind: KindOpenAIREClient, ClientID: "id"}},
		{Target: "unknown", Kinds: []Kind{KindCORE}, Legacy: Record{Kind: "unknown", APIKey: "secret"}},
		{Target: "unrelated", Kinds: []Kind{KindOpenAlex}},
	}, readerFunc(func(context.Context, string) (Record, error) {
		t.Fatal("legacy resolution read OS store")
		return Record{}, nil
	}), func(string) (string, bool) { t.Fatal("legacy resolution searched environment"); return "", false })
	if got, ok := r.Record("pair"); !ok || got != pair {
		t.Fatal("legacy pair was not preserved")
	}
	for _, target := range []string{"wrong", "partial", "unknown", "unrelated"} {
		if _, ok := r.Record(target); ok {
			t.Fatal("invalid or absent legacy credential accepted")
		}
	}
	for i, status := range r.Statuses() {
		if i == 0 && (status.State != "ready" || status.Source != "legacy") || i > 0 && i < 4 && (status.State != "invalid" || status.Source != "legacy") || i == 4 && status.State != "not_configured" {
			t.Fatalf("incorrect status: %#v", status)
		}
	}
}

func TestResolutionDiagnosticsRedactSecretsAndErrors(t *testing.T) {
	secret := "synthetic-private-value"
	r := Resolve(context.Background(), []Binding{
		{Target: "ready", Reference: testReference, Kinds: []Kind{KindCORE}},
		{Target: "bad-ref", Reference: secret, Kinds: []Kind{KindCORE}},
	}, readerFunc(func(context.Context, string) (Record, error) { return Record{Kind: KindCORE, APIKey: secret}, nil }), nil)
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	printed := fmt.Sprintf("%v %+v %#v %v %+v %#v", r, r, r, *r, *r, *r)
	if strings.Contains(printed+string(encoded), secret) {
		t.Fatal("diagnostics exposed secret or malformed reference")
	}
	want := `{"statuses":[{"target":"ready","reference":"` + testReference + `","source":"keyring","state":"ready"},{"target":"bad-ref","reference":"","source":"none","state":"invalid"}]}`
	if string(encoded) != want {
		t.Fatalf("diagnostic JSON contract changed: %s", encoded)
	}
}

func TestResolveDuplicateTargetsFailClosed(t *testing.T) {
	r := Resolve(context.Background(), []Binding{
		{Target: "ambiguous", Kinds: []Kind{KindCORE}, Legacy: Record{Kind: KindCORE, APIKey: "first"}},
		{Target: "ambiguous", Kinds: []Kind{KindCORE}, Legacy: Record{Kind: KindCORE, APIKey: "second"}},
		{Target: "", Kinds: []Kind{KindCORE}, Legacy: Record{Kind: KindCORE, APIKey: "third"}},
	}, nil, nil)
	if len(r.records) != 0 {
		t.Fatal("ambiguous consumer selected a credential")
	}
	for _, status := range r.Statuses() {
		if status.State != "invalid" {
			t.Fatal("ambiguous consumer was not diagnosed")
		}
	}
	var absent *Resolution
	if _, ok := absent.Record("anything"); ok || len(absent.Statuses()) != 0 {
		t.Fatal("nil resolution is not empty")
	}
}
