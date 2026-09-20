// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeDownloadContractCases(t *testing.T) {
	var cases []struct {
		Name, Base    string
		Path          []string
		Value         any
		Remove, Valid bool
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "native-download-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(corpusDir(t, "valid"), "browser-native-download-"+tc.Base+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var frame map[string]any
			if err = json.Unmarshal(raw, &frame); err != nil {
				t.Fatal(err)
			}
			obj := frame
			for _, key := range tc.Path[:len(tc.Path)-1] {
				obj = obj[key].(map[string]any)
			}
			key := tc.Path[len(tc.Path)-1]
			if tc.Remove {
				delete(obj, key)
			} else {
				obj[key] = tc.Value
			}
			raw, err = json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			_, err = DecodeBrowserMessage(raw)
			if (err == nil) != tc.Valid {
				t.Fatalf("valid=%t err=%v", tc.Valid, err)
			}
		})
	}
}
