// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentConfigurationEnrollment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.AccessMode = ModeDelegated
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "[agent]") {
		t.Fatal("default config unexpectedly enrolls agent")
	}
	cfg.Agent = &Agent{Backend: "typesafe"}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.Agent == nil || got.Agent.Backend != "typesafe" {
		t.Fatalf("agent=%v error=%v", got.Agent, err)
	}
	cfg.Agent.Backend = "unsupported"
	if err := Save(cfg, path); err == nil {
		t.Fatal("unknown backend accepted")
	}
	if err := os.WriteFile(path, []byte("[agent]\nbackend='typesafe'\napi_key='not-a-real-key'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("TOML secret field accepted")
	}
}
