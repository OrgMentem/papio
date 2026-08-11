// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/captures"
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

func TestScaffoldAdapterRepairRejectsUnprovenancedPrivateHTML(t *testing.T) {
	root := t.TempDir()
	capturePath := filepath.Join(root, "capture.html")
	secret := "COOKIE_SENTINEL"
	if err := os.WriteFile(capturePath, []byte("<html><body>"+secret+"</body></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := scaffoldAdapterRepair(context.Background(), adapterRepairCapture{
		Path: capturePath, Provider: "jstor", Scenario: "success",
		Host: "www.jstor.org", Origin: "https://www.jstor.org/stable/abc",
		SHA256:              "0000000000000000000000000000000000000000000000000000000000000000",
		SanitizerProvenance: captures.SanitizerProvenance, SanitizerVersion: captures.SanitizerVersion,
	}, adapterRepairDeps{RepoRoot: root})
	if err == nil || (!strings.Contains(err.Error(), "canonical") && !strings.Contains(err.Error(), "SHA-256")) {
		t.Fatalf("scaffold error = %v, want canonical provenance/hash rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "dev", "scratch", "repair")); !os.IsNotExist(statErr) {
		t.Fatalf("untrusted capture created a repair workspace: %v", statErr)
	}
}

func TestScaffoldAdapterRepairCertifiesExactEmittedBytes(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "extension", "tools"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "extension", "tools", "adapter-try.ts"), []byte("// test seam\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "extension", "src", "adapters"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "extension", "src", "adapters", "types.ts"), []byte(`{ id: "jstor", version: "0.3.0" }`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := captures.New(root, captures.Retention{MaxPerHost: 2, MaxAge: 24 * time.Hour})
	fixture := []byte("<!-- papio-fixture provider=\"jstor\" scenario=\"success\" origin=\"https://www.jstor.org/stable/abc\" captured=\"2026-08-10T00:00:00Z\" -->\n<html><body>safe</body></html>")
	path, err := store.StoreSanitized(context.Background(), "www.jstor.org", "success", "jstor", "0.3.0", fixture)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.List(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("capture rows = %#v, %v", rows, err)
	}
	if err := store.UpdateJob(context.Background(), "job-independent", path, path); err != nil {
		t.Fatal(err)
	}
	rows, err = store.List(context.Background())
	if err != nil || !rows[0].IndependentEvidence {
		t.Fatalf("independent evidence = %#v, %v", rows, err)
	}

	var runnerPath string
	result, err := scaffoldAdapterRepair(context.Background(), adapterRepairCapture{
		Path: path, Provider: rows[0].AdapterID, Scenario: rows[0].Scenario,
		Host: rows[0].Host, Captured: rows[0].Timestamp, AdapterVersion: rows[0].AdapterVersion,
		SHA256: rows[0].SHA256, SanitizerProvenance: rows[0].SanitizerProvenance,
		SanitizerVersion: rows[0].SanitizerVersion, IndependentEvidence: rows[0].IndependentEvidence,
	}, adapterRepairDeps{
		RepoRoot: root,
		Now:      func() time.Time { return time.Date(2026, 8, 10, 2, 3, 4, 0, time.UTC) },
		Run: adapterRepairRunnerFunc(func(_ context.Context, _ string, p, _ string) (string, error) {
			runnerPath = p
			data, readErr := os.ReadFile(p)
			if readErr != nil {
				return "", readErr
			}
			sum := sha256.Sum256(data)
			return "plan fixture sha256=" + hex.EncodeToString(sum[:]), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	emitted, err := os.ReadFile(result.Fixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(emitted) != string(fixture) {
		t.Fatalf("emitted fixture differs from canonical capture")
	}
	runnerBytes, err := os.ReadFile(runnerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(runnerBytes) != string(emitted) {
		t.Fatalf("adapter-try received bytes different from emitted fixture")
	}
	report, err := os.ReadFile(result.Report)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(emitted)
	if !strings.Contains(string(report), "Fixture SHA-256: `"+hex.EncodeToString(sum[:])+"`") ||
		!strings.Contains(string(report), "plan fixture sha256="+hex.EncodeToString(sum[:])) {
		t.Fatalf("report does not certify exact adapter-try bytes: %s", report)
	}
	if result.NextRevision != "0.3.1" {
		t.Fatalf("next revision = %q, want 0.3.1", result.NextRevision)
	}
}
