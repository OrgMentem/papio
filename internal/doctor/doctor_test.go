// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/discovery"
	"papio/internal/ownership"
	"papio/internal/pdf"
	"papio/internal/store"
	"papio/internal/update"
	"papio/internal/zotio"
)

func executable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "papio")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunReadyProfilePassesWithoutLeakingSecrets(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = data
	cfg.Email = "researcher@example.test"
	cfg.Sources[config.SourceOpenAlex] = config.Source{Enabled: true, APIKey: "SUPER_SECRET_KEY"}
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg.Path, []byte("access_mode='conservative'"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := executable(t)
	report := Run(ctx, cfg, db, pdf.Capability{
		PDFCPU: true, PDFInfo: tool, PDFToText: tool, PDFToPPM: tool, Tesseract: tool,
	}, tool, nil)
	if !report.OK {
		t.Fatalf("ready report failed: %+v", report)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "SUPER_SECRET_KEY") {
		t.Fatalf("doctor leaked credential: %s", encoded)
	}
	var dbPass bool
	for _, c := range report.Checks {
		if c.Name == "database" && c.Status == Pass && strings.Contains(c.Detail, "schema version 18") {
			dbPass = true
		}
	}
	if !dbPass {
		t.Fatalf("database migration check missing: %+v", report.Checks)
	}
}

// An acquisition nobody exported is the one thing a consumer structurally
// cannot detect for itself: a job is stranded precisely when the key naming it
// stops being derivable, so the orphan is the job the consumer can no longer
// ask about. The grace period keeps a freshly acquired job from reading as
// abandoned.
func TestRunReportsAcquisitionsNobodyEverCollected(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	seed := func(id, settled string, exported bool) {
		t.Helper()
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO work_requests (id, created_at, title) VALUES (?, ?, 'Example')`,
			"wr_"+id, settled); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at)
			VALUES (?, ?, 'ready', '{}', ?, ?)`, id, "wr_"+id, settled, settled); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO artifacts (sha256, size_bytes, mime, path, created_at)
			VALUES (?, 1, 'application/pdf', ?, ?)`, id+"sha", "/tmp/"+id, settled); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO job_artifacts (job_id, artifact_sha256, role, created_at)
			VALUES (?, ?, 'main', ?)`, id, id+"sha", settled); err != nil {
			t.Fatal(err)
		}
		if exported {
			if _, err := db.DB().ExecContext(ctx, `
				INSERT INTO exports (job_id, kind, idempotency_key, created_at)
				VALUES (?, 'bundle', ?, ?)`, id, "idem_"+id, settled); err != nil {
				t.Fatal(err)
			}
		}
	}
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
	fresh := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	seed("job_stranded", old, false)
	seed("job_collected", old, true)
	seed("job_recent", fresh, false)

	detail := uncollectedDetail(t, ctx, db, Warn)
	if !strings.Contains(detail, "1 acquired full texts") {
		t.Fatalf("detail = %q; want exactly the one stranded job — an exported job and one inside the grace period are not orphans", detail)
	}
}

func TestRunPassesWhenEveryAcquisitionWasCollected(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	uncollectedDetail(t, ctx, db, Pass)
}

func uncollectedDetail(t *testing.T, ctx context.Context, db *store.Store, want string) string {
	t.Helper()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	tool := executable(t)
	report := Run(ctx, cfg, db, pdf.Capability{
		PDFCPU: true, PDFInfo: tool, PDFToText: tool, PDFToPPM: tool, Tesseract: tool,
	}, tool, nil)
	for _, c := range report.Checks {
		if c.Name == "uncollected_acquisitions" {
			if c.Status != want {
				t.Fatalf("status = %q, want %q (detail %q)", c.Status, want, c.Detail)
			}
			return c.Detail
		}
	}
	t.Fatalf("uncollected_acquisitions check missing: %+v", report.Checks)
	return ""
}

