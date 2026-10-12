// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"papio/internal/app"
	"papio/internal/job"
)

// add-component accepts a file from any directory: the CLI stages a copy under
// the data directory and names only that copy, so the daemon never opens a
// path the operator typed and the operator's file stays where it was.
func TestAddComponentStagesTheFileFromAnyPathAndSendsOnlyItsName(t *testing.T) {
	body := []byte("%PDF-1.7\nsupplementary tables\n%%EOF")
	cfg := supplyPDFConfig(t, 1<<20)
	source := filepath.Join(t.TempDir(), "supp.pdf")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatal(err)
	}
	stageDir, err := app.ComponentStagingDir(cfg.DataDir, "job_01")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	var staged string
	root := NewInProcessRoot(&out, &errOut, cfg, func(_ context.Context, method string, params, result any) error {
		if method != "jobs.add_component_staged" {
			t.Fatalf("method = %q, want jobs.add_component_staged", method)
		}
		got := params.(map[string]string)
		if len(got) != 3 || got["job_id"] != "job_01" || got["role"] != "supplement" || got["name"] == "" || strings.ContainsAny(got["name"], `/\`) {
			t.Fatalf("params = %+v; want job_id, role and a bare staged name", got)
		}
		staged = filepath.Join(stageDir, got["name"])
		copied, err := os.ReadFile(staged)
		if err != nil || !bytes.Equal(copied, body) {
			t.Fatalf("staged copy = %q, %v; want the supplied bytes", copied, err)
		}
		*result.(*struct {
			Components []job.Component `json:"components"`
			Truncated  bool            `json:"truncated"`
		}) = struct {
			Components []job.Component `json:"components"`
			Truncated  bool            `json:"truncated"`
		}{Components: []job.Component{{Role: "supplement", SHA256: "abc"}}}
		return nil
	})
	root.SetArgs([]string{"jobs", "add-component", "job_01", source, "--role", "supplement"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("add-component: %v (stderr: %s)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "supplement\tabc") {
		t.Fatalf("output = %q; want the component row", out.String())
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("the operator's file was not left in place: %v", err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("staged copy remains after the call: %v", err)
	}
}

func TestAddComponentRefusesANonPDFBeforeCallingTheDaemon(t *testing.T) {
	cfg := supplyPDFConfig(t, 1<<20)
	source := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(source, []byte("not a pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, cfg, func(context.Context, string, any, any) error {
		t.Fatal("a refused file must never reach the daemon")
		return nil
	})
	root.SetArgs([]string{"jobs", "add-component", "job_01", source, "--role", "supplement"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "not a PDF") {
		t.Fatalf("add-component of a text file = %v; want a not-a-PDF refusal", err)
	}
}
