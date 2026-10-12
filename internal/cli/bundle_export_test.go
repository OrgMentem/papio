// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"papio/internal/api"
	"papio/internal/config"
)

// The daemon resolves a relative path against its own working directory, so
// the CLI must send the operator's directory resolved, and print what the
// daemon reports.
func TestBundleExportSendsAnAbsoluteDestination(t *testing.T) {
	t.Chdir(t.TempDir())
	want, err := filepath.Abs("bundle-out")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, method string, params any, result any) error {
		if method != "bundle.export_v2" {
			t.Fatalf("method = %q, want bundle.export_v2", method)
		}
		got := params.(map[string]string)["output_dir"]
		if got != want {
			t.Fatalf("output_dir sent = %q, want the absolute %q", got, want)
		}
		*result.(*api.BundleResult) = api.BundleResult{Path: filepath.Join(got, "bundle.json")}
		return nil
	})
	root.SetArgs([]string{"bundle", "export", "job_01", "-o", "bundle-out"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("bundle export: %v (stderr: %s)", err, errOut.String())
	}
	if wantOut := "Exported " + filepath.Join(want, "bundle.json") + "\n"; out.String() != wantOut {
		t.Fatalf("output = %q, want %q", out.String(), wantOut)
	}
}