func TestRunReportsMissingModeCredentialsToolsAndUnsafeConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Sources[config.SourceOpenAlex] = config.Source{Enabled: true}
	cfg.DataDir = t.TempDir()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg.Path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := Run(context.Background(), cfg, nil, pdf.Capability{}, "", nil)
	if report.OK {
		t.Fatalf("unsafe profile passed: %+v", report)
	}
	wantFailures := map[string]bool{
		"access_mode": false, "pdftotext": false, "ocr": false,
		"pdf_worker": false, "source_unpaywall": false, "source_openalex": false,
		"config_permissions": false,
	}
	for _, c := range report.Checks {
		if c.Status == Fail {
			if _, ok := wantFailures[c.Name]; ok {
				wantFailures[c.Name] = true
			}
		}
	}
	for name, found := range wantFailures {
		if !found {
			t.Errorf("missing failure check %s: %+v", name, report.Checks)
		}
	}
}

func TestRunWarnsWhenOCRExplicitlyDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.Email = "a@b.test"
	cfg.Sources[config.SourceOpenAlex] = config.Source{Enabled: false}
	cfg.PDF.OCREnabled = false
	tool := executable(t)
	report := Run(context.Background(), cfg, nil, pdf.Capability{PDFToText: tool}, tool, nil)
	var warned bool
	for _, c := range report.Checks {
		if c.Name == "ocr" && c.Status == Warn {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("disabled OCR warning missing: %+v", report.Checks)
	}
}

func TestRunWarnsOnRawAlmaResolverBase(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.Email = "a@b.test"
	cfg.Browser.OpenURLBase = "https://une.alma.exlibrisgroup.com/view/uresolver/61UNE_INST/openurl"
	cfg.Browser.Resolvers = map[string]config.Institution{
		"primo": {OpenURLBase: "https://une.primo.exlibrisgroup.com/nde/openurl?vid=61UNE_INST:61UNE_NDE"},
		"alma":  {OpenURLBase: "https://x.alma.exlibrisgroup.com/view/uresolver/61X_INST/openurl?svc_dat=viewit"},
	}
	tool := executable(t)
	report := Run(context.Background(), cfg, nil, pdf.Capability{PDFToText: tool}, tool, nil)
	byName := map[string]Check{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}
	if got := byName["resolver_base"]; got.Status != Warn {
		t.Fatalf("default resolver_base = %+v; want warn", got)
	}
	if got := byName["resolver_base:alma"]; got.Status != Warn {
		t.Fatalf("resolver_base:alma = %+v; want warn", got)
	}
	if _, ok := byName["resolver_base:primo"]; ok {
		t.Fatalf("primo profile should not warn: %+v", report.Checks)
	}
}

func TestRunPassesOnPrimoResolverBase(t *testing.T) {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	cfg.Email = "a@b.test"
	cfg.Browser.OpenURLBase = "https://une.primo.exlibrisgroup.com/nde/openurl?vid=61UNE_INST:61UNE_NDE"
	tool := executable(t)
	report := Run(context.Background(), cfg, nil, pdf.Capability{PDFToText: tool}, tool, nil)
	for _, c := range report.Checks {
		if c.Name == "resolver_base" && c.Status != Pass {
			t.Fatalf("resolver_base = %+v; want pass", c)
		}
	}
}

func TestRunIntegrationReportsVersionSkewAndSkipsUnconfiguredManifests(t *testing.T) {
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	report := RunIntegration(context.Background(), IntegrationDependencies{
		CLIVersion: "cli-version",
		LoadConfig: func() (config.Config, error) {
			return cfg, nil
		},
		DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
			return DaemonStatus{Status: "ok", Version: "daemon-version"}, nil
		},
		ManifestDir: func(config.Config) (string, error) { return t.TempDir(), nil },
		FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return &zotio.PreflightResult{Version: "1.2.3"}, nil
		},
	})
	if !report.OK {
		t.Fatalf("integration report failed: %+v", report)
	}
	if got := report.Checks[1]; got.Name != "daemon" || got.Status != Warn || !strings.Contains(got.Detail, "daemon-version") {
		t.Fatalf("daemon check = %#v", got)
	}
	if got := report.Checks[3]; got.Name != "native host (Chrome)" || got.Status != Skip {
		t.Fatalf("Chrome manifest check = %#v", got)
	}
}

