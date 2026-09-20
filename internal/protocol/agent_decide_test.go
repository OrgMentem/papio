// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// Both executable parsers and the published schema consume these mutations.
// Schema exceptions are uniqueness by ID across otherwise different controls
// (uniqueItems compares whole objects) and the DOI's UTF-8 byte bound
// (maxLength counts Unicode code points).
func TestAgentDecideV1Conformance(t *testing.T) {
	var cases []struct {
		Name        string   `json:"name"`
		Base        string   `json:"base"`
		Path        []string `json:"path"`
		Value       any      `json:"value"`
		Remove      bool     `json:"remove"`
		Valid       bool     `json:"valid"`
		SchemaValid *bool    `json:"schema_valid"`
	}
	raw, err := os.ReadFile(repoPath(t, "testdata", "protocol", "agent-decide-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	schema := compileSchema(t, "browser-v1.schema.json")
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			name := "browser-agent-decide-request-v1.json"
			if tc.Base == "result" {
				name = "browser-agent-decide-result-decision-v1.json"
			} else if tc.Base != "request" {
				name = "browser-agent-decide-result-" + tc.Base + "-v1.json"
			}
			doc := readFixture(t, "valid", name)
			at := doc
			for _, key := range tc.Path[:len(tc.Path)-1] {
				switch v := at.(type) {
				case map[string]any:
					at = v[key]
				case []any:
					index, err := strconv.Atoi(key)
					if err != nil {
						t.Fatal(err)
					}
					at = v[index]
				default:
					t.Fatalf("bad fixture path %v", tc.Path)
				}
			}
			fields := at.(map[string]any)
			key := tc.Path[len(tc.Path)-1]
			if tc.Remove {
				delete(fields, key)
			} else {
				fields[key] = tc.Value
			}
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := DecodeBrowserMessage(data)
			if (err == nil) != tc.Valid {
				t.Errorf("Go accepted=%v, want %v: %v", err == nil, tc.Valid, err)
			}
			schemaValid := tc.Valid
			if tc.SchemaValid != nil {
				schemaValid = *tc.SchemaValid
			}
			if err := schema.Validate(doc); (err == nil) != schemaValid {
				t.Errorf("schema accepted=%v, want %v: %v", err == nil, schemaValid, err)
			}
			if tc.Valid && msg != nil {
				assertAgentDecideRoundTrip(t, msg)
			}
		})
	}
}

func assertAgentDecideRoundTrip(t *testing.T, msg *BrowserMessage) {
	t.Helper()
	var err error
	switch p := msg.Payload.(type) {
	case *AgentDecideRequestV1Payload:
		err = p.Validate()
	case *AgentDecideResultV1Payload:
		err = p.Validate()
	default:
		t.Fatalf("unexpected payload type %T", msg.Payload)
	}
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeBrowserMessage(raw)
	if err != nil || !reflect.DeepEqual(msg, got) {
		t.Fatalf("round trip changed message: %v", err)
	}
}

func TestAgentDecideV1TypedPayloadRoundTrip(t *testing.T) {
	names, err := filepath.Glob(filepath.Join(corpusDir(t, "valid"), "browser-agent-decide-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := DecodeBrowserMessage(raw)
		if err != nil {
			t.Fatal(err)
		}
		assertAgentDecideRoundTrip(t, msg)
	}
}
