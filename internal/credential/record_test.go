// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package credential

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func validRecords() []Record {
	return []Record{
		{Kind: KindTypeSafe, APIKey: "synthetic-typesafe"},
		{Kind: KindOpenAlex, APIKey: "synthetic-openalex"},
		{Kind: KindSemanticScholar, APIKey: "synthetic-semanticscholar"},
		{Kind: KindCORE, APIKey: "synthetic-core"},
		{Kind: KindCrossrefTDM, APIKey: "synthetic-crossref"},
		{Kind: KindOpenAIREToken, APIKey: "synthetic-openaire-token"},
		{Kind: KindILLiad, APIKey: "synthetic-illiad"},
		{Kind: KindOpenAIREClient, ClientID: "synthetic-id", ClientSecret: "synthetic-client-secret"},
		{Kind: KindWebhook, URL: "https://example.com/secret-path?token=secret-query", Bearer: "synthetic-bearer"},
		{Kind: KindWebhook, URL: "http://localhost:8042/hook"},
	}
}

func TestRecordRoundTripAndRedaction(t *testing.T) {
	for _, record := range validRecords() {
		t.Run(string(record.Kind), func(t *testing.T) {
			encoded, err := Encode(record)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(encoded)
			if err != nil || decoded != record {
				t.Fatalf("record did not round trip exactly: %v", err)
			}
			printed := fmt.Sprintf("%v %+v %#v %s %q", record, record, record, record, record)
			ordinary, err := json.Marshal(struct{ Credential Record }{record})
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{record.APIKey, record.ClientID, record.ClientSecret, record.URL, record.Bearer} {
				if secret != "" && (strings.Contains(printed, secret) || strings.Contains(string(ordinary), secret)) {
					t.Fatal("ordinary serialization exposed a credential")
				}
			}
		})
	}
	// Even a malformed kind must not turn a formatting operation into a leak.
	malformed := Record{Kind: "synthetic-private-kind", APIKey: "synthetic-private-key"}
	encoded, _ := json.Marshal(malformed)
	if strings.Contains(string(encoded)+fmt.Sprintf("%#v", malformed), "synthetic-private") {
		t.Fatal("invalid record was not redacted")
	}
}