func TestRunIntegrationReportsConfiguredLibrarySourcesWithoutProbe(t *testing.T) {
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	cfg.Library.Sources = []config.LibrarySource{{
		Name: "papis", Kind: config.LibraryKindFile, Path: "/library.bib", Claim: config.LibraryClaimPDFPresent,
	}}
	report := RunIntegration(context.Background(), IntegrationDependencies{
		CLIVersion: "v1",
		LoadConfig: func() (config.Config, error) { return cfg, nil },
		DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
			return DaemonStatus{Status: "ok", Version: "v1"}, nil
		},
		ManifestDir: func(config.Config) (string, error) { return t.TempDir(), nil },
		FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return &zotio.PreflightResult{Version: "1.2.3"}, nil
		},
	})
	var library Check
	for _, check := range report.Checks {
		if check.Name == "library" {
			library = check
			break
		}
	}
	if library.Status != Skip {
		t.Fatalf("library check = %#v, want Skip", library)
	}
	if library.Detail != "configured library sources were not probed because this doctor invocation has no library probe" {
		t.Fatalf("library check detail = %q", library.Detail)
	}
	if library.Remediation != "run 'papio doctor' through the papio CLI; if it persists, reinstall papio" {
		t.Fatalf("library remediation = %q", library.Remediation)
	}
}

func TestRunIntegrationLibrarySourceChecks(t *testing.T) {
	baseDeps := func(cfg config.Config, probe func(context.Context, config.Config) ([]LibrarySourceStatus, error)) IntegrationDependencies {
		return IntegrationDependencies{
			CLIVersion: "v1",
			LoadConfig: func() (config.Config, error) { return cfg, nil },
			DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
				return DaemonStatus{Status: "ok", Version: "v1"}, nil
			},
			ManifestDir:    func(config.Config) (string, error) { return t.TempDir(), nil },
			FirefoxDir:     func(config.Config) (string, error) { return t.TempDir(), nil },
			ReadFile:       os.ReadFile,
			LibrarySources: probe,
			ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
				return &zotio.PreflightResult{Version: "1.2.3"}, nil
			},
		}
	}
	libraryConfig := func() config.Config {
		cfg := config.Default()
		cfg.Path = filepath.Join(t.TempDir(), "config.toml")
		cfg.Library.Sources = []config.LibrarySource{{
			Name: "owned-pdfs", Kind: config.LibraryKindFile, Path: "/library.bib", Claim: config.LibraryClaimPDFPresent,
		}}
		return cfg
	}
	check := func(t *testing.T, report Report, name string) Check {
		t.Helper()
		for _, check := range report.Checks {
			if check.Name == name {
				return check
			}
		}
		t.Fatalf("missing %s check in %#v", name, report.Checks)
		return Check{}
	}

	t.Run("skip without sources", func(t *testing.T) {
		cfg := libraryConfig()
		cfg.Library.Sources = nil
		report := RunIntegration(context.Background(), baseDeps(cfg, func(context.Context, config.Config) ([]LibrarySourceStatus, error) {
			t.Fatal("library probe ran without configured sources")
			return nil, nil
		}))
		got := check(t, report, "library")
		if got.Status != Skip || !strings.Contains(got.Detail, "not configured (optional") {
			t.Fatalf("library check = %#v, want optional Skip", got)
		}
	})

	t.Run("pass after readable source", func(t *testing.T) {
		cfg := libraryConfig()
		report := RunIntegration(context.Background(), baseDeps(cfg, func(context.Context, config.Config) ([]LibrarySourceStatus, error) {
			return []LibrarySourceStatus{{
				Name: "owned-pdfs", Complete: true, EntryCount: 2, LastSuccess: time.Now().Add(-time.Minute),
			}}, nil
		}))
		got := check(t, report, "library_source:owned-pdfs")
		if got.Status != Pass || !strings.Contains(got.Detail, "2 entries") || !strings.Contains(got.Detail, "read ") {
			t.Fatalf("library source check = %#v, want readable Pass", got)
		}
	})

	t.Run("fail after unreadable source", func(t *testing.T) {
		cfg := libraryConfig()
		report := RunIntegration(context.Background(), baseDeps(cfg, func(context.Context, config.Config) ([]LibrarySourceStatus, error) {
			return []LibrarySourceStatus{{
				Name: "owned-pdfs", FailureCode: ownership.FailureUnreadable,
			}}, nil
		}))
		got := check(t, report, "library_source:owned-pdfs")
		if got.Status != Fail || !strings.Contains(got.Detail, "missing or unreadable") || !strings.Contains(got.Remediation, "check the path and format") {
			t.Fatalf("library source check = %#v, want actionable Fail", got)
		}
	})
}

