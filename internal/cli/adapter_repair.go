// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/captures"
)

var (
	adapterRepairSegmentRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	adapterVersionRE       = regexp.MustCompile(`^[vV]?(\d+)\.(\d+)\.(\d+)(?:[-+][0-9A-Za-z.-]+)?$`)
	adapterFixtureHeaderRE = regexp.MustCompile(`^<!-- papio-fixture provider="([^"]+)" scenario="([^"]+)" origin="([^"]+)" captured="([^"]+)" -->$`)
)

type adapterRepairCapture struct {
	Path                string
	Provider            string
	Scenario            string
	Host                string
	Origin              string
	Captured            time.Time
	AdapterVersion      string
	SHA256              string
	SanitizerProvenance string
	SanitizerVersion    string
	IndependentEvidence bool
}
type adapterRepairMetadata struct {
	Provider            string    `json:"provider,omitempty"`
	AdapterID           string    `json:"adapter_id,omitempty"`
	Scenario            string    `json:"scenario,omitempty"`
	Host                string    `json:"host,omitempty"`
	Origin              string    `json:"origin,omitempty"`
	Captured            time.Time `json:"captured,omitempty"`
	Timestamp           time.Time `json:"timestamp,omitempty"`
	AdapterVersion      string    `json:"adapter_version,omitempty"`
	SHA256              string    `json:"sha256,omitempty"`
	SanitizerProvenance string    `json:"sanitizer_provenance,omitempty"`
	SanitizerVersion    string    `json:"sanitizer_version,omitempty"`
	IndependentEvidence bool      `json:"independent_evidence,omitempty"`
}

type adapterRepairRunner interface {
	Run(context.Context, string, string, ...string) (string, error)
}

type execAdapterRepairRunner struct{}

func (execAdapterRepairRunner) Run(ctx context.Context, repoRoot, tool string, args ...string) (string, error) {
	commandArgs := append([]string{"run", "--cwd", "extension", tool}, args...)
	command := exec.CommandContext(ctx, "bun", commandArgs...)
	command.Dir = repoRoot
	output, err := command.CombinedOutput()
	return string(output), err
}

type adapterRepairDeps struct {
	Now      func() time.Time
	Run      adapterRepairRunner
	RepoRoot string
}
type adapterRepairResult struct {
	Workspace           string `json:"workspace"`
	Fixture             string `json:"fixture"`
	Report              string `json:"report"`
	NextRevision        string `json:"next_revision"`
	IndependentEvidence bool   `json:"independent_evidence"`
}

type adapterRepairCandidate struct {
	Score              int    `json:"score"`
	Selector           string `json:"selector"`
	OuterHTML          string `json:"outer_html"`
	ClassifierVerified bool   `json:"classifier_verified"`
	PlanComplete       bool   `json:"plan_complete"`
	ReplaceSelector    string `json:"replace_selector"`
}

type adapterRepairCandidates struct {
	Provider   string                   `json:"provider"`
	Scenario   string                   `json:"scenario"`
	RuleKind   string                   `json:"rule_kind"`
	RuleIndex  int                      `json:"rule_index"`
	Candidates []adapterRepairCandidate `json:"candidates"`
}

func newAdapterRepairCommand(opt *options) *cobra.Command {
	var provider, scenario string
	command := &cobra.Command{
		Use:   "repair <capture-id-or-path>",
		Short: "Scaffold a reviewed adapter repair workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			capture, err := resolveAdapterRepairCapture(cmd.Context(), opt, args[0], provider, scenario)
			if err != nil {
				return err
			}
			repoRoot, err := findAdapterRepairRepoRoot()
			if err != nil {
				return err
			}
			result, err := scaffoldAdapterRepair(cmd.Context(), capture, adapterRepairDeps{
				Now:      time.Now,
				Run:      execAdapterRepairRunner{},
				RepoRoot: repoRoot,
			})
			if err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			_, err = fmt.Fprintf(opt.out, "%s\nReview report.md, then use apply.md to apply the generated patches and fixture.\n", result.Workspace)
			return err
		},
	}
	command.Flags().StringVar(&provider, "provider", "", "provider adapter id (must match daemon capture metadata)")
	command.Flags().StringVar(&scenario, "scenario", "", "fixture scenario (must match daemon capture metadata)")
	return command
}

