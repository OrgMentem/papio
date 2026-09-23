// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativeViewerFixture(t *testing.T, base string) map[string]any {
	t.Helper()
	var frame map[string]any
	nativeViewerReadJSON(t, filepath.Join("valid", "browser-native-viewer-save-"+base+".json"), &frame)
	return frame
}

func nativeViewerReadJSON(t *testing.T, name string, target any) {
	t.Helper()
	raw, err := os.ReadFile(repoPath(t, "testdata", "protocol", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}

func TestNativeViewerSaveContractCases(t *testing.T) {
	var cases []struct {
		Name, Base, Prefix string
		Path               []string
		Value              any
		Repeat             int
		Remove, Valid      bool
		SchemaValid        *bool `json:"schema_valid"`
	}
	nativeViewerReadJSON(t, "native-viewer-save-cases.json", &cases)
	schema := compileSchema(t, "browser-v1.schema.json")
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			frame := nativeViewerFixture(t, tc.Base)
			target := frame
			for _, key := range tc.Path[:len(tc.Path)-1] {
				target = target[key].(map[string]any)
			}
			key := tc.Path[len(tc.Path)-1]
			if tc.Remove {
				delete(target, key)
			} else if tc.Repeat > 0 {
				target[key] = tc.Prefix + strings.Repeat(tc.Value.(string), tc.Repeat)
			} else {
				target[key] = tc.Value
			}
			raw, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := DecodeBrowserMessage(raw)
			if (err == nil) != tc.Valid {
				t.Fatalf("Go valid=%t: %v", tc.Valid, err)
			}
			if tc.Valid {
				if err := msg.Payload.(interface{ Validate() error }).Validate(); err != nil {
					t.Fatal(err)
				}
				if p, ok := msg.Payload.(*NativeViewerSaveRequestV1Payload); ok && p.SourceURL != frame["payload"].(map[string]any)["source_url"] {
					t.Fatal("source URL was normalized")
				}
			}
			wantSchema := tc.Valid
			if tc.SchemaValid != nil {
				// Standard JSON Schema counts characters, not UTF-8 bytes.
				if tc.Name != "URL UTF8 over byte cap" {
					t.Fatal("unexpected schema-only exception")
				}
				wantSchema = *tc.SchemaValid
			}
			if err := schema.Validate(frame); (err == nil) != wantSchema {
				t.Fatalf("schema valid=%t: %v", wantSchema, err)
			}
		})
	}
}

func TestNativeViewerSaveRawDuplicates(t *testing.T) {
	var cases []struct{ Name, Raw string }
	nativeViewerReadJSON(t, "native-viewer-save-raw-cases.json", &cases)
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if _, err := DecodeBrowserMessage([]byte(tc.Raw)); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Fatalf("expected duplicate rejection, got %v", err)
			}
		})
	}
}

func TestNativeViewerSaveExportedValidation(t *testing.T) {
	request := NativeViewerSaveRequestV1Payload{
		RequestID: "viewer-request-1", ActionID: 1, ActionRevision: 1,
		BrowserEpoch: "epoch", DocumentID: "document", SourceURL: "https://example.invalid/p#page=2", Step: "prepare",
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*NativeViewerSaveRequestV1Payload){
		func(p *NativeViewerSaveRequestV1Payload) { p.ActionID = 0 },
		func(p *NativeViewerSaveRequestV1Payload) { p.ActionRevision = MaxBrowserInteger + 1 },
		func(p *NativeViewerSaveRequestV1Payload) { p.OperationID = "operation-1" },
		func(p *NativeViewerSaveRequestV1Payload) { p.Step = "advance" },
		func(p *NativeViewerSaveRequestV1Payload) { p.SourceURL = "https://synthetic:secret@example.invalid/p" }, // gitleaks:allow -- synthetic userinfo rejection fixture
		func(p *NativeViewerSaveRequestV1Payload) { p.SourceURL = "https://example.invalid/%invalid-synthetic" },
	} {
		p := request
		mutate(&p)
		err := p.Validate()
		if err == nil {
			t.Fatal("invalid typed request accepted")
		}
		if strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), "https://") {
			t.Fatal("validation error disclosed URL material")
		}
	}
	for _, outcome := range []string{"prepared", "pending"} {
		p := NativeViewerSaveResultV1Payload{RequestID: request.RequestID, Outcome: outcome}
		if p.Validate() == nil {
			t.Fatal("operation missing from nonterminal result")
		}
		p.OperationID = "operation-1"
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativeViewerSaveSelectionAuthority(t *testing.T) {
	frame := nativeViewerFixture(t, "prepare")
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := DecodeBrowserMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	base := *msg.Payload.(*NativeViewerSaveRequestV1Payload)
	if base.Selection != "" {
		t.Fatal("omitted selection must remain unset and grant no explicit authority")
	}
	for _, step := range []string{"prepare", "advance", "cancel"} {
		for _, selection := range []string{"", "automatic", "explicit", "Explicit", "unknown"} {
			p := base
			p.Step, p.Selection = step, selection
			if step != "prepare" {
				p.OperationID = "operation-1"
			}
			valid := selection == "" || (step == "prepare" && (selection == "automatic" || selection == "explicit"))
			if err := p.Validate(); (err == nil) != valid {
				t.Errorf("step=%s selection=%q valid=%t: %v", step, selection, valid, err)
			}
		}
	}
}