func TestRecordKindValidation(t *testing.T) {
	for _, valid := range validRecords() {
		bad := valid
		if valid.Kind == KindOpenAIREClient || valid.Kind == KindWebhook {
			bad.APIKey = "synthetic-unexpected"
		} else {
			bad.ClientSecret = "synthetic-unexpected"
		}
		if !sameSentinel(bad.Validate(), ErrInvalidRecord) {
			t.Fatal("fields from another kind accepted")
		}
	}
	for _, invalid := range []Record{
		{}, {Kind: "unknown", APIKey: "synthetic"},
		{Kind: KindOpenAIREClient, ClientID: "synthetic-id"},
		{Kind: KindOpenAIREClient, ClientSecret: "synthetic-secret"},
		{Kind: KindWebhook, Bearer: "synthetic-bearer"},
		{Kind: KindWebhook, URL: "https://user:secret@example.com/hook"},
		{Kind: KindWebhook, URL: "https:///hook"},
		{Kind: KindWebhook, URL: "file:///secret"},
		{Kind: KindWebhook, URL: "https://example.com/space here"},
		{Kind: KindWebhook, URL: "https://example.com", Bearer: "unsafe\r\nheader"},
	} {
		if !sameSentinel(invalid.Validate(), ErrInvalidRecord) {
			t.Fatal("invalid credential accepted")
		}
	}
	for _, key := range []string{"", " ", " leading", "trailing ", "embedded space", "line\nbreak", "\x00", "\t", "\r", "\x1f", "\x7f", "é", "\xff", strings.Repeat("a", 1025)} {
		if !sameSentinel((Record{Kind: KindTypeSafe, APIKey: key}).Validate(), ErrInvalidRecord) {
			t.Fatal("TypeSafe compatibility validation changed")
		}
	}
	for _, key := range []string{"synthetic", "a!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", strings.Repeat("a", 1024)} {
		if err := (Record{Kind: KindTypeSafe, APIKey: key}).Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []Kind{KindOpenAlex, KindSemanticScholar, KindCORE, KindCrossrefTDM, KindOpenAIREToken, KindILLiad} {
		for _, key := range []string{"clé-密钥", " literal secret "} {
			r := Record{Kind: kind, APIKey: key}
			encoded, err := Encode(r)
			if err != nil {
				t.Fatal("other integrations inherited TypeSafe's ASCII restrictions")
			}
			decoded, err := Decode(encoded)
			if err != nil || decoded != r {
				t.Fatal("credential bytes were normalized")
			}
		}
		for _, key := range []string{"", " ", "bad\nkey", "bad\x7fkey", "bad\u0085key", "\xff"} {
			if !sameSentinel((Record{Kind: kind, APIKey: key}).Validate(), ErrInvalidRecord) {
				t.Fatal("invalid token accepted")
			}
		}
	}
}

func TestDecodeStrictWireShape(t *testing.T) {
	for _, encoded := range []string{
		``, `null`, `[]`, `{}`, `{"version":1}`, `{"kind":"core","api_key":"synthetic"}`,
		`{"version":2,"kind":"core","api_key":"synthetic"}`,
		`{"version":1.0,"kind":"core","api_key":"synthetic"}`,
		`{"version":"1","kind":"core","api_key":"synthetic"}`,
		`{"version":1,"kind":"core","api_key":"synthetic","unknown":"private"}`,
		`{"version":1,"kind":"core","APIKey":"synthetic"}`,
		`{"version":1,"Kind":"core","api_key":"synthetic"}`,
		`{"version":1,"kind":"core","api_key":"synthetic","api_key":"other"}`,
		`{"version":1,"kind":"core","api_key":"synthetic","client_id":""}`,
		`{"version":1,"kind":"core","api_key":null}`,
		`{"version":1,"kind":"core","api_key":12}`,
		`{"version":1,"kind":"core","api_key":true}`,
		`{"version":1,"kind":"core","api_key":[]}`,
		`{"version":1,"kind":"core","api_key":"\ud800"}`,
		`{"version":1,"kind":"core","api_key":"\udfff"}`,
		`{"version":1,"kind":"core","api_key":"\ud800\u0061"}`,
		`{"version":1,"kind":"core","api_key":"\ud800\ud800"}`,
		`{"version":1,"kind":"webhook","url":"https://example.com","bearer":null}`,
		`{"version":1,"kind":"openaire_client","client_id":"id"}`,
		`{"version":1,"kind":"core","api_key":"synthetic"} {}`,
		`{"version":1,"kind":"core","api_key":"synthetic"} false`,
		`{"version":1,"kind":"core","api_key":"synthetic"} trailing`,
		"{\"version\":1,\"kind\":\"core\",\"api_key\":\"\xff\"}",
	} {
		if record, err := Decode(encoded); !sameSentinel(err, ErrInvalidRecord) || record != (Record{}) {
			t.Fatal("invalid wire record accepted or included in error")
		}
	}
	for _, tc := range []struct{ encoded, key string }{
		{`{"version":1,"kind":"core","api_key":"\ud83d\udd11"}`, "🔑"},
		{`{"version":1,"kind":"core","api_key":"\\ud800"}`, `\ud800`},
		{`{"version":1,"kind":"core","api_key":"\ufffd"}`, "�"},
		{`{"version":1,"kind":"core","api_key":"escaped\/slash"}`, "escaped/slash"},
	} {
		record, err := Decode(tc.encoded)
		if err != nil || record.APIKey != tc.key {
			t.Fatal("valid JSON Unicode/escape sequence changed")
		}
	}
}

func TestEncodedSizeBoundUsesWholeUTF8Record(t *testing.T) {
	base := Record{Kind: KindOpenAIREClient, ClientID: "id", ClientSecret: "a"}
	wire, err := Encode(base)
	if err != nil {
		t.Fatal(err)
	}
	base.ClientSecret = strings.Repeat("a", MaxEncodedBytes-len(wire)+1)
	exact, err := Encode(base)
	if err != nil || len(exact) != MaxEncodedBytes {
		t.Fatalf("exact size should fit: %v", err)
	}
	if decoded, err := Decode(exact); err != nil || decoded != base {
		t.Fatal("exact limit did not round trip")
	}
	base.ClientSecret += "a"
	if value, err := Encode(base); value != "" || !sameSentinel(err, ErrInvalidRecord) {
		t.Fatal("oversized record encoded")
	}
	if record, err := Decode(exact + " "); record != (Record{}) || !sameSentinel(err, ErrInvalidRecord) {
		t.Fatal("oversized wire decoded")
	}
	for _, secret := range []string{strings.Repeat("界", 700), strings.Repeat("\"", 1024), strings.Repeat("<", 500)} {
		if encoded, err := Encode(Record{Kind: KindCORE, APIKey: secret}); encoded != "" || !sameSentinel(err, ErrInvalidRecord) {
			t.Fatal("UTF8 or escaped JSON expansion bypassed bound")
		}
	}
}

func TestDecodeEnvironment(t *testing.T) {
	for _, record := range validRecords() {
		var value string
		if apiKeyKind(record.Kind) {
			value = record.APIKey
		} else {
			value, _ = Encode(record)
		}
		decoded, err := DecodeEnvironment(value, []Kind{record.Kind})
		if err != nil || decoded != record {
			t.Fatal("single-kind environment record did not round trip")
		}
	}
	allowed := []Kind{KindOpenAIREClient, KindOpenAIREToken}
	for _, record := range []Record{{Kind: KindOpenAIREClient, ClientID: "id", ClientSecret: "secret"}, {Kind: KindOpenAIREToken, APIKey: "token"}} {
		value, _ := Encode(record)
		if got, err := DecodeEnvironment(value, allowed); got != record || err != nil {
			t.Fatal("explicit OpenAIRE kind failed")
		}
	}
	for _, value := range []string{"raw-ambiguous-token", `{"kind":"openaire_token","api_key":"token"}`, `{"version":1,"kind":"core","api_key":"token"}`} {
		if got, err := DecodeEnvironment(value, allowed); got != (Record{}) || !sameSentinel(err, ErrInvalidRecord) {
			t.Fatal("ambiguous or wrong-kind environment value accepted")
		}
	}
	if _, err := DecodeEnvironment(" leading", []Kind{KindTypeSafe}); !sameSentinel(err, ErrInvalidRecord) {
		t.Fatal("raw environment value was trimmed")
	}
	if _, err := DecodeEnvironment("synthetic", nil); !sameSentinel(err, ErrInvalidRecord) {
		t.Fatal("missing kind accepted")
	}
}

func TestReferenceValidationAndGeneration(t *testing.T) {
	refs := make(map[string]bool)
	for range 100 {
		ref, err := NewReference()
		if err != nil || ValidateReference(ref) != nil || !keyringReference(ref) || refs[ref] {
			t.Fatal("generated reference invalid or reused")
		}
		refs[ref] = true
	}
	for _, ref := range []string{"env:API_KEY", "env:_local1", "env:" + strings.Repeat("A", 128), testReference} {
		if err := ValidateReference(ref); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(ref, "env:") {
			name, ok := EnvironmentName(ref)
			if !ok || name != strings.TrimPrefix(ref, "env:") {
				t.Fatal("environment reference changed")
			}
		}
	}
	for _, ref := range []string{"", "ENV:KEY", "env:", "env:1KEY", "env:MY-KEY", "env:é", "env:" + strings.Repeat("A", 129), "env:KEY\n", "keyring:" + strings.Repeat("A", 32), "keyring:" + strings.Repeat("g", 32), "keyring:" + strings.Repeat("a", 31), "keyring:" + strings.Repeat("a", 33), "file:/secret", "../path"} {
		if !sameSentinel(ValidateReference(ref), ErrInvalidReference) {
			t.Fatal("invalid reference accepted")
		}
		if name, ok := EnvironmentName(ref); name != "" || ok {
			t.Fatal("invalid environment name returned")
		}
	}
}