func resolveAdapterRepairCapture(ctx context.Context, opt *options, input, providerFlag, scenarioFlag string) (adapterRepairCapture, error) {
	// A path is only an identifier for a row returned by the daemon. Reading
	// an arbitrary local file would let secrets bypass the daemon sanitizer.
	var rows []captures.Capture
	if err := opt.call(ctx, "adapter.captures.list", struct{}{}, &rows); err != nil {
		return adapterRepairCapture{}, err
	}
	matches := make([]captures.Capture, 0, 1)
	for _, row := range rows {
		path := filepath.Clean(row.Path)
		base := filepath.Base(path)
		if input == path || input == base || strings.HasPrefix(path, input) || strings.HasPrefix(base, input) {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return adapterRepairCapture{}, fmt.Errorf("no daemon-stored capture matches %q; raw local HTML is not accepted", input)
	}
	if len(matches) > 1 {
		return adapterRepairCapture{}, fmt.Errorf("capture prefix %q is ambiguous (%d matches)", input, len(matches))
	}
	row := matches[0]
	if row.Path == "" {
		return adapterRepairCapture{}, errors.New("stored capture has no path")
	}
	if strings.TrimSpace(row.SHA256) == "" || !validSHA256(row.SHA256) {
		return adapterRepairCapture{}, errors.New("stored capture has missing or invalid content SHA-256")
	}
	if row.SanitizerProvenance != captures.SanitizerProvenance || row.SanitizerVersion != captures.SanitizerVersion {
		return adapterRepairCapture{}, errors.New("stored capture has missing or unsupported sanitizer provenance")
	}
	provider := strings.TrimSpace(providerFlag)
	if provider != "" && provider != strings.TrimSpace(row.AdapterID) {
		return adapterRepairCapture{}, errors.New("provider override conflicts with daemon capture metadata")
	}
	if provider == "" {
		provider = strings.TrimSpace(row.AdapterID)
	}
	scenario := strings.TrimSpace(scenarioFlag)
	if scenario != "" && scenario != strings.TrimSpace(row.Scenario) {
		return adapterRepairCapture{}, errors.New("scenario override conflicts with daemon capture metadata")
	}
	if scenario == "" {
		scenario = strings.TrimSpace(row.Scenario)
	}
	capture := adapterRepairCapture{
		Path: row.Path, Provider: provider, Scenario: scenario, Host: row.Host,
		Captured: row.Timestamp, AdapterVersion: row.AdapterVersion, SHA256: row.SHA256,
		SanitizerProvenance: row.SanitizerProvenance, SanitizerVersion: row.SanitizerVersion,
		IndependentEvidence: row.IndependentEvidence,
	}
	if err := validateDaemonCaptureRow(capture); err != nil {
		return adapterRepairCapture{}, err
	}
	return finishAdapterRepairCapture(row.Path, capture)
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validateDaemonCaptureRow(capture adapterRepairCapture) error {
	info, err := os.Lstat(capture.Path)
	if err != nil {
		return fmt.Errorf("read daemon capture %q: %w", capture.Path, err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("daemon capture path is no longer a regular canonical file")
	}
	data, err := os.ReadFile(capture.Path)
	if err != nil {
		return fmt.Errorf("read daemon capture %q: %w", capture.Path, err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != strings.ToLower(capture.SHA256) {
		return errors.New("daemon capture content SHA-256 does not match its list row")
	}
	if !captures.IsSanitizedFixture(data) {
		return errors.New("daemon capture is missing its canonical extension-sanitized fixture header")
	}
	first, _, _ := strings.Cut(string(data), "\n")
	header := adapterFixtureHeaderRE.FindStringSubmatch(strings.TrimSuffix(first, "\r"))
	if len(header) != 5 || header[1] != capture.Provider || header[2] != capture.Scenario {
		return errors.New("daemon capture fixture header does not match its canonical list row")
	}
	metadataPath := strings.TrimSuffix(capture.Path, filepath.Ext(capture.Path)) + ".json"
	metadataInfo, err := os.Lstat(metadataPath)
	if err != nil {
		return fmt.Errorf("read daemon capture metadata: %w", err)
	}
	if !metadataInfo.Mode().IsRegular() {
		return errors.New("daemon capture metadata path is not a regular canonical file")
	}
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("read daemon capture metadata: %w", err)
	}
	var metadata adapterRepairMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("decode daemon capture metadata: %w", err)
	}
	if metadata.SHA256 != capture.SHA256 ||
		metadata.SanitizerProvenance != capture.SanitizerProvenance ||
		metadata.SanitizerVersion != capture.SanitizerVersion ||
		metadata.AdapterID != capture.Provider ||
		metadata.AdapterVersion != capture.AdapterVersion ||
		metadata.IndependentEvidence != capture.IndependentEvidence {
		return errors.New("daemon capture metadata does not match its canonical list row")
	}
	return nil
}

func finishAdapterRepairCapture(path string, capture adapterRepairCapture) (adapterRepairCapture, error) {
	if capture.Provider == "" || !adapterRepairSegmentRE.MatchString(capture.Provider) {
		return adapterRepairCapture{}, fmt.Errorf("capture %q requires a valid provider (use --provider)", path)
	}
	if capture.Scenario == "" || !adapterRepairSegmentRE.MatchString(capture.Scenario) {
		return adapterRepairCapture{}, fmt.Errorf("capture %q requires a valid scenario (use --scenario)", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return adapterRepairCapture{}, fmt.Errorf("read capture %q: %w", path, err)
	}
	if match := adapterFixtureHeaderRE.FindStringSubmatch(strings.SplitN(string(data), "\n", 2)[0]); match != nil {
		if capture.Origin == "" {
			capture.Origin = match[3]
		}
		if parsed, err := time.Parse(time.RFC3339Nano, match[4]); err == nil && capture.Captured.IsZero() {
			capture.Captured = parsed.UTC()
		}
	}
	if capture.Origin == "" {
		capture.Origin = "https://" + capture.Host + "/"
	}
	capture.Origin = normalizeRepairOrigin(capture.Origin, capture.Host)
	if capture.Captured.IsZero() {
		capture.Captured = time.Now().UTC()
	}
	return capture, nil
}

func normalizeRepairOrigin(raw, fallbackHost string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "https://" + fallbackHost + "/"
	}
	u.RawQuery, u.Fragment = "", ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

func findAdapterRepairRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "extension", "tools", "adapter-try.ts")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd, nil
		}
	}
}

