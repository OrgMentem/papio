// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	Path           string
	Provider       string
	Scenario       string
	Host           string
	Origin         string
	Captured       time.Time
	AdapterVersion string
}

type adapterRepairMetadata struct {
	Provider       string    `json:"provider,omitempty"`
	AdapterID      string    `json:"adapter_id,omitempty"`
	Scenario       string    `json:"scenario,omitempty"`
	Host           string    `json:"host,omitempty"`
	Origin         string    `json:"origin,omitempty"`
	Captured       time.Time `json:"captured,omitempty"`
	Timestamp      time.Time `json:"timestamp,omitempty"`
	AdapterVersion string    `json:"adapter_version,omitempty"`
}

type adapterRepairRunner interface {
	Run(context.Context, string, string, string) (string, error)
}

type execAdapterRepairRunner struct{}

func (execAdapterRepairRunner) Run(ctx context.Context, repoRoot, capturePath, provider string) (string, error) {
	command := exec.CommandContext(ctx, "bun", "run", "--cwd", "extension", "tools/adapter-try.ts", capturePath, "--id", provider)
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
	Workspace    string `json:"workspace"`
	Fixture      string `json:"fixture"`
	Report       string `json:"report"`
	NextRevision string `json:"next_revision"`
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
			_, err = fmt.Fprintf(opt.out, "%s\nReview report.md and apply.md; copy the reviewed fixture and add the focused adapter test before editing source.\n", result.Workspace)
			return err
		},
	}
	command.Flags().StringVar(&provider, "provider", "", "provider adapter id (required when capture metadata is absent)")
	command.Flags().StringVar(&scenario, "scenario", "", "fixture scenario (required when capture metadata is absent)")
	return command
}

func resolveAdapterRepairCapture(ctx context.Context, opt *options, input, providerFlag, scenarioFlag string) (adapterRepairCapture, error) {
	if info, err := os.Stat(input); err == nil {
		if !info.Mode().IsRegular() {
			return adapterRepairCapture{}, fmt.Errorf("capture path %q is not a regular file", input)
		}
		return loadAdapterRepairCapture(input, providerFlag, scenarioFlag)
	} else if filepath.IsAbs(input) {
		return adapterRepairCapture{}, fmt.Errorf("capture %q: %w", input, err)
	}

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
		return adapterRepairCapture{}, fmt.Errorf("no stored capture matches %q", input)
	}
	if len(matches) > 1 {
		return adapterRepairCapture{}, fmt.Errorf("capture prefix %q is ambiguous (%d matches)", input, len(matches))
	}
	row := matches[0]
	if row.Path == "" {
		return adapterRepairCapture{}, errors.New("stored capture has no path")
	}
	provider := strings.TrimSpace(providerFlag)
	if provider == "" {
		provider = strings.TrimSpace(row.AdapterID)
	}
	scenario := strings.TrimSpace(scenarioFlag)
	if scenario == "" {
		scenario = strings.TrimSpace(row.Scenario)
	}
	return finishAdapterRepairCapture(row.Path, adapterRepairCapture{
		Path:           row.Path,
		Provider:       provider,
		Scenario:       scenario,
		Host:           row.Host,
		Captured:       row.Timestamp,
		AdapterVersion: row.AdapterVersion,
	})
}

func loadAdapterRepairCapture(path, providerFlag, scenarioFlag string) (adapterRepairCapture, error) {
	capture := adapterRepairCapture{Path: path}
	if info, err := os.Stat(path); err == nil {
		capture.Captured = info.ModTime().UTC()
	}
	capture.Host = filepath.Base(filepath.Dir(path))
	for _, metadataPath := range []string{
		strings.TrimSuffix(path, filepath.Ext(path)) + ".json",
		path + ".observed.json",
		filepath.Join(filepath.Dir(path), "observed.json"),
	} {
		data, err := os.ReadFile(metadataPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return adapterRepairCapture{}, fmt.Errorf("read capture metadata %q: %w", metadataPath, err)
		}
		var metadata adapterRepairMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return adapterRepairCapture{}, fmt.Errorf("decode capture metadata %q: %w", metadataPath, err)
		}
		if metadata.Provider != "" {
			capture.Provider = metadata.Provider
		}
		if metadata.AdapterID != "" {
			capture.Provider = metadata.AdapterID
		}
		if metadata.Scenario != "" {
			capture.Scenario = metadata.Scenario
		}
		if metadata.Host != "" {
			capture.Host = metadata.Host
		}
		if metadata.Origin != "" {
			capture.Origin = metadata.Origin
		}
		if !metadata.Captured.IsZero() {
			capture.Captured = metadata.Captured.UTC()
		}
		if !metadata.Timestamp.IsZero() {
			capture.Captured = metadata.Timestamp.UTC()
		}
		if metadata.AdapterVersion != "" {
			capture.AdapterVersion = metadata.AdapterVersion
		}
	}
	if providerFlag != "" {
		capture.Provider = providerFlag
	}
	if scenarioFlag != "" {
		capture.Scenario = scenarioFlag
	}
	return finishAdapterRepairCapture(path, capture)
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