func TestRunIntegrationFailsOnDanglingHostExecutable(t *testing.T) {
	const extID = "abcdefghijklmnopabcdefghijklmnop"
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	cfg.Browser.ExtensionID = extID
	manifestDir := t.TempDir()
	manifest := `{"name":"com.orgmentem.papio","path":"/gone/papio-native-host","type":"stdio",` +
		`"allowed_origins":["chrome-extension://` + extID + `/"]}`
	if err := os.WriteFile(filepath.Join(manifestDir, "com.orgmentem.papio.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	report := RunIntegration(context.Background(), IntegrationDependencies{
		CLIVersion: "v1",
		LoadConfig: func() (config.Config, error) { return cfg, nil },
		DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
			return DaemonStatus{Status: "ok", Version: "v1"}, nil
		},
		ManifestDir: func(config.Config) (string, error) { return manifestDir, nil },
		FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
		ReadFile:    os.ReadFile,
		// The host symlink points at a binary a brew upgrade removed.
		HostExecutableResolves: func(string) bool { return false },
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return &zotio.PreflightResult{Version: "1.2.3"}, nil
		},
	})
	var chrome Check
	for _, c := range report.Checks {
		if c.Name == "native host (Chrome)" {
			chrome = c
		}
	}
	if chrome.Status != Fail || !strings.Contains(chrome.Detail, "dangling") {
		t.Fatalf("Chrome host check = %#v, want Fail mentioning dangling", chrome)
	}
	if chrome.Remediation != "papio native-host install" {
		t.Fatalf("remediation = %q, want papio native-host install", chrome.Remediation)
	}
	if report.OK {
		t.Fatalf("report must fail when the host executable is missing: %+v", report)
	}
}