func validateRepairCapture(capture adapterRepairCapture) error {
	if capture.Provider == "" || !adapterRepairSegmentRE.MatchString(capture.Provider) {
		return errors.New("repair capture has no canonical provider")
	}
	if capture.Scenario == "" || !adapterRepairSegmentRE.MatchString(capture.Scenario) {
		return errors.New("repair capture has no canonical scenario")
	}
	if !validSHA256(capture.SHA256) {
		return errors.New("repair capture has no valid canonical content SHA-256")
	}
	if capture.SanitizerProvenance != captures.SanitizerProvenance ||
		capture.SanitizerVersion != captures.SanitizerVersion {
		return errors.New("repair capture lacks canonical sanitizer provenance")
	}
	return validateDaemonCaptureRow(capture)
}
func scaffoldAdapterRepair(ctx context.Context, capture adapterRepairCapture, deps adapterRepairDeps) (adapterRepairResult, error) {
	if err := validateRepairCapture(capture); err != nil {
		return adapterRepairResult{}, err
	}

	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Run == nil {
		deps.Run = execAdapterRepairRunner{}
	}
	if deps.RepoRoot == "" {
		deps.RepoRoot, _ = findAdapterRepairRepoRoot()
	}
	now := deps.Now().UTC()
	repairRoot := filepath.Join(deps.RepoRoot, "dev", "scratch", "repair")
	if err := os.MkdirAll(repairRoot, 0o700); err != nil {
		return adapterRepairResult{}, fmt.Errorf("create repair scratch root: %w", err)
	}
	name := capture.Provider + "-" + now.Format("20060102T150405Z")
	workspace := filepath.Join(repairRoot, name)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return adapterRepairResult{}, fmt.Errorf("create repair workspace: %w", err)
	}

	raw, err := os.ReadFile(capture.Path)
	if err != nil {
		return adapterRepairResult{}, fmt.Errorf("read capture: %w", err)
	}
	// The capture was already verified against its daemon hash, metadata, and
	// canonical sanitizer provenance above. Emit those exact bytes; prepending
	// or rewriting a header would make the certified plan differ from the
	// fixture the maintainer is asked to commit.
	fixturePath := filepath.Join(workspace, "fixture.html")
	reportPath := filepath.Join(workspace, "report.md")
	applyPath := filepath.Join(workspace, "apply.md")
	finalFixtureRelative := filepath.Join("extension", "fixtures", capture.Provider, capture.Scenario+".html")
	finalFixturePath := filepath.Join(workspace, finalFixtureRelative)
	if err := os.MkdirAll(filepath.Dir(finalFixturePath), 0o700); err != nil {
		return adapterRepairResult{}, fmt.Errorf("create final fixture directory: %w", err)
	}
	// #nosec G703 -- workspace is under the repo scratch root and both variable
	// path segments passed adapterRepairSegmentRE before these writes.
	if err := os.WriteFile(fixturePath, raw, 0o600); err != nil {
		return adapterRepairResult{}, fmt.Errorf("write fixture: %w", err)
	}
	// #nosec G703 -- same provenance as the write above: finalFixturePath is
	// built from the repo scratch root and segments that passed
	// adapterRepairSegmentRE.
	if err := os.WriteFile(finalFixturePath, raw, 0o600); err != nil {
		return adapterRepairResult{}, fmt.Errorf("write final fixture path: %w", err)
	}

	currentVersion, versionLine, sourceStatus := "unavailable", 0, "extension workspace unavailable"
	typesPath := filepath.Join(deps.RepoRoot, "extension", "src", "adapters", "types.ts")
	typesSource, typesReadErr := os.ReadFile(typesPath)
	if typesReadErr == nil {
		if parsed, parseErr := parseAdapterVersion(string(typesSource), capture.Provider); parseErr == nil {
			currentVersion = parsed
			versionLine = adapterVersionLine(string(typesSource), capture.Provider)
			sourceStatus = "current version read from extension/src/adapters/types.ts"
		} else if capture.AdapterVersion != "" {
			currentVersion = capture.AdapterVersion
			sourceStatus = fmt.Sprintf("%v; using stored capture adapter version", parseErr)
		} else {
			sourceStatus = parseErr.Error()
		}
	} else if capture.AdapterVersion != "" {
		currentVersion = capture.AdapterVersion
		sourceStatus = "extension workspace unavailable; using stored capture adapter version"
	}
	nextRevision := "unknown"
	evidenceStatus := "untrusted caller-labelled scenario; revision promotion is locked"
	if capture.IndependentEvidence {
		evidenceStatus = "independent daemon-correlated provider outcome"
		if next, nextErr := nextAdapterRevision(currentVersion); nextErr == nil {
			nextRevision = next
		}
	}

	analysis := ""
	if _, statErr := os.Stat(filepath.Join(deps.RepoRoot, "extension", "tools", "adapter-try.ts")); statErr != nil {
		analysis = "adapter-try analysis skipped: extension/tools/adapter-try.ts is unavailable"
	} else {
		output, runErr := deps.Run.Run(ctx, deps.RepoRoot, "tools/adapter-try.ts", fixturePath, "--id", capture.Provider)
		analysis = output
		if runErr != nil {
			analysis += fmt.Sprintf("\n(adapter-try analysis skipped or failed: %v)\n", runErr)
		}
	}

	ruleKind, kindErr := adapterRepairRuleKind(capture.Scenario)
	candidateStatus := ""
	generationSupported := false
	var candidates adapterRepairCandidates
	if kindErr != nil {
		candidateStatus = "Patch generation stopped: " + kindErr.Error()
	} else if _, statErr := os.Stat(filepath.Join(deps.RepoRoot, "extension", "tools", "adapter-repair.ts")); statErr != nil {
		candidateStatus = "Patch generation stopped: extension/tools/adapter-repair.ts is unavailable."
	} else {
		output, runErr := deps.Run.Run(
			ctx,
			deps.RepoRoot,
			"tools/adapter-repair.ts",
			fixturePath,
			"--id", capture.Provider,
			"--scenario", capture.Scenario,
			"--rule-kind", ruleKind,
		)
		if runErr != nil {
			candidateStatus = fmt.Sprintf("Patch generation stopped because selector synthesis failed: %v\n%s", runErr, output)
		} else if decodeErr := json.Unmarshal([]byte(output), &candidates); decodeErr != nil {
			candidateStatus = fmt.Sprintf("Patch generation stopped because selector synthesis returned invalid JSON: %v", decodeErr)
		} else if candidates.Provider != capture.Provider || candidates.Scenario != capture.Scenario || candidates.RuleKind != ruleKind {
			candidateStatus = "Patch generation stopped because selector synthesis returned mismatched provider, scenario, or rule-kind metadata."
		} else {
			generationSupported = true
		}
	}

	var topClassifier, top *adapterRepairCandidate
	if generationSupported {
		for i := range candidates.Candidates {
			candidate := &candidates.Candidates[i]
			if candidate.ClassifierVerified && topClassifier == nil {
				topClassifier = candidate
			}
			if candidate.ClassifierVerified && candidate.PlanComplete && candidate.ReplaceSelector != "" {
				top = candidate
				break
			}
		}
		candidateStatus = "Classifier verification only proves selector matching. Treat every candidate as a starting point. A maintainer must confirm the article file against live `%PDF` bytes before applying it."
		if top != nil {
			candidateStatus += fmt.Sprintf("\n\nTop candidate with a complete plan: `%s` (score %d).\n\nProven missing selector: `%s`.\n\nMatched node: `%s`", top.Selector, top.Score, top.ReplaceSelector, top.OuterHTML)
		} else if topClassifier != nil && topClassifier.ReplaceSelector == "" {
			candidateStatus += "\n\nThe capture proves no declared selector is missing. No types.ts.patch was emitted."
		} else if topClassifier != nil {
			candidateStatus += "\n\nAt least one candidate passed classifier matching, but none produced a complete non-assisted plan. No types.ts.patch was emitted."
		} else {
			candidateStatus += fmt.Sprintf("\n\nNo selector candidate passed classifier matching for the `%s` rule. No types.ts.patch was emitted.", ruleKind)
		}
	}

	testPatchWritten := false
	if generationSupported {
		adaptersTestPath := filepath.Join(deps.RepoRoot, "extension", "test", "adapters.test.ts")
		adaptersTestSource, readErr := os.ReadFile(adaptersTestPath)
		if readErr != nil {
			return adapterRepairResult{}, fmt.Errorf("read extension/test/adapters.test.ts: %w", readErr)
		}
		testCase := adapterRepairTestCase(capture.Provider, capture.Scenario, ruleKind)
		testPatch := appendUnifiedPatch("extension/test/adapters.test.ts", string(adaptersTestSource), testCase)
		testPatchPath := filepath.Join(workspace, "adapters.test.ts.patch")
		if err := os.WriteFile(testPatchPath, []byte(testPatch), 0o600); err != nil {
			return adapterRepairResult{}, fmt.Errorf("write adapters test patch: %w", err)
		}
		testPatchWritten = true
	}

	typesPatchWritten := false
	if top != nil && capture.IndependentEvidence && nextRevision != "unknown" {
		typesPatch, patchErr := adapterTypesUnifiedPatch(
			string(typesSource),
			capture.Provider,
			ruleKind,
			top.ReplaceSelector,
			top.Selector,
			currentVersion,
			nextRevision,
			versionLine,
		)
		if patchErr != nil {
			return adapterRepairResult{}, fmt.Errorf("generate types patch: %w", patchErr)
		}
		// #nosec G703 -- workspace is the repo scratch root and its variable
		// segments passed adapterRepairSegmentRE; the filename is a literal.
		if err := os.WriteFile(filepath.Join(workspace, "types.ts.patch"), []byte(typesPatch), 0o600); err != nil {
			return adapterRepairResult{}, fmt.Errorf("write types patch: %w", err)
		}
		typesPatchWritten = true
	} else if top != nil && !capture.IndependentEvidence {
		candidateStatus += "\n\nThe complete candidate remains proposal-only. Independent evidence is absent, so the revision bump and types.ts.patch stay locked."
	} else if top != nil && nextRevision == "unknown" {
		candidateStatus += "\n\nThe adapter revision could not be advanced, so types.ts.patch was not emitted."
	}

	report := fmt.Sprintf(
		"# Adapter repair analysis\n\nProvider: `%s`\nScenario: `%s`\nEvidence: %s\nFixture SHA-256: `%s`\nCurrent adapter version: `%s`\nNext adapter revision: `%s`\nVersion source: %s\n\n## adapter-try output (against exact emitted fixture bytes)\n\n%s\n\n## selector candidates\n\n%s\n",
		capture.Provider,
		capture.Scenario,
		evidenceStatus,
		capture.SHA256,
		currentVersion,
		nextRevision,
		sourceStatus,
		analysis,
		candidateStatus,
	)
	// #nosec G703 -- reportPath shares the validated, repo-owned workspace above.
	if err := os.WriteFile(reportPath, []byte(report), 0o600); err != nil {
		return adapterRepairResult{}, fmt.Errorf("write report: %w", err)
	}

	workspaceRelative, relErr := filepath.Rel(deps.RepoRoot, workspace)
	if relErr != nil {
		return adapterRepairResult{}, fmt.Errorf("locate repair workspace: %w", relErr)
	}
	workspaceRelative = filepath.ToSlash(workspaceRelative)
	fixtureSource := workspaceRelative + "/" + filepath.ToSlash(finalFixtureRelative)
	apply := "# No applicable repair patch\n\nPatch generation stopped. Review `report.md` for the exact reason.\n"
	if testPatchWritten {
		patches := workspaceRelative + "/adapters.test.ts.patch"
		if typesPatchWritten {
			patches += " " + workspaceRelative + "/types.ts.patch"
		}
		reviewTarget := "the generated test diff"
		if typesPatchWritten {
			reviewTarget = "both generated diffs"
		}
		apply = fmt.Sprintf(
			"# Apply this reviewed repair\n\nClassifier verification does not prove that an article candidate returns PDF bytes. Confirm the candidate against live `%%PDF` bytes first.\n\nRun from the repository root after reviewing `report.md` and %s.\n\n```sh\ngit apply %s\nmkdir -p extension/fixtures/%s\ncp %s extension/fixtures/%s/%s.html\n(cd extension && bun test test/adapters.test.ts)\n```\n",
			reviewTarget,
			patches,
			capture.Provider,
			fixtureSource,
			capture.Provider,
			capture.Scenario,
		)
	}
	if err := os.WriteFile(applyPath, []byte(apply), 0o600); err != nil {
		return adapterRepairResult{}, fmt.Errorf("write apply instructions: %w", err)
	}
	return adapterRepairResult{Workspace: workspace, Fixture: fixturePath, Report: reportPath, NextRevision: nextRevision, IndependentEvidence: capture.IndependentEvidence}, nil
}