func scaffoldAdapterRepair(ctx context.Context, capture adapterRepairCapture, deps adapterRepairDeps) (adapterRepairResult, error) {
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
	fixture := rewrapAdapterFixture(string(raw), capture.Provider, capture.Scenario, capture.Origin, capture.Captured)
	fixturePath := filepath.Join(workspace, "fixture.html")
	reportPath := filepath.Join(workspace, "report.md")
	applyPath := filepath.Join(workspace, "apply.md")
	if err := os.WriteFile(fixturePath, []byte(fixture), 0o600); err != nil {
		return adapterRepairResult{}, fmt.Errorf("write fixture: %w", err)
	}

	currentVersion, versionLine, sourceStatus := "unavailable", 0, "extension workspace unavailable"
	typesPath := filepath.Join(deps.RepoRoot, "extension", "src", "adapters", "types.ts")
	if source, readErr := os.ReadFile(typesPath); readErr == nil {
		if parsed, parseErr := parseAdapterVersion(string(source), capture.Provider); parseErr == nil {
			currentVersion = parsed
			versionLine = adapterVersionLine(string(source), capture.Provider)
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
	if next, nextErr := nextAdapterRevision(currentVersion); nextErr == nil {
		nextRevision = next
	}

	analysis := ""
	if _, err := os.Stat(filepath.Join(deps.RepoRoot, "extension", "tools", "adapter-try.ts")); err != nil {
		analysis = "adapter-try analysis skipped: extension/tools/adapter-try.ts is unavailable"
	} else {
		output, runErr := deps.Run.Run(ctx, deps.RepoRoot, capture.Path, capture.Provider)
		analysis = output
		if runErr != nil {
			analysis += fmt.Sprintf("\n(adapter-try analysis skipped or failed: %v)\n", runErr)
		}
	}
	report := fmt.Sprintf("# Adapter repair analysis\n\nProvider: `%s`\nScenario: `%s`\nCurrent adapter version: `%s`\nNext adapter revision: `%s`\nVersion source: %s\n\n## adapter-try output\n\n%s", capture.Provider, capture.Scenario, currentVersion, nextRevision, sourceStatus, analysis)
	if err := os.WriteFile(reportPath, []byte(report), 0o600); err != nil {
		return adapterRepairResult{}, fmt.Errorf("write report: %w", err)
	}
	typesInstruction := "Locate the adapter entry in extension/src/adapters/types.ts"
	if versionLine > 0 {
		typesInstruction = fmt.Sprintf("Edit extension/src/adapters/types.ts line %d (the `%s` adapter version line)", versionLine, capture.Provider)
	}
	apply := fmt.Sprintf("# Apply this reviewed scaffold\n\nThis workspace is proposal-only; papio did not modify extension source. Review `report.md` and the captured page before applying anything.\n\n1. Copy `fixture.html` to `extension/fixtures/%s/%s.html`.\n2. Add a focused case to `extension/test/adapters.test.ts`, following the existing fixture-backed adapter test pattern for `loadFixture` and the expected page verdict.\n3. %s: change the current adapter version `%s` to the exact next revision `%s`.\n4. Re-run the focused adapter test and review the resulting source diff before opening a PR.\n\nGenerated fixture path: `extension/fixtures/%s/%s.html`.\n", capture.Provider, capture.Scenario, typesInstruction, currentVersion, nextRevision, capture.Provider, capture.Scenario)
	if err := os.WriteFile(applyPath, []byte(apply), 0o600); err != nil {
		return adapterRepairResult{}, fmt.Errorf("write apply instructions: %w", err)
	}
	return adapterRepairResult{Workspace: workspace, Fixture: fixturePath, Report: reportPath, NextRevision: nextRevision}, nil
}
func rewrapAdapterFixture(raw, provider, scenario, origin string, captured time.Time) string {
	body := raw
	if first, rest, ok := strings.Cut(raw, "\n"); ok && adapterFixtureHeaderRE.MatchString(strings.TrimSuffix(first, "\r")) {
		body = rest
	}
	if captured.IsZero() {
		captured = time.Now().UTC()
	}
	header := fmt.Sprintf("<!-- papio-fixture provider=\"%s\" scenario=\"%s\" origin=\"%s\" captured=\"%s\" -->", provider, scenario, normalizeRepairOrigin(origin, "provider.example"), captured.UTC().Format(time.RFC3339Nano))
	return header + "\n" + body
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