func TestRunIntegrationUpdates(t *testing.T) {
	baseConfig := func() config.Config {
		cfg := config.Default()
		cfg.Path = filepath.Join(t.TempDir(), "config.toml")
		cfg.DataDir = t.TempDir()
		return cfg
	}
	depsFor := func(cfg config.Config) IntegrationDependencies {
		return IntegrationDependencies{
			CLIVersion: "1.2.3",
			LoadConfig: func() (config.Config, error) { return cfg, nil },
			DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
				return DaemonStatus{Status: "ok", Version: "1.2.3"}, nil
			},
			ManifestDir: func(config.Config) (string, error) { return t.TempDir(), nil },
			FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
			ReadFile:    os.ReadFile,
			ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
				return &zotio.PreflightResult{Version: "1.2.3"}, nil
			},
			CheckUpdates: func(context.Context, config.Config) *update.Info {
				return &update.Info{LatestVersion: "1.2.3", URL: "https://example.test/papio"}
			},
			CheckZotioUpdates: func(context.Context, config.Config) *update.Info {
				return &update.Info{LatestVersion: "1.2.3", URL: "https://example.test/zotio"}
			},
		}
	}
	find := func(report Report, name string) Check {
		for _, check := range report.Checks {
			if check.Name == name {
				return check
			}
		}
		t.Fatalf("%s check missing: %+v", name, report.Checks)
		return Check{}
	}

	t.Run("disabled", func(t *testing.T) {
		cfg := baseConfig()
		deps := depsFor(cfg)
		deps.CheckUpdates = func(context.Context, config.Config) *update.Info {
			t.Fatal("disabled papio update check was invoked")
			return nil
		}
		deps.CheckZotioUpdates = func(context.Context, config.Config) *update.Info {
			t.Fatal("disabled zotio update check was invoked")
			return nil
		}
		report := RunIntegration(context.Background(), deps)
		for _, name := range []string{"updates (papio)", "updates (zotio)"} {
			got := find(report, name)
			if got.Status != Skip || got.Detail != "update check disabled ([updates] check = false)" {
				t.Fatalf("%s check = %#v", name, got)
			}
		}
	})

	t.Run("current", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Updates.Check = true
		report := RunIntegration(context.Background(), depsFor(cfg))
		for _, name := range []string{"updates (papio)", "updates (zotio)"} {
			got := find(report, name)
			if got.Status != Pass {
				t.Fatalf("%s check = %#v", name, got)
			}
		}
	})

	t.Run("zotio behind", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Updates.Check = true
		cfg.Zotio.Executable = "/opt/homebrew/bin/zotio"
		deps := depsFor(cfg)
		deps.CheckZotioUpdates = func(context.Context, config.Config) *update.Info {
			return &update.Info{LatestVersion: "1.2.4", URL: "https://example.test/zotio"}
		}
		got := find(RunIntegration(context.Background(), deps), "updates (zotio)")
		if got.Status != Warn || got.Detail != "zotio 1.2.4 available (you have 1.2.3)" || got.Remediation != "brew upgrade zotio" {
			t.Fatalf("zotio update check = %#v", got)
		}
	})

	t.Run("zotio preflight failed", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Updates.Check = true
		deps := depsFor(cfg)
		deps.ZotioPreflight = func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			return nil, errors.New("zotio not found")
		}
		deps.CheckZotioUpdates = func(context.Context, config.Config) *update.Info {
			t.Fatal("zotio update check ran despite failed preflight")
			return nil
		}
		got := find(RunIntegration(context.Background(), deps), "updates (zotio)")
		if got.Status != Skip || got.Detail != "skipped: zotio preflight failed" {
			t.Fatalf("zotio update check = %#v", got)
		}
	})
}

func TestRunIntegrationSkipsZotioWhenUnconfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Path = filepath.Join(t.TempDir(), "config.toml")
	cfg.DataDir = t.TempDir()
	cfg.Zotio.Executable = ""
	cfg.Updates.Check = true
	deps := IntegrationDependencies{
		CLIVersion: "1.2.3",
		LoadConfig: func() (config.Config, error) { return cfg, nil },
		DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
			return DaemonStatus{Status: "ok", Version: "1.2.3"}, nil
		},
		ManifestDir: func(config.Config) (string, error) { return t.TempDir(), nil },
		FirefoxDir:  func(config.Config) (string, error) { return t.TempDir(), nil },
		ReadFile:    os.ReadFile,
		ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
			t.Fatal("zotio preflight ran despite empty executable")
			return nil, nil
		},
		CheckUpdates: func(context.Context, config.Config) *update.Info {
			return &update.Info{LatestVersion: "1.2.3", URL: "https://example.test/papio"}
		},
		CheckZotioUpdates: func(context.Context, config.Config) *update.Info {
			t.Fatal("zotio update check ran despite empty executable")
			return nil
		},
	}
	report := RunIntegration(context.Background(), deps)
	var zotioCheck, zotioUpdates Check
	for _, check := range report.Checks {
		switch check.Name {
		case "zotio":
			zotioCheck = check
		case "updates (zotio)":
			zotioUpdates = check
		}
	}
	if zotioCheck.Status != Skip || !strings.Contains(zotioCheck.Detail, "not configured") {
		t.Fatalf("zotio check = %#v, want Skip not-configured", zotioCheck)
	}
	if !report.OK {
		t.Fatalf("unconfigured zotio must not fail doctor: %+v", report)
	}
	if zotioUpdates.Status != Skip || zotioUpdates.Detail != "skipped: zotio is not configured" {
		t.Fatalf("zotio updates check = %#v", zotioUpdates)
	}
}

