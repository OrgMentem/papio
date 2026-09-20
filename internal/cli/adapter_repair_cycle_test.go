// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/captures"
)

// This is a controlled regression, not provider acceptance. It traverses the
// daemon capture store, real Bun synthesis, Go patch scaffold, git apply and
// the generated Bun regression. No provider, normal daemon or browser is used.
func TestAdapterRepairCycle(t *testing.T) {
	repo, err := findAdapterRepairRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	_, bunErr := exec.LookPath("bun")
	modules := filepath.Join(repo, "extension", "node_modules")
	_, modulesErr := os.Stat(modules)
	if bunErr != nil || modulesErr != nil {
		if os.Getenv("PAPIO_REPAIR_CYCLE_REQUIRED") == "1" {
			t.Fatal("repair cycle requires bun and installed extension dependencies")
		}
		t.Skip("requires bun and installed extension dependencies; required in extension CI")
	}
	root := t.TempDir()
	write := func(relative string, data []byte) {
		t.Helper()
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, relative := range []string{
		"extension/src/plan.ts", "extension/test/harness.ts",
		"extension/tools/adapter-try.ts", "extension/tools/adapter-repair.ts", "extension/tools/adapter-repair-source.ts",
	} {
		data, err := os.ReadFile(filepath.Join(repo, relative))
		if err != nil {
			t.Fatal(err)
		}
		write(relative, data)
	}
	if err := os.Symlink(modules, filepath.Join(root, "extension", "node_modules")); err != nil {
		t.Fatal(err)
	}
	write("extension/src/adapters/types.ts", []byte(`export const adapters: AdapterSpec[] = [
  {
    id: "jstor",
    version: "0.3.0",
    hosts: ["www.jstor.org"],
    workEvidence: { kind: "doi", selector: "meta[name='citation_doi']", attribute: "content" },
    classify: [
      { kind: "login", all: ["#login", ".old-pdf"] },
      { kind: "article", all: ["#legacy-record", ".old-pdf"] },
      { kind: "article", all: ["#current-record", ".old-pdf"] },
    ],
    download: {
      selector: ".old-pdf", requireKind: "article", method: "href",
      workTarget: { kind: "opaque" },
      allowedDestinations: [{ origin: "https://www.jstor.org", pathPrefix: "/files/" }],
    },
  },
];
`))
	write("extension/test/adapters.test.ts", []byte(`import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { adapters, type AdapterSpec } from "../src/adapters/types";
import { planExecution } from "../src/plan";
import { captureOrigin, fixturePath, parseHTML } from "./harness";
test("existing access and legacy rules remain intact", () => {
  expect(adapters[0].classify[0].all).toEqual(["#login", ".old-pdf"]);
  expect(adapters[0].classify[1].all).toEqual(["#legacy-record", ".old-pdf"]);
});
`))
	write("extension/fixtures/jstor/drift.html", []byte("original negative fixture\n"))
	store := captures.New(root, captures.Retention{MaxPerHost: 2, MaxAge: 24 * time.Hour})
	fixture := []byte(`<!-- papio-fixture provider="jstor" scenario="drift" origin="https://www.jstor.org/stable/abc" captured="2026-09-20T00:00:00Z" -->
<html><head><meta name="citation_doi" content="10.2307/repair"></head><body>
<div id="login"></div><main id="current-record"><a id="pdf-download" href="/files/paper.pdf">Download PDF</a></main>
</body></html>`)
	path, err := store.StoreSanitized(context.Background(), "www.jstor.org", "drift", "jstor", "0.3.0", fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateJob(context.Background(), "job-controlled-regression", path, path); err != nil {
		t.Fatal(err)
	}
	rows, err := store.List(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("capture: %v, %v", rows, err)
	}
	row := rows[0]
	capture := adapterRepairCapture{Path: row.Path, Provider: row.AdapterID, Scenario: row.Scenario,
		Host: row.Host, Captured: row.Timestamp, AdapterVersion: row.AdapterVersion, SHA256: row.SHA256,
		SanitizerProvenance: row.SanitizerProvenance, SanitizerVersion: row.SanitizerVersion, IndependentEvidence: row.IndependentEvidence}
	deps := adapterRepairDeps{RepoRoot: root, Now: func() time.Time { return time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC) }}
	result, err := scaffoldAdapterRepair(context.Background(), capture, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "proposal" {
		report, _ := os.ReadFile(result.Report)
		t.Fatalf("repair did not produce a proposal: %s", report)
	}
	run := func(dir string, wantSuccess bool, command string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), command, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("%s %v: %v\n%s", command, args, err, out)
		}
		return string(out)
	}
	run(root, true, "git", "apply", "--check", filepath.Join(result.Workspace, "adapters.test.ts.patch"), filepath.Join(result.Workspace, "types.ts.patch"))
	run(root, true, "git", "apply", filepath.Join(result.Workspace, "adapters.test.ts.patch"))
	// The generated test must fail, not skip, when its fixture wasn't copied.
	missing := run(filepath.Join(root, "extension"), false, "bun", "test", "test/adapters.test.ts")
	if !strings.Contains(missing, "ENOENT") {
		t.Fatalf("missing fixture failed for another reason: %s", missing)
	}
	fixtureRelative := "extension/fixtures/jstor/repair-" + row.SHA256 + ".html"
	emitted, err := os.ReadFile(filepath.Join(result.Workspace, fixtureRelative))
	if err != nil {
		t.Fatal(err)
	}
	write(fixtureRelative, emitted)
	before := run(filepath.Join(root, "extension"), false, "bun", "test", "test/adapters.test.ts")
	if !strings.Contains(before, `Received: "unknown"`) {
		t.Fatalf("old adapter failed for another reason: %s", before)
	}
	run(root, true, "git", "apply", filepath.Join(result.Workspace, "types.ts.patch"))
	run(filepath.Join(root, "extension"), true, "bun", "test", "test/adapters.test.ts")
	oldFixture, err := os.ReadFile(filepath.Join(root, "extension/fixtures/jstor/drift.html"))
	if err != nil || string(oldFixture) != "original negative fixture\n" {
		t.Fatalf("original fixture overwritten: %s, %v", oldFixture, err)
	}
	// Re-analysis in the same second must not expose the preceding source patch.
	again, err := scaffoldAdapterRepair(context.Background(), capture, deps)
	if err != nil {
		t.Fatal(err)
	}
	if again.Workspace == result.Workspace || again.Outcome != "blocked" {
		t.Fatalf("repeat reused a repair: %+v", again)
	}
	for _, name := range []string{"types.ts.patch", "adapters.test.ts.patch"} {
		if _, err := os.Stat(filepath.Join(again.Workspace, name)); !os.IsNotExist(err) {
			t.Fatalf("stale %s present: %v", name, err)
		}
	}
	apply, err := os.ReadFile(filepath.Join(again.Workspace, "apply.md"))
	if err != nil || strings.Contains(string(apply), "git apply") {
		t.Fatalf("blocked repair suggests applying: %s, %v", apply, err)
	}
}
