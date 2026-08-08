// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"papio/internal/config"
	"papio/internal/delivery"
	"papio/internal/discovery"
	"papio/internal/job"
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
		if c.Name == "database" && c.Status == Pass && strings.Contains(c.Detail, "schema version 25") {
			dbPass = true
		}
	}
	if !dbPass {
		t.Fatalf("database migration check missing: %+v", report.Checks)
	}
}

// TestCheckAdoptionRootTimesOutAndFailsWithGrantRemediation reproduces the
// TCC wall a download_adoption_root under ~/Downloads can hit: ReadDir
// blocks in-kernel forever waiting on a consent decision doctor can never
// supply. The probe must still return, bounded, and report a Fail naming
// the real remediation instead of hanging the whole doctor run.
func TestCheckAdoptionRootTimesOutAndFailsWithGrantRemediation(t *testing.T) {
	original := adoptionRootReadDir
	block := make(chan struct{})
	t.Cleanup(func() {
		close(block)
		adoptionRootReadDir = original
	})
	adoptionRootReadDir = func(string) ([]os.DirEntry, error) {
		<-block
		return nil, errors.New("unreachable in this test")
	}

	cfg := config.Default()
	cfg.DataDir = t.TempDir()

	var checks []Check
	add := func(name, status, detail, remediation string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
	}
	start := time.Now()
	checkAdoptionRoot(cfg, add)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("checkAdoptionRoot took %s, want bounded near the scan deadline", elapsed)
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want exactly one", checks)
	}
	c := checks[0]
	if c.Name != "adoption_root" || c.Status != Fail {
		t.Fatalf("check = %#v, want a Fail adoption_root check", c)
	}
	if !strings.Contains(c.Remediation, "System Settings") ||
		!strings.Contains(c.Remediation, "Full Disk Access") ||
		!strings.Contains(c.Remediation, "download_adoption_root") {
		t.Fatalf("remediation = %q, want the TCC grant steps naming download_adoption_root", c.Remediation)
	}
}

