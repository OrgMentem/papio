// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/captures"
)

type adapterRepairRunnerFunc func(context.Context, string, string, ...string) (string, error)

func (f adapterRepairRunnerFunc) Run(ctx context.Context, root, tool string, args ...string) (string, error) {
	return f(ctx, root, tool, args...)
}

const completeAdapterRepairCandidate = `{"provider":"jstor","scenario":"success","rule_kind":"article","rule_index":2,"candidates":[{"score":100,"selector":"a#pdf-download","outer_html":"<a id=\"pdf-download\" data-doi=\"10.2307/repair\">PDF</a>","classifier_verified":true,"plan_complete":true,"replace_selector":"mfe-download-pharos-button[data-qa='download-pdf'][data-doi][data-sc='but click:pdf download'][variant='primary']"}]}`

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

func TestScaffoldAdapterRepairWritesApplicablePatches(t *testing.T) {
	root := t.TempDir()
	writeAdapterRepairTestRepo(t, root)
	row, fixture := storeAdapterRepairTestCapture(t, root, true)

	var runnerPath string
	result, err := scaffoldAdapterRepair(context.Background(), adapterRepairCapture{
		Path: row.Path, Provider: row.AdapterID, Scenario: row.Scenario,
		Host: row.Host, Captured: row.Timestamp, AdapterVersion: row.AdapterVersion,
		SHA256: row.SHA256, SanitizerProvenance: row.SanitizerProvenance,
		SanitizerVersion: row.SanitizerVersion, IndependentEvidence: row.IndependentEvidence,
	}, adapterRepairDeps{
		RepoRoot: root,
		Now:      func() time.Time { return time.Date(2026, 8, 10, 2, 3, 4, 0, time.UTC) },
		Run: adapterRepairRunnerFunc(func(_ context.Context, _ string, tool string, args ...string) (string, error) {
			if tool == "tools/adapter-repair.ts" {
				return completeAdapterRepairOutput(t, root), nil
			}
			runnerPath = args[0]
			data, readErr := os.ReadFile(runnerPath)
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
	finalFixture, err := os.ReadFile(filepath.Join(result.Workspace, "extension", "fixtures", "jstor", "repair-"+row.SHA256+".html"))
	if err != nil || string(finalFixture) != string(fixture) {
		t.Fatalf("final fixture = %q, %v; want canonical bytes", finalFixture, err)
	}

	report, err := os.ReadFile(result.Report)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(emitted)
	if !strings.Contains(string(report), "Fixture SHA-256: `"+hex.EncodeToString(sum[:])+"`") ||
		!strings.Contains(string(report), "plan fixture sha256="+hex.EncodeToString(sum[:])) ||
		!strings.Contains(string(report), "Top candidate with a complete plan: `a#pdf-download`") ||
		!strings.Contains(string(report), "downloaded, adopted, identity-validated PDF") {
		t.Fatalf("report does not certify the fixture and bound candidate: %s", report)
	}
	if result.NextRevision != "0.3.1" {
		t.Fatalf("next revision = %q, want 0.3.1", result.NextRevision)
	}
	apply, err := os.ReadFile(filepath.Join(result.Workspace, "apply.md"))
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(root, result.Workspace)
	// The generated instructions are shell commands and Git paths on every OS.
	rel = filepath.ToSlash(rel)
	fixtureName := "repair-" + row.SHA256 + ".html"
	expectedApply := fmt.Sprintf("# Apply this reviewed repair\n\nThis is a development proposal, not a verified provider repair. Review the fixture for private data before copying it into tracked source.\n\nRun from an isolated development checkout after reviewing `report.md` and both generated diffs.\n\n```sh\ngit apply %s/adapters.test.ts.patch %s/types.ts.patch\nmkdir -p extension/fixtures/jstor\ncp %s/extension/fixtures/jstor/%s extension/fixtures/jstor/%s\n(cd extension && bun test test/adapters.test.ts)\n```\n\nThen run the full extension tests, typecheck and both builds. Reload the development extension and confirm its new browser session ID. Use a fresh isolated acquisition with imports disabled; record Open actions, sign-ins and other interventions separately. Require download, adoption and validation, then inspect the PDF first page and page count. A passing fixture is not live acceptance.\n", rel, rel, rel, fixtureName, fixtureName)

	if string(apply) != expectedApply {
		t.Fatalf("apply.md = %q; want %q", apply, expectedApply)
	}

	typesPatch := filepath.Join(result.Workspace, "types.ts.patch")
	command := exec.Command("git", "apply", "--check", typesPatch)
	command.Dir = root
	if output, applyErr := command.CombinedOutput(); applyErr != nil {
		t.Fatalf("git apply --check types.ts.patch: %v\n%s", applyErr, output)
	}
	testPatch := filepath.Join(result.Workspace, "adapters.test.ts.patch")
	command = exec.Command("git", "apply", "--check", testPatch)
	command.Dir = root
	if output, applyErr := command.CombinedOutput(); applyErr != nil {
		t.Fatalf("git apply --check adapters.test.ts.patch: %v\n%s", applyErr, output)
	}
	testPatchBytes, readErr := os.ReadFile(testPatch)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(testPatchBytes), "const planned = planExecution(page, spec, expectedWork, {});") ||
		!strings.Contains(string(testPatchBytes), `expect("assisted" in planned).toBe(false)`) {
		t.Fatalf("generated test does not require a complete identity-bound plan: %s", testPatchBytes)
	}
	command = exec.Command("git", "apply", typesPatch)
	command.Dir = root
	if output, applyErr := command.CombinedOutput(); applyErr != nil {
		t.Fatalf("git apply types.ts.patch: %v\n%s", applyErr, output)
	}
	patchedTypes, readErr := os.ReadFile(filepath.Join(root, "extension", "src", "adapters", "types.ts"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	oldSelector := "mfe-download-pharos-button[data-qa='download-pdf'][data-doi][data-sc='but click:pdf download'][variant='primary']"
	if strings.Contains(string(patchedTypes), oldSelector) || strings.Count(string(patchedTypes), "a#pdf-download") < 3 {
		t.Fatalf("types patch did not update classify, download, and work-evidence selectors")
	}
}

func TestScaffoldAdapterRepairOmitsTypesPatchWithoutCompleteCandidate(t *testing.T) {
	root := t.TempDir()
	writeAdapterRepairTestRepo(t, root)
	row, _ := storeAdapterRepairTestCapture(t, root, true)

	result, err := scaffoldAdapterRepair(context.Background(), adapterRepairCapture{
		Path: row.Path, Provider: row.AdapterID, Scenario: row.Scenario,
		Host: row.Host, Captured: row.Timestamp, AdapterVersion: row.AdapterVersion,
		SHA256: row.SHA256, SanitizerProvenance: row.SanitizerProvenance,
		SanitizerVersion: row.SanitizerVersion, IndependentEvidence: row.IndependentEvidence,
	}, adapterRepairDeps{
		RepoRoot: root,
		Now:      func() time.Time { return time.Date(2026, 8, 10, 3, 4, 5, 0, time.UTC) },
		Run: adapterRepairRunnerFunc(func(_ context.Context, _ string, tool string, _ ...string) (string, error) {
			if tool == "tools/adapter-repair.ts" {
				return `{"provider":"jstor","scenario":"success","rule_kind":"article","rule_index":0,"candidates":[]}`, nil
			}
			return "adapter-try found no matching selector", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(result.Workspace, "types.ts.patch")); !os.IsNotExist(statErr) {
		t.Fatalf("types.ts.patch exists without a complete candidate: %v", statErr)
	}
	report, err := os.ReadFile(result.Report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "No selector candidate passed classifier matching") ||
		!strings.Contains(string(report), "No types.ts.patch was emitted") {
		t.Fatalf("report omits the no-candidate reason: %s", report)
	}
}

func TestScaffoldAdapterRepairKeepsRevisionLockedWithoutIndependentEvidence(t *testing.T) {
	root := t.TempDir()
	writeAdapterRepairTestRepo(t, root)
	row, _ := storeAdapterRepairTestCapture(t, root, false)

	result, err := scaffoldAdapterRepair(context.Background(), adapterRepairCapture{
		Path: row.Path, Provider: row.AdapterID, Scenario: row.Scenario,
		Host: row.Host, Captured: row.Timestamp, AdapterVersion: row.AdapterVersion,
		SHA256: row.SHA256, SanitizerProvenance: row.SanitizerProvenance,
		SanitizerVersion: row.SanitizerVersion, IndependentEvidence: row.IndependentEvidence,
	}, adapterRepairDeps{
		RepoRoot: root,
		Now:      func() time.Time { return time.Date(2026, 8, 10, 4, 5, 6, 0, time.UTC) },
		Run: adapterRepairRunnerFunc(func(_ context.Context, _ string, tool string, _ ...string) (string, error) {
			if tool == "tools/adapter-repair.ts" {
				return completeAdapterRepairOutput(t, root), nil
			}
			return "adapter-try found a missing selector", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(result.Workspace, "types.ts.patch")); !os.IsNotExist(statErr) {
		t.Fatalf("types.ts.patch bypassed the independent-evidence gate: %v", statErr)
	}
	report, err := os.ReadFile(result.Report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "revision promotion is locked") ||
		!strings.Contains(string(report), "Independent evidence is absent") {
		t.Fatalf("report does not explain the independent-evidence gate: %s", report)
	}
}

func TestScaffoldAdapterRepairStopsWhenAdapterLacksScenarioRule(t *testing.T) {
	root := t.TempDir()
	writeAdapterRepairTestRepo(t, root)
	row, _ := storeAdapterRepairScenarioCapture(t, root, "no-entitlement", true)

	result, err := scaffoldAdapterRepair(context.Background(), adapterRepairCapture{
		Path: row.Path, Provider: row.AdapterID, Scenario: row.Scenario,
		Host: row.Host, Captured: row.Timestamp, AdapterVersion: row.AdapterVersion,
		SHA256: row.SHA256, SanitizerProvenance: row.SanitizerProvenance,
		SanitizerVersion: row.SanitizerVersion, IndependentEvidence: row.IndependentEvidence,
	}, adapterRepairDeps{
		RepoRoot: root,
		Now:      func() time.Time { return time.Date(2026, 8, 10, 5, 6, 7, 0, time.UTC) },
		Run: adapterRepairRunnerFunc(func(_ context.Context, _ string, tool string, _ ...string) (string, error) {
			if tool == "tools/adapter-repair.ts" {
				return "", errors.New("adapter jstor has no no_entitlement rule")
			}
			return "adapter-try classified the page as unknown", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"adapters.test.ts.patch", "types.ts.patch"} {
		if _, statErr := os.Stat(filepath.Join(result.Workspace, name)); !os.IsNotExist(statErr) {
			t.Fatalf("%s exists for an unsupported adapter scenario: %v", name, statErr)
		}
	}
	report, err := os.ReadFile(result.Report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "Patch generation stopped") ||
		!strings.Contains(string(report), "adapter jstor has no no_entitlement rule") {
		t.Fatalf("report omits the unsupported-rule reason: %s", report)
	}
}

func writeAdapterRepairTestRepo(t *testing.T, root string) {
	t.Helper()
	for _, directory := range []string{
		filepath.Join(root, "extension", "tools"),
		filepath.Join(root, "extension", "src", "adapters"),
		filepath.Join(root, "extension", "test"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"adapter-try.ts", "adapter-repair.ts"} {
		if err := os.WriteFile(filepath.Join(root, "extension", "tools", tool), []byte("// test seam\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repoRoot, err := findAdapterRepairRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		filepath.Join("extension", "src", "adapters", "types.ts"),
		filepath.Join("extension", "test", "adapters.test.ts"),
	} {
		source, readErr := os.ReadFile(filepath.Join(repoRoot, relative))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(root, relative), source, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
}

func storeAdapterRepairTestCapture(t *testing.T, root string, independent bool) (captures.Capture, []byte) {
	t.Helper()
	return storeAdapterRepairScenarioCapture(t, root, "success", independent)
}

func storeAdapterRepairScenarioCapture(t *testing.T, root, scenario string, independent bool) (captures.Capture, []byte) {
	t.Helper()
	store := captures.New(root, captures.Retention{MaxPerHost: 2, MaxAge: 24 * time.Hour})
	fixture := []byte("<!-- papio-fixture provider=\"jstor\" scenario=\"" + scenario + "\" origin=\"https://www.jstor.org/stable/abc\" captured=\"2026-08-10T00:00:00Z\" -->\n<html><body><a id=\"pdf-download\" data-doi=\"10.2307/repair\">PDF</a></body></html>")
	path, err := store.StoreSanitized(context.Background(), "www.jstor.org", scenario, "jstor", "0.3.0", fixture)
	if err != nil {
		t.Fatal(err)
	}
	if independent {
		if err := store.UpdateJob(context.Background(), "job-independent", path, path); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.List(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("capture rows = %#v, %v", rows, err)
	}
	return rows[0], fixture
}

// The unit runner supplies already-edited source; the cross-language cycle
// test exercises the real parser and emitter rather than this seam.
func completeAdapterRepairOutput(t *testing.T, root string) string {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(completeAdapterRepairCandidate), &result); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "extension", "src", "adapters", "types.ts"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	start := strings.Index(source, `id: "jstor"`)
	version := strings.Index(source[start:], `version: "0.3.0"`) + start
	source = source[:version] + strings.Replace(source[version:], `version: "0.3.0"`, `version: "0.3.1"`, 1)
	source = strings.ReplaceAll(source, `mfe-download-pharos-button[data-qa='download-pdf'][data-doi][data-sc='but click:pdf download'][variant='primary']`, "a#pdf-download")
	result["patched_source"] = source
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
