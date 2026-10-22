// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
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
	"papio/internal/config"
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
	result, err := scaffoldAdapterRepair(context.Background(), captureFromRow(row), adapterRepairDeps{
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
	typesSource, err := os.ReadFile(filepath.Join(root, "extension", "src", "adapters", "types.ts"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := parseAdapterVersion(string(typesSource), "jstor")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := nextAdapterRevision(current); result.NextRevision != want {
		t.Fatalf("next revision = %q, want %q", result.NextRevision, want)
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

// captureFromRow is the adapterRepairCapture a scaffold run is given for a
// stored capture row.
func captureFromRow(row captures.Capture) adapterRepairCapture {
	return adapterRepairCapture{
		Path: row.Path, Provider: row.AdapterID, Scenario: row.Scenario,
		Host: row.Host, Captured: row.Timestamp, AdapterVersion: row.AdapterVersion,
		SHA256: row.SHA256, SanitizerProvenance: row.SanitizerProvenance,
		SanitizerVersion: row.SanitizerVersion, IndependentEvidence: row.IndependentEvidence,
	}
}

// TestScaffoldAdapterRepairRefusesPatch: each row is one reason the scaffold
// must not emit a patch it cannot stand behind, and the report must say why.
func TestScaffoldAdapterRepairRefusesPatch(t *testing.T) {
	for _, test := range []struct {
		name        string
		scenario    string
		independent bool
		now         time.Time
		repairOut   func(t *testing.T, root string) (string, error)
		tryOut      string
		absent      []string
		absentWhy   string
		reportWants []string
		reportWhy   string
	}{
		{
			name: "no complete candidate", scenario: "success", independent: true,
			now: time.Date(2026, 8, 10, 3, 4, 5, 0, time.UTC),
			repairOut: func(*testing.T, string) (string, error) {
				return `{"provider":"jstor","scenario":"success","rule_kind":"article","rule_index":0,"candidates":[]}`, nil
			},
			tryOut:      "adapter-try found no matching selector",
			absent:      []string{"types.ts.patch"},
			absentWhy:   "types.ts.patch exists without a complete candidate",
			reportWants: []string{"No selector candidate passed classifier matching", "No types.ts.patch was emitted"},
			reportWhy:   "report omits the no-candidate reason",
		},
		{
			name: "no independent evidence", scenario: "success", independent: false,
			now: time.Date(2026, 8, 10, 4, 5, 6, 0, time.UTC),
			repairOut: func(t *testing.T, root string) (string, error) {
				return completeAdapterRepairOutput(t, root), nil
			},
			tryOut:      "adapter-try found a missing selector",
			absent:      []string{"types.ts.patch"},
			absentWhy:   "types.ts.patch bypassed the independent-evidence gate",
			reportWants: []string{"revision promotion is locked", "Independent evidence is absent"},
			reportWhy:   "report does not explain the independent-evidence gate",
		},
		{
			name: "adapter lacks the scenario rule", scenario: "no-entitlement", independent: true,
			now: time.Date(2026, 8, 10, 5, 6, 7, 0, time.UTC),
			repairOut: func(*testing.T, string) (string, error) {
				return "", errors.New("adapter jstor has no no_entitlement rule")
			},
			tryOut:      "adapter-try classified the page as unknown",
			absent:      []string{"adapters.test.ts.patch", "types.ts.patch"},
			absentWhy:   "patch exists for an unsupported adapter scenario",
			reportWants: []string{"Patch generation stopped", "adapter jstor has no no_entitlement rule"},
			reportWhy:   "report omits the unsupported-rule reason",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeAdapterRepairTestRepo(t, root)
			row, _ := storeAdapterRepairScenarioCapture(t, root, test.scenario, test.independent)

			result, err := scaffoldAdapterRepair(context.Background(), captureFromRow(row), adapterRepairDeps{
				RepoRoot: root,
				Now:      func() time.Time { return test.now },
				Run: adapterRepairRunnerFunc(func(_ context.Context, _ string, tool string, _ ...string) (string, error) {
					if tool == "tools/adapter-repair.ts" {
						return test.repairOut(t, root)
					}
					return test.tryOut, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range test.absent {
				if _, statErr := os.Stat(filepath.Join(result.Workspace, name)); !os.IsNotExist(statErr) {
					t.Fatalf("%s: %s: %v", test.absentWhy, name, statErr)
				}
			}
			report, err := os.ReadFile(result.Report)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range test.reportWants {
				if !strings.Contains(string(report), want) {
					t.Fatalf("%s: %s", test.reportWhy, report)
				}
			}
		})
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
	current, err := parseAdapterVersion(source, "jstor")
	if err != nil {
		t.Fatal(err)
	}
	next, err := nextAdapterRevision(current)
	if err != nil {
		t.Fatal(err)
	}
	version := strings.Index(source[start:], `version: "`+current+`"`) + start
	source = source[:version] + strings.Replace(source[version:], `version: "`+current+`"`, `version: "`+next+`"`, 1)
	source = strings.ReplaceAll(source, `mfe-download-pharos-button[data-qa='download-pdf'][data-doi][data-sc='but click:pdf download'][variant='primary']`, "a#pdf-download")
	result["patched_source"] = source
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFindAdapterRepairRepoRootRefusesOutsideACheckout(t *testing.T) {
	t.Chdir(t.TempDir())
	root, err := findAdapterRepairRepoRoot()
	if err == nil || !strings.Contains(err.Error(), "must run inside a papio source checkout") {
		t.Fatalf("findAdapterRepairRepoRoot = %q, %v; want a source-checkout error", root, err)
	}
	if entries, _ := os.ReadDir("."); len(entries) != 0 {
		t.Fatalf("working directory gained %d entries, want none", len(entries))
	}
}

// A capture recorded with an uppercase provider can never be repaired, and
// --provider cannot override the stored id, so the error must say to recapture.
func TestResolveAdapterRepairCaptureExplainsAnInvalidStoredProvider(t *testing.T) {
	row := captures.Capture{
		Path: "/data/captures/host/capture.html", Host: "onlinelibrary.wiley.com", AdapterID: "Wiley",
		SHA256:              strings.Repeat("a", 64),
		SanitizerProvenance: captures.SanitizerProvenance, SanitizerVersion: captures.SanitizerVersion,
	}
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(_ context.Context, _ string, _ any, result any) error {
		*result.(*[]captures.Capture) = []captures.Capture{row}
		return nil
	})
	root.SetArgs([]string{"adapter", "repair", "capture.html", "--provider", "wiley", "--scenario", "success"})
	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), `provider "Wiley"`) || !strings.Contains(err.Error(), "recapture") {
		t.Fatalf("err = %v, want a recapture instruction naming the stored provider", err)
	}
}

func TestAdapterCaptureRefusesANonAdapterProvider(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewInProcessRoot(&out, &errOut, config.Config{}, func(context.Context, string, any, any) error {
		t.Fatal("an invalid provider must be refused before the daemon call")
		return nil
	})
	root.SetArgs([]string{"adapter", "capture", "https://www.wiley.com/x", "--provider", "Wiley", "--scenario", "success"})
	if err := root.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "adapter id such as wiley") {
		t.Fatalf("err = %v, want the adapter id refusal", err)
	}
}