// TestCheckAdoptionRootMissingDirIsHealthy asserts ENOENT stays Pass: the
// root simply has not been created yet (today's pre-first-download state),
// not a permissions or hang failure.
func TestCheckAdoptionRootMissingDirIsHealthy(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir() // adoptions/ under this root does not exist yet

	var checks []Check
	add := func(name, status, detail, remediation string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
	}
	checkAdoptionRoot(cfg, add)
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want exactly one", checks)
	}
	c := checks[0]
	if c.Name != "adoption_root" || c.Status != Pass {
		t.Fatalf("check = %#v, want a Pass adoption_root check for a not-yet-created root", c)
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

// Going quiet must not make the queue invisible: the action is still the
// user's to finish, so doctor is the out-of-band surface that says so. (It has
// to be out of band — the IPC layer decodes strictly, so widening an existing
// result shape would make an older CLI reject every response from a newer
// daemon. A doctor check is a new element in a list that already exists.)
func TestRunReportsActionsThatHaveGoneQuiet(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seed := func(id, jobID, createdAt, status string) {
		t.Helper()
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO work_requests (id, created_at) VALUES (?, ?)`, "wr_"+jobID, createdAt); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at)
			VALUES (?, ?, 'awaiting_human', '{}', ?, ?)`, jobID, "wr_"+jobID, createdAt, createdAt); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO human_actions (job_id, kind, status, detail, created_at, revision)
			VALUES (?, 'openurl_handoff', ?, 'handoff', ?, 1)`, jobID, status, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func(age time.Duration) string {
		return time.Now().UTC().Add(-age).Format(time.RFC3339Nano)
	}
	seed("quiet", "job_quiet", stamp(job.QuiesceAfter+24*time.Hour), "open")
	seed("live", "job_live", stamp(time.Hour), "open")
	seed("done", "job_done", stamp(job.QuiesceAfter+24*time.Hour), "resolved")

	detail := quiescedDetail(t, ctx, db, Warn)
	if !strings.Contains(detail, "1 human action(s)") {
		t.Fatalf("detail = %q; want exactly the one quiet action — a live one and a resolved one are not waiting", detail)
	}
}

func TestRunPassesWhenNoActionHasGoneQuiet(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	quiescedDetail(t, ctx, db, Pass)
}

func quiescedDetail(t *testing.T, ctx context.Context, db *store.Store, want string) string {
	t.Helper()
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = t.TempDir()
	tool := executable(t)
	report := Run(ctx, cfg, db, pdf.Capability{
		PDFCPU: true, PDFInfo: tool, PDFToText: tool, PDFToPPM: tool, Tesseract: tool,
	}, tool, nil)
	for _, c := range report.Checks {
		if c.Name == "quiesced_actions" {
			if c.Status != want {
				t.Fatalf("status = %q, want %q (detail %q)", c.Status, want, c.Detail)
			}
			return c.Detail
		}
	}
	t.Fatalf("quiesced_actions check missing: %+v", report.Checks)
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
	cfg.Browser.OpenURLBase = "https://example.alma.exlibrisgroup.com/view/uresolver/61EXU_INST/openurl"
	cfg.Browser.Resolvers = map[string]config.Institution{
		"primo": {OpenURLBase: "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXU_INST:61EXU_NDE"},
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
	cfg.Browser.OpenURLBase = "https://example.primo.exlibrisgroup.com/nde/openurl?vid=61EXU_INST:61EXU_NDE"
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

func TestRunIntegrationHostVersionSkew(t *testing.T) {
	const extID = "abcdefghijklmnopabcdefghijklmnop"
	const hostPath = "/opt/homebrew/bin/papio"
	depsFor := func(t *testing.T, hostVersion string) IntegrationDependencies {
		t.Helper()
		cfg := config.Default()
		cfg.Path = filepath.Join(t.TempDir(), "config.toml")
		cfg.Browser.ExtensionID = extID
		manifestDir := t.TempDir()
		manifest := `{"name":"com.orgmentem.papio","path":"` + hostPath + `","type":"stdio",` +
			`"allowed_origins":["chrome-extension://` + extID + `/"]}`
		if err := os.WriteFile(filepath.Join(manifestDir, "com.orgmentem.papio.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		return IntegrationDependencies{
			CLIVersion: "0.17.0",
			LoadConfig: func() (config.Config, error) { return cfg, nil },
			DaemonStatus: func(context.Context, config.Config) (DaemonStatus, error) {
				return DaemonStatus{Status: "ok", Version: "0.17.0"}, nil
			},
			ManifestDir:            func(config.Config) (string, error) { return manifestDir, nil },
			FirefoxDir:             func(config.Config) (string, error) { return t.TempDir(), nil },
			ReadFile:               os.ReadFile,
			HostExecutableResolves: func(string) bool { return true },
			HostExecutableVersion: func(_ context.Context, execPath string) (string, error) {
				if execPath != hostPath {
					t.Fatalf("probed %q, want the executable the manifest names (%q)", execPath, hostPath)
				}
				return hostVersion, nil
			},
			ZotioPreflight: func(context.Context, config.Config) (*zotio.PreflightResult, error) {
				return &zotio.PreflightResult{Version: "1.2.3"}, nil
			},
		}
	}
	find := func(t *testing.T, report Report) Check {
		t.Helper()
		for _, c := range report.Checks {
			if c.Name == "native host (version)" {
				return c
			}
		}
		t.Fatalf("no native host (version) check in %+v", report.Checks)
		return Check{}
	}

	t.Run("matching host passes", func(t *testing.T) {
		report := RunIntegration(context.Background(), depsFor(t, "0.17.0"))
		if check := find(t, report); check.Status != Pass {
			t.Fatalf("check = %#v, want Pass", check)
		}
	})

	t.Run("stale host fails with the remediation", func(t *testing.T) {
		// The brew copy the symlink points at predates the daemon, so browsers
		// keep spawning a host that enforces the old transport rules.
		report := RunIntegration(context.Background(), depsFor(t, "0.16.0"))
		check := find(t, report)
		if check.Status != Fail {
			t.Fatalf("check = %#v, want Fail", check)
		}
		if !strings.Contains(check.Detail, "0.16.0") || !strings.Contains(check.Detail, hostPath) {
			t.Fatalf("detail %q must name the stale version and its path", check.Detail)
		}
		if !strings.Contains(check.Remediation, "papio native-host install") {
			t.Fatalf("remediation = %q, want papio native-host install", check.Remediation)
		}
		if report.OK {
			t.Fatalf("report must fail on host/daemon skew: %+v", report)
		}
	})

	t.Run("unprobeable host warns without failing the report", func(t *testing.T) {
		deps := depsFor(t, "")
		deps.HostExecutableVersion = func(context.Context, string) (string, error) {
			return "", errors.New("permission denied")
		}
		report := RunIntegration(context.Background(), deps)
		if check := find(t, report); check.Status != Warn {
			t.Fatalf("check = %#v, want Warn", check)
		}
	})
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

// documentDeliveryTestConfig builds a config with a structurally clean
// illiad profile: every static Decision 3A condition holds, so the only
// thing standing between prefill_only and auto_capable is recorded live
// acceptance.
func documentDeliveryTestConfig(dataDir string) config.Config {
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = dataDir
	cfg.Email = "researcher@example.test"
	cfg.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	cfg.Browser.DocumentDelivery = &config.DocumentDelivery{
		Kind:              "illiad",
		SubmitPolicy:      "auto_if_unconditional",
		RequestClasses:    []string{"digital_journal_article"},
		LegalBasis:        "institution_policy",
		PatronAttestation: "not_required",
		PatronFeePolicy:   "zero_standard",
		APIKey:            "issued-by-institution",
		PatronRef:         "patron-ref-123",
	}
	return cfg
}

func TestRunDocumentDeliveryDistinguishesDeclaredFromPass(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := documentDeliveryTestConfig(data)
	tool := executable(t)
	report := Run(ctx, cfg, db, pdf.Capability{
		PDFCPU: true, PDFInfo: tool, PDFToText: tool, PDFToPPM: tool, Tesseract: tool,
	}, tool, nil)

	byName := map[string]Check{}
	for _, c := range report.Checks {
		byName[c.Name] = c
	}

	// legal_basis, patron_attestation, and patron_fee_policy are read
	// straight from config, never independently verified — ADR-0017
	// Decision 3C: doctor "never prints PASS for a policy it merely read
	// from config." Each must render DECLARED, and DECLARED must never
	// collapse into PASS.
	for _, name := range []string{
		"document_delivery:default:legal_basis",
		"document_delivery:default:patron_attestation",
		"document_delivery:default:patron_fee_policy",
	} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("%s check missing: %+v", name, report.Checks)
		}
		if c.Status != Declared {
			t.Fatalf("%s status = %q, want %q", name, c.Status, Declared)
		}
		if c.Status == Pass {
			t.Fatalf("%s must never render declared config as PASS", name)
		}
	}

	// kind (config parses, the adapter is shipped) and credentials
	// (api_key/patron_ref presence) are genuinely verifiable offline, so
	// they render PASS, never DECLARED.
	for _, name := range []string{
		"document_delivery:default:kind",
		"document_delivery:default:credentials",
	} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("%s check missing: %+v", name, report.Checks)
		}
		if c.Status != Pass {
			t.Fatalf("%s status = %q, want %q", name, c.Status, Pass)
		}
	}
}

func TestRunDocumentDeliveryLiveAcceptanceFlipsResultVerdict(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := documentDeliveryTestConfig(data)
	tool := executable(t)
	run := func() Report {
		return Run(ctx, cfg, db, pdf.Capability{
			PDFCPU: true, PDFInfo: tool, PDFToText: tool, PDFToPPM: tool, Tesseract: tool,
		}, tool, nil)
	}
	find := func(report Report, name string) (Check, bool) {
		for _, c := range report.Checks {
			if c.Name == name {
				return c, true
			}
		}
		return Check{}, false
	}

	before := run()
	result, ok := find(before, "document_delivery:default:result")
	if !ok || result.Status != Warn || !strings.Contains(result.Detail, "PREFILL ONLY") {
		t.Fatalf("pre-acceptance result = %+v, want warn PREFILL ONLY", result)
	}
	var sawNoLiveAcceptanceBlock bool
	for _, c := range before.Checks {
		if strings.HasPrefix(c.Name, "document_delivery:default:block:") && strings.Contains(c.Detail, "no recorded live acceptance") {
			sawNoLiveAcceptanceBlock = true
		}
	}
	if !sawNoLiveAcceptanceBlock {
		t.Fatalf("pre-acceptance report missing the no-recorded-live-acceptance BLOCK line: %+v", before.Checks)
	}

	svc := delivery.New(db, &cfg, nil)
	if err := svc.RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
		t.Fatalf("record live acceptance: %v", err)
	}

	after := run()
	result, ok = find(after, "document_delivery:default:result")
	if !ok || result.Status != Pass || !strings.Contains(result.Detail, "AUTO-CAPABLE") || !strings.Contains(result.Detail, "digital_journal_article") {
		t.Fatalf("post-acceptance result = %+v, want pass AUTO-CAPABLE for digital_journal_article", result)
	}
	for _, c := range after.Checks {
		if strings.HasPrefix(c.Name, "document_delivery:default:block:") {
			t.Fatalf("post-acceptance report must have no BLOCK lines: %+v", after.Checks)
		}
	}
	liveAcceptance, ok := find(after, "document_delivery:default:live_acceptance")
	if !ok || liveAcceptance.Status != Pass {
		t.Fatalf("live_acceptance check = %+v, want pass", liveAcceptance)
	}
}

// A form-routed provider (openurl, libkey, custom) can never compile
// auto_capable regardless of live acceptance (ADR-0017 Decision 3A: "Only
// source-controlled API integrations can compile auto_capable"). This
// exercises the RESULT line's other class, plus the distinct blocker
// wording for a permanently-prefill provider versus the missing-live-
// acceptance case above.
func TestRunDocumentDeliveryResultLineForPermanentlyPrefillOnlyProvider(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	db, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	cfg.AccessMode = config.ModeConservative
	cfg.DataDir = data
	cfg.Email = "researcher@example.test"
	cfg.Browser.OpenURLBase = "https://resolver.example.edu/openurl"
	cfg.Browser.DocumentDelivery = &config.DocumentDelivery{
		Kind:         "openurl",
		BaseURL:      "https://ill.example.edu/request",
		SubmitPolicy: "prefill_only",
	}
	tool := executable(t)
	report := Run(ctx, cfg, db, pdf.Capability{
		PDFCPU: true, PDFInfo: tool, PDFToText: tool, PDFToPPM: tool, Tesseract: tool,
	}, tool, nil)

	var result Check
	var block Check
	for _, c := range report.Checks {
		switch {
		case c.Name == "document_delivery:default:result":
			result = c
		case strings.HasPrefix(c.Name, "document_delivery:default:block:"):
			block = c
		}
	}
	if result.Status != Warn || !strings.Contains(result.Detail, "PREFILL ONLY") {
		t.Fatalf("result = %+v, want warn PREFILL ONLY", result)
	}
	if !strings.Contains(block.Detail, "prefilled request form") {
		t.Fatalf("block = %+v, want the form-routed provider wording, not the live-acceptance wording", block)
	}
	if strings.Contains(block.Detail, "no recorded live acceptance") {
		t.Fatalf("block = %+v, a permanently-prefill provider must not cite the live-acceptance wording", block)
	}
}

// TestRunDocumentDeliveryFulfillmentLine pins the 2026-08-07 amendment's
// doctor surface: submission auto-capability (RESULT) and end-to-end
// fulfillment retrieval (the new FULFILLMENT line) are reported
// independently — an operator can be AUTO-CAPABLE for submission while
// still seeing fulfillment: none until patron_web_base_url is configured.
func TestRunDocumentDeliveryFulfillmentLine(t *testing.T) {
	ctx := context.Background()
	find := func(report Report, name string) (Check, bool) {
		for _, c := range report.Checks {
			if c.Name == name {
				return c, true
			}
		}
		return Check{}, false
	}
	run := func(cfg config.Config, db *store.Store) Report {
		tool := executable(t)
		return Run(ctx, cfg, db, pdf.Capability{
			PDFCPU: true, PDFInfo: tool, PDFToText: tool, PDFToPPM: tool, Tesseract: tool,
		}, tool, nil)
	}

	t.Run("absent patron_web_base_url warns fulfillment none, even once auto-capable for submission", func(t *testing.T) {
		data := t.TempDir()
		db, err := store.Open(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		cfg := documentDeliveryTestConfig(data)
		if err := delivery.New(db, &cfg, nil).RecordLiveAcceptance(ctx, "default", "illiad"); err != nil {
			t.Fatalf("record live acceptance: %v", err)
		}
		report := run(cfg, db)
		result, ok := find(report, "document_delivery:default:result")
		if !ok || result.Status != Pass || !strings.Contains(result.Detail, "AUTO-CAPABLE") {
			t.Fatalf("result = %+v, want pass AUTO-CAPABLE", result)
		}
		fulfillment, ok := find(report, "document_delivery:default:fulfillment")
		if !ok || fulfillment.Status != Warn || !strings.Contains(fulfillment.Detail, "fulfillment: none") {
			t.Fatalf("fulfillment = %+v, want warn \"fulfillment: none\" despite AUTO-CAPABLE submission", fulfillment)
		}
	})

	t.Run("configured patron_web_base_url passes fulfillment patron_web", func(t *testing.T) {
		data := t.TempDir()
		db, err := store.Open(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		cfg := documentDeliveryTestConfig(data)
		cfg.Browser.DocumentDelivery.PatronWebBaseURL = "https://illiadweb.example.edu/illiad/illiad.dll"
		report := run(cfg, db)
		fulfillment, ok := find(report, "document_delivery:default:fulfillment")
		if !ok || fulfillment.Status != Pass || !strings.Contains(fulfillment.Detail, "fulfillment: patron_web") {
			t.Fatalf("fulfillment = %+v, want pass \"fulfillment: patron_web\"", fulfillment)
		}
	})
}

// TestRunDocumentDeliveryPollHealth pins the poll-health surface
// (ADR-0017 Decision 4): no live requests reports Skip; a live row with
// 3+ consecutive failed status polls reports Warn "degraded", never a
// claim that the request itself failed.
func TestRunDocumentDeliveryPollHealth(t *testing.T) {
	ctx := context.Background()
	find := func(report Report, name string) (Check, bool) {
		for _, c := range report.Checks {
			if c.Name == name {
				return c, true
			}
		}
		return Check{}, false
	}
	run := func(cfg config.Config, db *store.Store) Report {
		tool := executable(t)
		return Run(ctx, cfg, db, pdf.Capability{
			PDFCPU: true, PDFInfo: tool, PDFToText: tool, PDFToPPM: tool, Tesseract: tool,
		}, tool, nil)
	}

	t.Run("no live requests skips", func(t *testing.T) {
		data := t.TempDir()
		db, err := store.Open(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		cfg := documentDeliveryTestConfig(data)
		report := run(cfg, db)
		health, ok := find(report, "document_delivery:default:poll_health")
		if !ok || health.Status != Skip {
			t.Fatalf("poll_health = %+v, want skip with no live requests", health)
		}
	})

	t.Run("3+ consecutive poll failures warns degraded", func(t *testing.T) {
		data := t.TempDir()
		db, err := store.Open(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		cfg := documentDeliveryTestConfig(data)

		svc := delivery.New(db, &cfg, nil)
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO work_requests (id, created_at) VALUES (?, ?)`, "wr_poll1", store.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at) VALUES (?, ?, 'queued', '{}', ?, ?)`,
			"poll1", "wr_poll1", store.Now(), store.Now()); err != nil {
			t.Fatal(err)
		}
		created, err := svc.Create(ctx, delivery.CreateRequest{
			JobID: "poll1", InstitutionProfile: "default", Provider: "illiad",
			RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/poll-health", GateProfileDigest: "d",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.UpdateState(ctx, created.ID, delivery.StateSubmitted); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx,
			`UPDATE delivery_requests SET consecutive_poll_failures = 3, last_poll_error_class = 'transient_transport' WHERE id = ?`, created.ID); err != nil {
			t.Fatal(err)
		}

		report := run(cfg, db)
		health, ok := find(report, "document_delivery:default:poll_health")
		if !ok || health.Status != Warn || !strings.Contains(health.Detail, "3+ consecutive failed status polls") {
			t.Fatalf("poll_health = %+v, want warn degraded", health)
		}
	})

	// TestDocumentDeliveryPollHealthRemedyNamesRealCommand (folded in here
	// rather than a standalone Test func, sharing this test's run/find
	// helpers): a contract-drift park has one operator recovery path,
	// 'papio delivery resume <request-id>' (internal/cli/delivery.go's
	// newDeliveryResumeCommand); this pins the remedy text against that
	// exact command name and the affected request's id, so renaming or
	// removing the CLI command without updating this string is caught here
	// rather than shipping a remedy that names a command that does not
	// exist — the P2 gap this test closes.
	t.Run("contract drift names the real papio delivery resume command", func(t *testing.T) {
		data := t.TempDir()
		db, err := store.Open(ctx, data)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		cfg := documentDeliveryTestConfig(data)
		svc := delivery.New(db, &cfg, nil)
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO work_requests (id, created_at) VALUES (?, ?)`, "wr_poll_drift", store.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx,
			`INSERT INTO jobs (id, work_request_id, state, policy_json, created_at, updated_at) VALUES (?, ?, 'queued', '{}', ?, ?)`,
			"poll_drift", "wr_poll_drift", store.Now(), store.Now()); err != nil {
			t.Fatal(err)
		}
		created, err := svc.Create(ctx, delivery.CreateRequest{
			JobID: "poll_drift", InstitutionProfile: "default", Provider: "illiad",
			RequestClass: "digital_journal_article", WorkIdentity: "doi:10.1/poll-drift", GateProfileDigest: "d",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.UpdateState(ctx, created.ID, delivery.StateSubmitted); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx,
			`UPDATE delivery_requests SET consecutive_poll_failures = 9, last_poll_error_class = ? WHERE id = ?`,
			delivery.PollErrorClassContractDrift, created.ID); err != nil {
			t.Fatal(err)
		}

		report := run(cfg, db)
		health, ok := find(report, "document_delivery:default:poll_health")
		if !ok {
			t.Fatal("poll_health check missing")
		}
		wantCommand := fmt.Sprintf("papio delivery resume %d", created.ID)
		if !strings.Contains(health.Remediation, wantCommand) {
			t.Fatalf("remediation = %q, want it to name the real command %q", health.Remediation, wantCommand)
		}
		if strings.Contains(health.Remediation, "papio delivery reconciliation") {
			t.Fatalf("remediation = %q, still names the nonexistent 'papio delivery reconciliation' command", health.Remediation)
		}
		if !strings.Contains(health.Remediation, "papio jobs retry") {
			t.Fatalf("remediation = %q, want it to also name 'papio jobs retry <job-id>' — resume alone does not force an immediate poll", health.Remediation)
		}
	})
}
