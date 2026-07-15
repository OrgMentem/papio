// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/config"
)

func TestNormalizeIdentifiersAcceptsCommonDOIAndArXivForms(t *testing.T) {
	ids, err := normalizeIdentifiers([]string{"https://doi.org/10.48550/arXiv.2601.12345"}, "", "", "", "", "")
	if err != nil || ids.DOI != "10.48550/arxiv.2601.12345" {
		t.Fatalf("DOI normalization = %+v, %v", ids, err)
	}
	ids, err = normalizeIdentifiers([]string{"arXiv:2601.12345v2"}, "", "", "", "", "")
	if err != nil || ids.ArXiv != "2601.12345v2" {
		t.Fatalf("arXiv normalization = %+v, %v", ids, err)
	}
}

func TestNormalizeIdentifiersRejectsAmbiguousOrMultipleInputs(t *testing.T) {
	if _, err := normalizeIdentifiers([]string{"not-an-id"}, "", "", "", "", ""); err == nil {
		t.Fatal("ambiguous identifier accepted")
	}
	if _, err := normalizeIdentifiers([]string{"10.1000/example"}, "10.1000/other", "", "", "", ""); err == nil {
		t.Fatal("positional plus explicit identifier accepted")
	}
}

func TestSearchCommandAllowsSnowballWithoutQuery(t *testing.T) {
	command := newSearchCommand(&options{})
	if err := command.Flags().Set("cites", "10.1000/seed"); err != nil {
		t.Fatal(err)
	}
	if err := command.Args(command, nil); err != nil {
		t.Fatalf("snowball search without query rejected: %v", err)
	}
	if err := command.Flags().Set("cites", ""); err != nil {
		t.Fatal(err)
	}
	if err := command.Args(command, nil); err == nil {
		t.Fatal("search without query or a snowball DOI succeeded")
	}
	for _, name := range []string{"cites", "cited-by", "related-to"} {
		flag := command.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("missing --%s flag", name)
		}
	}
	if got := command.Flags().Lookup("cited-by").Usage; !strings.Contains(got, "backward references") || !strings.Contains(got, "cited_by:") {
		t.Fatalf("cited-by help = %q", got)
	}
}

func TestConfigInitWritesPrivateStructuredConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	var stdout, stderr bytes.Buffer
	root := NewRoot(&stdout, &stderr)
	root.SetArgs([]string{"--config", path, "--json", "config", "init", "--access-mode", "maximal", "--email", "reader@example.test"})
	if err := root.Execute(); err != nil {
		t.Fatalf("config init: %v (%s)", err, stderr.String())
	}
	var output map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("JSON output: %v (%q)", err, stdout.String())
	}
	if output["access_mode"] != "maximal" || output["config_path"] != path {
		t.Fatalf("output = %v", output)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v", info.Mode().Perm())
	}
	cfg, err := config.Load(path)
	if err != nil || cfg.AccessMode != config.ModeMaximal || cfg.Email != "reader@example.test" {
		t.Fatalf("loaded config = %+v, %v", cfg, err)
	}
}