func adapterRepairRuleKind(scenario string) (string, error) {
	switch scenario {
	case "success", "drift":
		return "article", nil
	case "login-return":
		return "login", nil
	case "terms":
		return "terms", nil
	case "no-entitlement":
		return "no_entitlement", nil
	case "wrong-work":
		return "wrong_work_check", nil
	default:
		return "", fmt.Errorf("scenario %q has no adapter repair rule kind", scenario)
	}
}

func adapterRepairTestCase(provider, scenario, expected string) string {
	identifier := strings.NewReplacer("-", "_", ".", "_").Replace(provider + "_" + scenario)
	var planLines string
	if expected == "article" {
		planLines = fmt.Sprintf(
			"    const evidence = spec.workEvidence;\n    if (evidence === undefined) throw new Error(%q);\n    const evidenceNode = page.querySelector(evidence.selector);\n    if (evidenceNode === null) throw new Error(%q);\n    let identity = evidence.attribute === undefined ? evidenceNode.textContent?.trim() ?? \"\" : evidenceNode.getAttribute(evidence.attribute)?.trim() ?? \"\";\n    if (evidence.pattern !== undefined) identity = new RegExp(evidence.pattern).exec(identity)?.[1]?.trim() ?? \"\";\n    expect(identity).not.toBe(\"\");\n    const expectedWork = evidence.kind === \"doi\" ? { doi: identity } : { title: identity };\n    const planned = planExecution(page, spec, expectedWork, {});\n",
			"generated article repair requires packaged work evidence",
			"generated article repair fixture lacks packaged identity evidence",
		)
	} else {
		planLines = "    const planned = planExecution(page, spec, {}, {});\n"
	}
	return fmt.Sprintf(
		"\nconst repair_%s = loadFixture(%q, %q);\ntest.skipIf(repair_%s === null)(\n  %q,\n  () => {\n    const page = repair_%s as Document;\n    const spec = adapters.find((candidate) => candidate.id === %q) as AdapterSpec;\n%s    expect(\"assisted\" in planned).toBe(false);\n    expect(planned.verdict.kind).toBe(%q);\n  },\n);\n",
		identifier,
		provider,
		scenario,
		identifier,
		fmt.Sprintf("generated %s %s fixture produces a complete %s plan", provider, scenario, expected),
		identifier,
		provider,
		planLines,
		expected,
	)
}