// fakeDiscoveryHealth is a discovery.Source that also implements
// discovery.BackendHealth, so the discovery check can be exercised without a
// real discovery.Multi and its network-backed backends.
type fakeDiscoveryHealth struct {
	failures []discovery.BackendFailure
}

func (fakeDiscoveryHealth) Name() string { return "fake" }

func (fakeDiscoveryHealth) Search(context.Context, discovery.SearchParams) ([]discovery.DiscoveredWork, error) {
	return nil, nil
}

func (f fakeDiscoveryHealth) LastFailures() []discovery.BackendFailure { return f.failures }

// fakeDiscoverySourceNoHealth implements discovery.Source only, exercising
// the check's Skip path for a source that cannot report backend health.
type fakeDiscoverySourceNoHealth struct{}

func (fakeDiscoverySourceNoHealth) Name() string { return "fake" }

func (fakeDiscoverySourceNoHealth) Search(context.Context, discovery.SearchParams) ([]discovery.DiscoveredWork, error) {
	return nil, nil
}

func TestRunReportsDiscoveryBackendFailure(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	source := fakeDiscoveryHealth{failures: []discovery.BackendFailure{
		{Source: "semanticscholar", Message: "GET https://api.semanticscholar.org/...: connection refused"},
	}}
	report := Run(context.Background(), cfg, nil, pdf.Capability{}, "", source)
	var got Check
	for _, c := range report.Checks {
		if c.Name == "discovery" {
			got = c
		}
	}
	if got.Status != Warn {
		t.Fatalf("discovery check = %#v, want Warn", got)
	}
	if !strings.Contains(got.Detail, "semanticscholar") || !strings.Contains(got.Detail, "connection refused") {
		t.Fatalf("discovery detail = %q, want the failing backend name and its message", got.Detail)
	}
	if got.Remediation == "" {
		t.Fatalf("discovery check = %#v, want a remediation", got)
	}
	if report.OK {
		t.Fatalf("a Warn-only report must still be OK: %+v", report)
	}

	multiple := fakeDiscoveryHealth{failures: []discovery.BackendFailure{
		{Source: "openalex", Message: "timeout"},
		{Source: "semanticscholar", Message: "connection refused"},
	}}
	report = Run(context.Background(), cfg, nil, pdf.Capability{}, "", multiple)
	for _, c := range report.Checks {
		if c.Name == "discovery" {
			got = c
		}
	}
	if !strings.Contains(got.Detail, "openalex") || !strings.Contains(got.Detail, "semanticscholar") {
		t.Fatalf("discovery detail with two failures = %q, want both backend names", got.Detail)
	}
}

func TestRunReportsDiscoveryHealthyWhenNoFailures(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Discovery.Sources = []string{config.SourceOpenAlex, config.SourceSemanticScholar}
	report := Run(context.Background(), cfg, nil, pdf.Capability{}, "", fakeDiscoveryHealth{})
	var got Check
	for _, c := range report.Checks {
		if c.Name == "discovery" {
			got = c
		}
	}
	if got.Status != Pass || got.Detail != "2 discovery backend(s) configured; all healthy" {
		t.Fatalf("discovery check = %#v, want Pass naming 2 backends", got)
	}
}

func TestRunSkipsDiscoveryWhenHealthIsUnavailable(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()

	t.Run("no discovery source wired", func(t *testing.T) {
		report := Run(context.Background(), cfg, nil, pdf.Capability{}, "", nil)
		var got Check
		for _, c := range report.Checks {
			if c.Name == "discovery" {
				got = c
			}
		}
		if got.Status != Skip {
			t.Fatalf("discovery check = %#v, want Skip", got)
		}
	})

	t.Run("source cannot report health", func(t *testing.T) {
		report := Run(context.Background(), cfg, nil, pdf.Capability{}, "", fakeDiscoverySourceNoHealth{})
		var got Check
		for _, c := range report.Checks {
			if c.Name == "discovery" {
				got = c
			}
		}
		if got.Status != Skip {
			t.Fatalf("discovery check = %#v, want Skip", got)
		}
	})
}
