// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type adapterRepairRunnerFunc func(context.Context, string, string, string) (string, error)

func (f adapterRepairRunnerFunc) Run(ctx context.Context, root, path, provider string) (string, error) {
	return f(ctx, root, path, provider)
}

func TestRewrapAdapterFixtureHeader(t *testing.T) {
	captured := time.Date(2026, 8, 10, 1, 2, 3, 4000000, time.UTC)
	got := rewrapAdapterFixture("<!-- papio-fixture provider=\"old\" scenario=\"success\" origin=\"https://old.example/a?secret=1\" captured=\"2020-01-01T00:00:00Z\" -->\n<html><body>safe</body></html>", "jstor", "drift", "https://www.jstor.org/stable/abc?token=discard", captured)
	wantHeader := "<!-- papio-fixture provider=\"jstor\" scenario=\"drift\" origin=\"https://www.jstor.org/stable/abc\" captured=\"2026-08-10T01:02:03.004Z\" -->"
	if !strings.HasPrefix(got, wantHeader+"\n<html>") {
		t.Fatalf("fixture = %q, want header %q and original body", got, wantHeader)
	}
}

func TestParseAdapterVersionFromEmbeddedTypesSample(t *testing.T) {
	source := `export const adapters = [
  { id: "other", version: "9.9.9", hosts: [] },
  {
    id: "jstor",
    version: "0.3.0",
    hosts: ["jstor.org"],
  },
];`
	got, err := parseAdapterVersion(source, "jstor")
	if err != nil || got != "0.3.0" {
		t.Fatalf("parseAdapterVersion = %q, %v; want 0.3.0", got, err)
	}
}

func TestNextAdapterRevision(t *testing.T) {
	for _, tc := range []struct{ current, want string }{
		{"0.3.0", "0.3.1"},
		{"1.9.9", "1.9.10"},
		{"v2.0.0", "v2.0.1"},
	} {
		t.Run(tc.current, func(t *testing.T) {
			got, err := nextAdapterRevision(tc.current)
			if err != nil || got != tc.want {
				t.Fatalf("nextAdapterRevision(%q) = %q, %v; want %q", tc.current, got, err, tc.want)
			}
		})
	}
}

func TestScaffoldAdapterRepairWorkspaceLayoutDegradesWithoutExtension(t *testing.T) {
	root := t.TempDir()
	capturePath := filepath.Join(root, "capture.html")
	if err := os.WriteFile(capturePath, []byte("<html><body>safe</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "observed.json"), []byte(`{"provider":"jstor","scenario":"observed","origin":"https://www.jstor.org/stable/abc","adapter_version":"0.3.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := scaffoldAdapterRepair(context.Background(), adapterRepairCapture{
		Path: capturePath, Provider: "jstor", Scenario: "observed", Host: "www.jstor.org",
		Origin: "https://www.jstor.org/stable/abc", Captured: time.Date(2026, 8, 10, 1, 2, 3, 0, time.UTC), AdapterVersion: "0.3.0",
	}, adapterRepairDeps{
		RepoRoot: root,
		Now:      func() time.Time { return time.Date(2026, 8, 10, 2, 3, 4, 0, time.UTC) },
		Run: adapterRepairRunnerFunc(func(context.Context, string, string, string) (string, error) {
			t.Fatal("bun runner must not run when extension workspace is absent")
			return "", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fixture.html", "report.md", "apply.md"} {
		if _, err := os.Stat(filepath.Join(result.Workspace, name)); err != nil {
			t.Fatalf("workspace missing %s: %v", name, err)
		}
	}
	fixture, _ := os.ReadFile(result.Fixture)
	if !strings.HasPrefix(string(fixture), "<!-- papio-fixture provider=\"jstor\" scenario=\"observed\"") {
		t.Fatalf("fixture header = %q", fixture)
	}
	if result.NextRevision != "0.3.1" {
		t.Fatalf("next revision = %q, want 0.3.1", result.NextRevision)
	}
	report, _ := os.ReadFile(result.Report)
	if !strings.Contains(string(report), "adapter-try analysis skipped") {
		t.Fatalf("degraded report omitted skip reason: %s", report)
	}
}
