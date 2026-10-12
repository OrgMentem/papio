// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCampaignLoadLegacyCitationISBN(t *testing.T) {
	const id = "campaign_legacy"
	manifest := func(citationExtra string) string {
		return `{"version":1,"id":"` + id + `","created_at":"2026-01-01T00:00:00Z","source_digest":"d","format":"jsonl","library_fingerprint":"","zotio_executable":"","paused":false,` +
			`"records":[{"ordinal":1,"source_key":"k","citation":{"SourceKey":"k","DOI":"","PMID":"","ArXiv":"","Title":"T","Authors":null,"Year":2020` + citationExtra + `},"work_index":-1,"error":"no identifier"}],"works":[],"chunks":[]}`
	}
	load := func(t *testing.T, body string) error {
		path := filepath.Join(t.TempDir(), id+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadCampaign(path, id)
		return err
	}
	for name, extra := range map[string]string{
		"empty isbn": `,"ISBN":""`,
		"isbn value": `,"ISBN":"9780306406157"`,
		"no isbn":    ``,
	} {
		t.Run(name, func(t *testing.T) {
			if err := load(t, manifest(extra)); err != nil {
				t.Fatalf("load: %v", err)
			}
		})
	}
	t.Run("other unknown citation field", func(t *testing.T) {
		err := load(t, manifest(`,"ISBN":"","Bogus":1`))
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("err = %v, want unknown field", err)
		}
	})
	t.Run("unknown record field", func(t *testing.T) {
		body := strings.Replace(manifest(""), `"work_index":-1`, `"work_index":-1,"bogus":1`, 1)
		if err := load(t, body); err == nil {
			t.Fatal("expected error")
		}
	})
}
