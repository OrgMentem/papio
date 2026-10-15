// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"os"
	"papio/internal/config"
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

func TestCampaignUnknownIDNamesTheIDAndCreatesNoLock(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{DataDir: dir}, nilRPC)
	root.SetArgs([]string{"campaign", "report", "campaign_missing"})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), `campaign "campaign_missing" not found in `) {
		t.Fatalf("err = %v, want a not-found error naming the id", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "campaigns")); !os.IsNotExist(statErr) {
		t.Fatalf("campaigns dir was created for a missing campaign: %v", statErr)
	}
	root = NewInProcessRoot(&out, &errOut, config.Config{DataDir: dir}, nilRPC)
	root.SetArgs([]string{"campaign", "report", "bad id"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), `"bad id"`) {
		t.Fatalf("err = %v, want the invalid id quoted", err)
	}
}

func TestCampaignListSummarisesStoredManifests(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "campaigns"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"id":"campaign_one","created_at":"2026-01-01T00:00:00Z","source_digest":"d","format":"jsonl","library_fingerprint":"","zotio_executable":"","paused":true,` +
		`"records":[{"ordinal":1,"source_key":"k","citation":{"SourceKey":"k","DOI":"","PMID":"","ArXiv":"","Title":"T","Authors":null,"Year":2020},"work_index":-1,"error":"no identifier"}],"works":[],"chunks":[]}`
	if err := os.WriteFile(filepath.Join(dir, "campaigns", "campaign_one.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{DataDir: dir}, nilRPC)
	root.SetArgs([]string{"--json", "campaign", "list"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("campaign list: %v (%s)", err, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, `"id": "campaign_one"`) && !strings.Contains(got, `"id":"campaign_one"`) || !strings.Contains(got, `"paused"`) {
		t.Fatalf("list output = %s", got)
	}
}