func appendUnifiedPatch(path, source, addition string) string {
	lines := strings.Split(strings.TrimSuffix(source, "\n"), "\n")
	start := len(lines) - 3
	if start < 0 {
		start = 0
	}
	additionLines := strings.Split(strings.TrimPrefix(strings.TrimSuffix(addition, "\n"), "\n"), "\n")
	var patch strings.Builder
	fmt.Fprintf(&patch, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", path, path, path, path)
	fmt.Fprintf(&patch, "@@ -%d,%d +%d,%d @@\n", start+1, len(lines)-start, start+1, len(lines)-start+len(additionLines))
	for _, line := range lines[start:] {
		fmt.Fprintf(&patch, " %s\n", line)
	}
	for _, line := range additionLines {
		fmt.Fprintf(&patch, "+%s\n", line)
	}
	return patch.String()
}

func adapterTypesUnifiedPatch(source, provider, ruleKind, oldSelector, newSelector, currentVersion, nextRevision string, versionLine int) (string, error) {
	if versionLine <= 0 {
		return "", errors.New("adapter version line is unavailable")
	}
	if oldSelector == "" {
		return "", errors.New("candidate did not identify the failing selector")
	}
	lines := strings.Split(strings.TrimSuffix(source, "\n"), "\n")
	versionIndex := versionLine - 1
	if versionIndex >= len(lines) || !strings.Contains(lines[versionIndex], strconv.Quote(currentVersion)) {
		return "", errors.New("adapter version line does not contain the current version")
	}

	adapterStart, adapterEnd := -1, len(lines)
	for i, line := range lines {
		if strings.Contains(line, `id: "`+provider+`"`) {
			adapterStart = i
			continue
		}
		if adapterStart >= 0 && strings.Contains(line, `id: "`) {
			adapterEnd = i
			break
		}
	}
	if adapterStart < 0 {
		return "", fmt.Errorf("adapter %q is unavailable", provider)
	}

	ruleStart, ruleEnd := -1, adapterEnd
	for i := adapterStart; i < adapterEnd; i++ {
		if strings.Contains(lines[i], `kind: "`+ruleKind+`"`) {
			ruleStart = i
			continue
		}
		if ruleStart >= 0 && strings.Contains(lines[i], `kind: "`) {
			ruleEnd = i
			break
		}
	}
	if ruleStart < 0 {
		return "", fmt.Errorf("adapter %q has no %s rule", provider, ruleKind)
	}

	oldQuoted, newQuoted := strconv.Quote(oldSelector), strconv.Quote(newSelector)
	foundInRule := false
	for i := ruleStart; i < ruleEnd; i++ {
		if strings.Contains(lines[i], oldQuoted) {
			foundInRule = true
			break
		}
	}
	if !foundInRule {
		return "", fmt.Errorf("the %s rule does not contain selector %q", ruleKind, oldSelector)
	}

	changes := map[int]string{
		versionIndex: strings.Replace(lines[versionIndex], strconv.Quote(currentVersion), strconv.Quote(nextRevision), 1),
	}
	for i := adapterStart; i < adapterEnd; i++ {
		if strings.Contains(lines[i], oldQuoted) {
			changes[i] = strings.ReplaceAll(lines[i], oldQuoted, newQuoted)
		}
	}
	return replacementUnifiedPatch("extension/src/adapters/types.ts", lines, changes), nil
}

func replacementUnifiedPatch(path string, lines []string, changes map[int]string) string {
	indexes := make([]int, 0, len(changes))
	for index := range changes {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	type patchRange struct {
		start int
		end   int
	}
	ranges := make([]patchRange, 0, len(indexes))
	for _, index := range indexes {
		start, end := index-3, index+3
		if start < 0 {
			start = 0
		}
		if end >= len(lines) {
			end = len(lines) - 1
		}
		if len(ranges) > 0 && start <= ranges[len(ranges)-1].end+1 {
			ranges[len(ranges)-1].end = end
		} else {
			ranges = append(ranges, patchRange{start: start, end: end})
		}
	}

	var patch strings.Builder
	fmt.Fprintf(&patch, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", path, path, path, path)
	for _, current := range ranges {
		count := current.end - current.start + 1
		fmt.Fprintf(&patch, "@@ -%d,%d +%d,%d @@\n", current.start+1, count, current.start+1, count)
		for i := current.start; i <= current.end; i++ {
			replacement, changed := changes[i]
			if changed {
				fmt.Fprintf(&patch, "-%s\n+%s\n", lines[i], replacement)
			} else {
				fmt.Fprintf(&patch, " %s\n", lines[i])
			}
		}
	}
	return patch.String()
}

func parseAdapterVersion(source, provider string) (string, error) {
	pattern := regexp.MustCompile(`(?ms)\bid:\s*"` + regexp.QuoteMeta(provider) + `"\s*,.*?\bversion:\s*"([^"]+)"`)
	match := pattern.FindStringSubmatch(source)
	if len(match) != 2 {
		return "", fmt.Errorf("adapter %q is not present in extension/src/adapters/types.ts", provider)
	}
	if !adapterVersionRE.MatchString(match[1]) {
		return "", fmt.Errorf("adapter %q has invalid version %q", provider, match[1])
	}
	return match[1], nil
}

func adapterVersionLine(source, provider string) int {
	lines := strings.Split(source, "\n")
	seen := false
	for i, line := range lines {
		if strings.Contains(line, `id: "`+provider+`"`) {
			seen = true
			if strings.Contains(line, "version:") {
				return i + 1
			}
			continue
		}
		if seen && strings.Contains(line, "version:") {
			return i + 1
		}
		if seen && strings.Contains(line, `id: "`) {
			return 0
		}
	}
	return 0
}

func nextAdapterRevision(version string) (string, error) {
	match := adapterVersionRE.FindStringSubmatch(version)
	if len(match) != 4 {
		return "", fmt.Errorf("adapter version %q is not semantic major.minor.patch", version)
	}
	prefix := ""
	trimmed := version
	if strings.HasPrefix(trimmed, "v") || strings.HasPrefix(trimmed, "V") {
		prefix, trimmed = trimmed[:1], trimmed[1:]
	}
	parts := strings.SplitN(trimmed, ".", 3)
	if len(parts) != 3 {
		return "", fmt.Errorf("adapter version %q is not semantic major.minor.patch", version)
	}
	patchText := parts[2]
	if dash := strings.IndexAny(patchText, "-+"); dash >= 0 {
		patchText = patchText[:dash]
	}
	patch, err := strconv.ParseUint(patchText, 10, 64)
	if err != nil || patch == ^uint64(0) {
		return "", fmt.Errorf("adapter version %q has an unincrementable patch", version)
	}
	return fmt.Sprintf("%s%s.%s.%d", prefix, parts[0], parts[1], patch+1), nil
}
