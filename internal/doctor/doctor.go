// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package doctor reports actionable, secret-free readiness checks for the
// Phase 1 acquisition core.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"papio/internal/browser"
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

// Status values are stable CLI/agent output. Declared is distinct from Pass
// on purpose (ADR-0017 Decision 3C): it marks a fact doctor only ever read
// from config — a policy declaration such as legal_basis, patron_attestation,
// or patron_fee_policy — never something doctor independently verified. A
// declared value can be wrong; doctor must never render it as a PASS.
const (
	Pass     = "pass"
	Warn     = "warn"
	Fail     = "fail"
	Skip     = "skip"
	Declared = "declared"
)

// Check is one deterministic diagnostic. Detail never contains a credential.
type Check struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

// Report is the doctor command result.
type Report struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

// Run evaluates config, filesystem, database, executable, source credentials,
// discovery backend health, and PDF helper capabilities. A nil store means
// database integrity is checked by the daemon-backed doctor command instead;
// a nil discoverySource means discovery backend health is checked there too.
func Run(ctx context.Context, cfg config.Config, db *store.Store, capability pdf.Capability, workerBinary string, discoverySource discovery.Source) Report {
	var checks []Check
	add := func(name, status, detail, remediation string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail, Remediation: remediation})
	}

	if _, err := cfg.RequireAccessMode(); err != nil {
		add("access_mode", Fail, "no explicit access mode is configured", "set access_mode to conservative, assisted, or delegated")
	} else {
		add("access_mode", Pass, "explicit access mode configured", "")
	}
	if cfg.Fetch.AllowHTTPLoopback {
		add("fetch_policy", Warn, "HTTP loopback development override is enabled", "disable fetch.allow_http_loopback outside fixture tests")
	} else {
		add("fetch_policy", Pass, "HTTPS-only production policy", "")
	}

	if err := checkDataDir(cfg.DataDir); err != nil {
		add("data_dir", Fail, "data directory is not private and writable", err.Error())
	} else {
		add("data_dir", Pass, "private writable data directory", "")
	}
	if cfg.Path != "" {
		if info, err := os.Stat(cfg.Path); err == nil {
			if info.Mode().Perm()&0o077 != 0 {
				add("config_permissions", Fail, "configuration is readable by group or others", "chmod 600 "+cfg.Path)
			} else {
				add("config_permissions", Pass, "configuration permissions are user-only", "")
			}
		} else if os.IsNotExist(err) {
			add("config_permissions", Warn, "configuration file does not exist", "create "+cfg.Path)
		} else {
			add("config_permissions", Fail, "configuration metadata cannot be read", "check file ownership and permissions")
		}
	}

	if db == nil {
		add("database", Skip, "database integrity is checked by the daemon", "")
	} else if err := db.IntegrityCheck(ctx); err != nil {
		add("database", Fail, "SQLite integrity check failed", "restore from a verified backup before acquisition")
	} else {
		version, err := db.UserVersion(ctx)
		if err != nil {
			add("database", Fail, "database schema version could not be read", "inspect database permissions")
		} else {
			add("database", Pass, fmt.Sprintf("SQLite integrity ok; schema version %d", version), "")
		}
	}

	if db == nil {
		add("uncollected_acquisitions", Skip, "uncollected acquisitions are checked by the daemon", "")
	} else {
		n, oldest, err := uncollectedAcquisitions(ctx, db)
		switch {
		case err != nil:
			add("uncollected_acquisitions", Warn, "uncollected acquisitions could not be counted", "inspect database permissions")
		case n == 0:
			add("uncollected_acquisitions", Pass, "every acquisition older than the grace period has been collected", "")
		default:
			add("uncollected_acquisitions", Warn,
				fmt.Sprintf("%d acquired full texts have never been exported; oldest settled %s ago", n, oldest.Round(time.Hour)),
				"run `papio jobs list --state ready --limit 500` and export them, or cancel the jobs if the work is no longer wanted")
		}
	}

	// The counterpart to going quiet. An action papio has stopped volunteering
	// is still the user's to finish, so the queue must not become invisible
	// just because it stopped being noisy.
	if db == nil {
		add("quiesced_actions", Skip, "quiesced human actions are checked by the daemon", "")
	} else {
		n, oldest, err := quiescedActions(ctx, db)
		switch {
		case err != nil:
			add("quiesced_actions", Warn, "quiesced human actions could not be counted", "inspect database permissions")
		case n == 0:
			add("quiesced_actions", Pass, "no human action has been waiting past the quiesce window", "")
		default:
			detail := fmt.Sprintf("%d human action(s) have gone quiet after waiting more than %s",
				n, quiesceDays(job.QuiesceAfter))
			// An unparseable stored timestamp yields a zero age. Say nothing
			// rather than "oldest opened 0s ago", which reads as a live action.
			if oldest > 0 {
				detail += fmt.Sprintf("; oldest opened %s ago", quiesceDays(oldest))
			}
			add("quiesced_actions", Warn, detail,
				"papio no longer offers these on its own — run `papio actions open` to drive them, or `papio actions dismiss` to clear the ones you are done with")
		}
	}

	if workerBinary == "" {
		add("pdf_worker", Fail, "papio worker executable path is missing", "run doctor from the papio binary")
	} else if info, err := os.Stat(workerBinary); err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		add("pdf_worker", Fail, "papio worker executable is not runnable", "install or rebuild papio and retry")
	} else {
		add("pdf_worker", Pass, "isolated pdfcpu worker is runnable", "")
	}
	if capability.PDFToText == "" {
		// Worth a blunt detail: without semantic extraction every validated PDF
		// fails identity and is staged for human review, which presents as
		// "papio stopped trusting anything" rather than as a missing tool.
		add("pdftotext", Fail,
			"Poppler pdftotext is unavailable, so every PDF will be staged for human review",
			"install poppler; if it is already installed, this daemon cannot see it — stop it with `papio daemon stop` so the next command restarts it from your shell")
	} else {
		add("pdftotext", Pass, "Poppler semantic extraction available at "+capability.PDFToText, "")
	}
	if capability.PDFInfo == "" {
		add("pdfinfo", Warn, "Poppler pdfinfo cross-check is unavailable", "install poppler for independent page-count checks")
	} else {
		add("pdfinfo", Pass, "Poppler structural cross-check available", "")
	}
	if cfg.PDF.OCREnabled {
		if capability.PDFToPPM == "" || capability.Tesseract == "" {
			add("ocr", Fail, "OCR is enabled but pdftoppm or tesseract is unavailable", "install poppler and tesseract, or explicitly disable OCR")
		} else {
			add("ocr", Pass, "bounded OCR fallback available", "")
		}
	} else {
		add("ocr", Warn, "OCR fallback is explicitly disabled", "image-only papers will require review")
	}

	checkSourceCredentials(cfg, add)
	checkResolverBases(cfg, add)
	checkDocumentDelivery(ctx, cfg, db, add)
	checkDiscoveryHealth(cfg, discoverySource, add)
	sort.SliceStable(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	out := Report{OK: true, Checks: checks}
	for _, c := range checks {
		if c.Status == Fail {
			out.OK = false
		}
	}
	return out
}

// uncollectedGracePeriod is how long an acquired full text may sit before it
// reads as abandoned rather than simply not fetched yet. It is a constant, not
// a setting: a new [pdf]-style config field would make an older binary reject
// the whole config file, and nobody has yet needed a different number. Promote
// it to a `papio doctor` flag at the first real request, not before.
const uncollectedGracePeriod = 7 * 24 * time.Hour

// uncollectedAcquisitions counts full texts papio acquired that nobody ever
// exported, and how long the oldest has waited.
//
// This can only be answered here. A consumer sees the jobs it can still
// address, but an acquisition is stranded precisely when the key that named it
// stops being derivable — so the orphan is by definition the one the consumer
// cannot ask about. papio knows the one fact it cannot reconstruct: this was
// acquired and nobody came for it.
//
// Deliberately reports a count and an age rather than job ids. The human
// running doctor is the only consumer today; anything programmatic needs a new
// IPC method name, because widening a ratified result breaks older clients.
// When that method arrives it must key on job id, not work key: the job id is
// what survives an identity change on the consumer's side, and a work-shaped
// answer would be useless for exactly the case it exists to catch.
func uncollectedAcquisitions(ctx context.Context, db *store.Store) (int, time.Duration, error) {
	// updated_at is when the job settled: nothing transitions a ready job after.
	row := db.DB().QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MIN(j.updated_at), '')
		FROM jobs j
		JOIN job_artifacts ja ON ja.job_id = j.id
		WHERE j.state = 'ready'
		  AND j.updated_at < ?
		  AND NOT EXISTS (SELECT 1 FROM exports e WHERE e.job_id = j.id)`,
		time.Now().UTC().Add(-uncollectedGracePeriod).Format(time.RFC3339Nano))
	var n int
	var oldest string
	if err := row.Scan(&n, &oldest); err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, nil
	}
	settled, err := time.Parse(time.RFC3339Nano, oldest)
	if err != nil {
		return n, 0, nil
	}
	return n, time.Since(settled), nil
}

// quiescedActions counts open human actions that have aged past
// job.QuiesceAfter, and how long the oldest has waited. These are the actions
// papio has stopped offering and stopped reminding about; they remain the
// user's to finish, so doctor is where they resurface.
func quiescedActions(ctx context.Context, db *store.Store) (int, time.Duration, error) {
	row := db.DB().QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MIN(created_at), '')
		FROM human_actions
		WHERE status = 'open' AND created_at < ?`,
		time.Now().UTC().Add(-job.QuiesceAfter).Format(time.RFC3339Nano))
	var n int
	var oldest string
	if err := row.Scan(&n, &oldest); err != nil {
		return 0, 0, err
	}
	if n == 0 {
		return 0, 0, nil
	}
	opened, err := time.Parse(time.RFC3339Nano, oldest)
	if err != nil {
		return n, 0, nil
	}
	return n, time.Since(opened), nil
}

// quiesceDays renders a multi-day duration the way a person reads it. Go's
// Duration has no day unit, so the raw value prints as "168h0m0s" — accurate,
// and useless in a line whose whole job is to be scanned.
func quiesceDays(d time.Duration) string {
	if d < 48*time.Hour {
		return d.Round(time.Hour).String()
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}

func checkDataDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("set data_dir")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("chmod %s to 0700: %w", path, err)
	}
	probe, err := os.CreateTemp(path, ".doctor-write-*")
	if err != nil {
		return fmt.Errorf("make %s writable by its owner", path)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

// checkResolverBases warns when an OpenURL base points at a raw Alma uresolver
// (…/view/uresolver/…). That deep link sends an unauthenticated patron to the
// Alma staff login (…/mng/login), which patrons cannot complete. The patron
// path is the institution's Primo OpenURL endpoint (…/discovery/openurl or
// …/nde/openurl?vid=…), which routes sign-in through the IdP.
func checkResolverBases(cfg config.Config, add func(string, string, string, string)) {
	const remediation = "point openurl_base_url at the institution's Primo OpenURL endpoint " +
		"(…/discovery/openurl or …/nde/openurl?vid=…) instead of the raw …/view/uresolver/ link"
	if isRawAlmaResolver(cfg.Browser.OpenURLBase) {
		add("resolver_base", Warn,
			"default OpenURL base is a raw Alma uresolver; unauthenticated patrons are sent to the Alma staff login (/mng/login)",
			remediation)
	} else if strings.TrimSpace(cfg.Browser.OpenURLBase) != "" {
		add("resolver_base", Pass, "default OpenURL base uses a patron discovery endpoint", "")
	}
	names := make([]string, 0, len(cfg.Browser.Resolvers))
	for name := range cfg.Browser.Resolvers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if isRawAlmaResolver(cfg.Browser.Resolvers[name].OpenURLBase) {
			add("resolver_base:"+name, Warn,
				"OpenURL base is a raw Alma uresolver; unauthenticated patrons are sent to the Alma staff login (/mng/login)",
				remediation)
		}
	}
}

// isRawAlmaResolver reports whether base is an Alma link-resolver deep link
// (host *.alma.exlibrisgroup.com with a /view/uresolver/ path), the shape that
// bounces patrons to the Alma staff console instead of Primo/IdP sign-in.
func isRawAlmaResolver(base string) bool {
	if strings.TrimSpace(base) == "" {
		return false
	}
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Host), ".alma.exlibrisgroup.com") &&
		strings.Contains(strings.ToLower(u.Path), "/view/uresolver/")
}

func checkSourceCredentials(cfg config.Config, add func(string, string, string, string)) {
	if cfg.SourcePolicy(config.SourceUnpaywall).Enabled {
		if strings.TrimSpace(cfg.Email) == "" {
			add("source_unpaywall", Fail, "Unpaywall is enabled without a contact email", "set email in config.toml")
		} else {
			add("source_unpaywall", Pass, "Unpaywall contact identity configured", "")
		}
	}
	if cfg.SourcePolicy(config.SourceOpenAlex).Enabled {
		if strings.TrimSpace(cfg.Email) == "" {
			add("source_openalex", Fail, "OpenAlex is enabled without a contact email", "set email (polite pool); sources.openalex.api_key is optional premium capacity")
		} else if strings.TrimSpace(cfg.SourcePolicy(config.SourceOpenAlex).APIKey) == "" {
			// Passing cleanly here reads as fully configured, and that is how a
			// real operator missed it: they measured the anonymous tier's 1000
			// credits a day against an unkeyed client and recorded multi-day
			// cohort acquisition as a property of the design. An account is
			// free and roughly ten times the allowance, so the gap is worth a
			// word even though nothing is broken.
			add("source_openalex", Warn, "OpenAlex is on the anonymous allowance, roughly a tenth of an account's",
				"set sources.openalex.api_key from a free openalex.org account; the anonymous quota is shared per-IP with every other tool on this machine")
		} else {
			add("source_openalex", Pass, "OpenAlex credentials configured", "")
		}
	}
	for _, source := range []string{config.SourceCORE, config.SourceCrossrefTDM} {
		p := cfg.SourcePolicy(source)
		if !p.Enabled {
			continue
		}
		name := "source_" + strings.ReplaceAll(source, "_", "-")
		if strings.TrimSpace(p.APIKey) == "" {
			add(name, Fail, source+" is enabled without its API credential", "configure the API key/token, or disable the source")
		} else {
			add(name, Pass, source+" credential configured", "")
		}
	}
}

// configuredDocumentDeliveryProfiles returns the institution profiles
// (default and/or named) with document_delivery configured, sorted for
// deterministic check ordering.
func configuredDocumentDeliveryProfiles(cfg config.Config) []string {
	var names []string
	if cfg.Browser.DocumentDelivery != nil {
		names = append(names, "default")
	}
	for name, inst := range cfg.Browser.Resolvers {
		if inst.DocumentDelivery != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// checkDocumentDelivery reports one section per configured document_delivery
// profile (ADR-0017 Decision 3C): PASS/OBSERVED lines for facts doctor can
// actually verify offline or cheaply (config parses, the kind adapter is
// shipped, api_key/patron_ref presence for illiad, and — when a store is
// available — whether one live acceptance is on record), DECLARED lines for
// policy doctor only ever reads from config (legal_basis, patron_attestation,
// patron_fee_policy — 3C: "it never prints PASS for a policy it merely read
// from config"), and the compiled RESULT (AUTO-CAPABLE or PREFILL ONLY plus
// its BLOCK lines).
//
// db, when present, folds in the real store-backed live-acceptance fact via
// Service.ResolveGateProfile — CompileGateProfile alone can never observe it
// (see internal/delivery/gate.go). A nil db (doctor run before the daemon is
// reachable) reports the pure compiled answer instead and Skips the
// live-acceptance line, mirroring every other db-gated check in Run.
//
// v1 does not probe the provider API — no auth check, no patron-mapping
// lookup, no transaction/patron-request lookup, no adapter conformance
// version, and it never creates a test request: Decision 3C forbids a probe
// request outright, and a safe, budget-respecting auth-only check is future
// work, not this pass.
func checkDocumentDelivery(ctx context.Context, cfg config.Config, db *store.Store, add func(string, string, string, string)) {
	for _, name := range configuredDocumentDeliveryProfiles(cfg) {
		inst, _ := cfg.InstitutionFor(name)
		dd := inst.DocumentDelivery
		prefix := "document_delivery:" + name

		profile := delivery.CompileGateProfile(inst, name)
		switch {
		case db == nil:
			add(prefix+":live_acceptance", Skip, "live-acceptance record is checked by the daemon", "")
		default:
			resolved, err := delivery.New(db, &cfg, nil).ResolveGateProfile(ctx, name, inst)
			if err != nil {
				add(prefix+":live_acceptance", Fail, "live-acceptance record could not be read: "+err.Error(), "inspect database permissions")
				break
			}
			profile = resolved
			if profile.LiveAccepted {
				add(prefix+":live_acceptance", Pass, "one supervised submit-and-reconcile is recorded for this profile", "")
			} else {
				add(prefix+":live_acceptance", Warn, "no recorded live acceptance for this profile", "")
			}
		}

		if profile.Class == delivery.GateClassInvalid {
			add(prefix+":kind", Fail, "kind "+documentDeliveryOrUnset(dd.Kind)+" has no shipped delivery adapter", "use kind = openurl, libkey, illiad, or custom")
			continue
		}
		add(prefix+":kind", Pass, "kind "+documentDeliveryOrUnset(dd.Kind)+" delivery adapter is shipped", "")

		if dd.Kind == "illiad" {
			switch {
			case dd.APIKey == "" && dd.PatronRef == "":
				add(prefix+":credentials", Warn, "api_key and patron_ref are not configured", "configure document_delivery.api_key and .patron_ref (0600 config only)")
			case dd.APIKey == "":
				add(prefix+":credentials", Warn, "api_key is not configured", "configure document_delivery.api_key (0600 config only)")
			case dd.PatronRef == "":
				add(prefix+":credentials", Warn, "patron_ref is not configured", "configure document_delivery.patron_ref (0600 config only)")
			default:
				add(prefix+":credentials", Pass, "api_key and patron_ref are configured", "")
			}
			// v1 does not call ILLiad's Web Platform API to verify the key
			// authenticates or that patron_ref resolves to a real patron —
			// see the doc comment above.
		}

		add(prefix+":legal_basis", Declared, "legal_basis is "+documentDeliveryOrUnset(dd.LegalBasis)+" (declared in config, not independently verified)", "")
		add(prefix+":patron_attestation", Declared, "patron_attestation is "+documentDeliveryOrUnset(dd.PatronAttestation)+" (declared in config, not independently verified)", "")
		add(prefix+":patron_fee_policy", Declared, "patron_fee_policy is "+documentDeliveryOrUnset(dd.PatronFeePolicy)+" (declared in config, not independently verified)", "")

		if profile.Class == delivery.GateClassAutoCapable {
			add(prefix+":result", Pass, "AUTO-CAPABLE · "+strings.Join(documentDeliverySortedClasses(profile.SupportedRequestClasses), ", "), "")
			continue
		}
		add(prefix+":result", Warn, "PREFILL ONLY", "an operator must resolve the BLOCK lines below before this profile can auto-submit")
		for i, b := range profile.Blockers {
			add(fmt.Sprintf("%s:block:%d", prefix, i+1), Warn, "BLOCK "+b.Code+": "+documentDeliveryBlockerText(b), "")
		}
	}
}

func documentDeliverySortedClasses(classes map[string]bool) []string {
	names := make([]string, 0, len(classes))
	for name := range classes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func documentDeliveryOrUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return fmt.Sprintf("%q", s)
}

// documentDeliveryBlockerText maps ADR-0017 Decision 3A's closed 13-code
// blocker vocabulary to the human-readable sentence a BLOCK line prints for
// it, matching the wording papio init uses for the same vocabulary.
func documentDeliveryBlockerText(b delivery.Blocker) string {
	switch b.Code {
	case delivery.BlockerProviderNotImplemented:
		return "papio has no delivery adapter for this provider"
	case delivery.BlockerProviderNotAutoCapable:
		if b.Evidence == "no recorded live acceptance" {
			return "papio has not yet completed one supervised submit-and-reconcile against this deployment (no recorded live acceptance)"
		}
		return "this delivery route only opens a prefilled request form; it has no automatic submission-and-reconciliation contract"
	case delivery.BlockerAPICredentialMissing:
		return "the institution-issued API key is not configured"
	case delivery.BlockerPatronMappingUnverified:
		return "the patron reference used to map requests to your account is not configured"
	case delivery.BlockerRequestClassUnsupported:
		return "none of the configured request classes are supported for automatic submission (v1: digital journal articles only)"
	case delivery.BlockerPerRequestLogin:
		return "your institution requires a login step on every request before it can be created"
	case delivery.BlockerPerRequestTerms:
		return "your institution requires a per-request terms declaration before a request can be created"
	case delivery.BlockerPerRequestCopyrightDeclaration:
		return "your institution requires a copyright declaration on every digital-copy request"
	case delivery.BlockerPerRequestPurposeStatement:
		return "your institution requires a per-request purpose statement before a request can be created"
	case delivery.BlockerPatronFeeNotZero:
		return "your institution charges a per-request patron fee, so requests cannot be auto-submitted"
	case delivery.BlockerPatronFeeUnknown:
		return "the patron fee policy is not declared, so papio cannot confirm requests are free"
	case delivery.BlockerReconciliationUnavailable:
		return "this provider has no reconciliation support, so a submitted request could never be confirmed"
	case delivery.BlockerInstitutionPolicyUnknown:
		return "a required policy declaration is missing or not recognized"
	default:
		return b.Evidence
	}
}

// checkDiscoveryHealth reports the state of the configured discovery
// backends. A backend that started failing used to be invisible: search
// still returned the survivor's results, so a user seeing thin results
// could not tell a dead backend from a work that simply is not indexed.
//
// A nil discoverySource, or one that does not implement BackendHealth,
// means backend health cannot be observed from here — mirrored as Skip,
// the same convention the database check uses for a nil store.
func checkDiscoveryHealth(cfg config.Config, source discovery.Source, add func(string, string, string, string)) {
	if source == nil {
		add("discovery", Skip, "discovery source is checked by the daemon", "")
		return
	}
	health, ok := source.(discovery.BackendHealth)
	if !ok {
		add("discovery", Skip, "configured discovery source does not report backend health", "")
		return
	}
	failures := health.LastFailures()
	if len(failures) == 0 {
		add("discovery", Pass, fmt.Sprintf("%d discovery backend(s) configured; all healthy", discoveryBackendCount(cfg)), "")
		return
	}
	names := make([]string, len(failures))
	for i, f := range failures {
		names[i] = f.Source
	}
	detail := fmt.Sprintf("%s: %s", failures[0].Source, failures[0].Message)
	if len(failures) > 1 {
		detail = fmt.Sprintf("%s failing; first error (%s): %s", strings.Join(names, ", "), failures[0].Source, failures[0].Message)
	}
	add("discovery", Warn, detail, "search results may be incomplete; check network connectivity and credentials for the named backend(s)")
}

// discoveryBackendCount mirrors bootstrap's discoverySources default: an
// empty [discovery] sources list still searches OpenAlex alone.
func discoveryBackendCount(cfg config.Config) int {
	if len(cfg.Discovery.Sources) == 0 {
		return 1
	}
	return len(cfg.Discovery.Sources)
}

// DefaultWorkerPath resolves the current executable for pdf worker re-exec.
func DefaultWorkerPath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	path, _ = filepath.Abs(path)
	return path
}

// DaemonStatus is the daemon information returned by the ping status RPC.
// It must accept every field ping can emit: the CLI decodes strictly, and a
// missing field here turns an optional ping addition (update availability)
// into a doctor FAIL. Regression: v0.9.1 doctor failed against its own
// daemon once the update checker had populated update_available.
type DaemonStatus struct {
	Status                 string `json:"status"`
	Version                string `json:"version"`
	ExtensionConnected     bool   `json:"extension_connected"`
	ExtensionVersion       string `json:"extension_version"`
	PendingBrowserSessions int    `json:"pending_browser_sessions"`
	BrowserSessionDenied   int    `json:"browser_session_denied"`
	UpdateAvailable        *bool  `json:"update_available,omitempty"`
	LatestVersion          string `json:"latest_version,omitempty"`
	ZotioUpdateAvailable   *bool  `json:"zotio_update_available,omitempty"`
	ZotioLatestVersion     string `json:"zotio_latest_version,omitempty"`
}

// IntegrationDependencies supplies the local integration checks. The functions
// are deliberately independent of command-line plumbing so other callers can
// reuse RunIntegration.
type IntegrationDependencies struct {
	CLIVersion   string
	LoadConfig   func() (config.Config, error)
	DaemonStatus func(context.Context, config.Config) (DaemonStatus, error)
	ManifestDir  func(config.Config) (string, error)
	FirefoxDir   func(config.Config) (string, error)
	ReadFile     func(string) ([]byte, error)
	// HostExecutableResolves reports whether the native-host executable a
	// manifest points at actually exists (following the symlink). Optional: when
	// nil the check is skipped. Catches a dangling host symlink — e.g. a brew
	// upgrade deleting the versioned binary the symlink was pinned to.
	HostExecutableResolves func(execPath string) bool
	// HostExecutableVersion reports the bare version string (no "papio "
	// prefix) that the native-host executable at execPath prints. Optional:
	// when nil the skew check is skipped. Catches a host binary older than the
	// running daemon — see runHostVersionCheck for why that is not cosmetic.
	HostExecutableVersion func(ctx context.Context, execPath string) (string, error)
	ZotioPreflight        func(context.Context, config.Config) (*zotio.PreflightResult, error)
	CheckUpdates          func(context.Context, config.Config) *update.Info
	CheckZotioUpdates     func(context.Context, config.Config) *update.Info
	// LibrarySources probes the configured generic holdings sources. A nil
	// function is an unavailable probe facility, reported as a bounded Skip when
	// sources are configured.
	LibrarySources func(context.Context, config.Config) ([]LibrarySourceStatus, error)
}

// LibrarySourceStatus is one holdings source's standing state, as probed for
// diagnostics.
//
// This is *only* diagnostics. Runtime completeness travels in the ownership
// lookup result itself (ADR-0008 invariant 1), because a caller deciding whether
// "no match" means "not held" needs the answer at decision time, not from a
// command the user may never run. Do not delete the in-result health on the
// grounds that doctor reports it.
type LibrarySourceStatus struct {
	Name        string
	Complete    bool
	EntryCount  int
	LastSuccess time.Time
	FailureCode string
}

// RunIntegration checks the daemon, browser extension, native-host manifests,
// and zotio. Configuration is loaded by the supplied seam so this function can
// report parse errors as part of the same diagnostic report.
func RunIntegration(ctx context.Context, deps IntegrationDependencies) Report {
	report := Report{OK: true}
	add := func(name, status, detail, remediation string) {
		report.Checks = append(report.Checks, Check{
			Name: name, Status: status, Detail: detail, Remediation: remediation,
		})
		if status == Fail {
			report.OK = false
		}
	}
	skipRemaining := func(reason string) {
		add("daemon", Skip, reason, "")
		add("integrations", Skip, reason+" (extension, native hosts, zotio, updates)", "")
	}

	if !integrationDependenciesComplete(deps) {
		add("doctor", Fail, "doctor command dependencies are incomplete", "reinstall papio")
		return report
	}
	cfg, err := deps.LoadConfig()
	if err != nil {
		remediation := "correct the configuration error above"
		if strings.Contains(strings.ToLower(err.Error()), "unknown field") {
			remediation = "update papio or remove the unrecognized field"
		}
		add("config", Fail, err.Error(), remediation)
		skipRemaining("skipped: configuration did not parse")
		return report
	}
	add("config", Pass, "parsed "+cfg.Path, "")

	status, err := deps.DaemonStatus(ctx, cfg)
	if err != nil {
		add("daemon", Fail, "not running or unreachable ("+err.Error()+")", "check the daemon log, then retry 'papio doctor'")
		add("integrations", Skip, "skipped: daemon is unreachable (extension, native hosts, zotio, database)", "")
		return report
	}
	if status.Status != "ok" || strings.TrimSpace(status.Version) == "" {
		add("daemon", Fail, fmt.Sprintf("unexpected daemon status %q (version %q)", status.Status, status.Version), "papio daemon stop, then rerun doctor")
		add("integrations", Skip, "skipped: daemon status is invalid (extension, native hosts, zotio, database)", "")
		return report
	}
	if status.Version != deps.CLIVersion {
		add("daemon", Warn, fmt.Sprintf("reachable; daemon %s, CLI %s", status.Version, deps.CLIVersion), "papio daemon stop (next command autostarts the new daemon)")
	} else {
		add("daemon", Pass, "reachable; version "+status.Version, "")
	}
	if status.ExtensionConnected {
		detail := "connected"
		if status.ExtensionVersion != "" {
			detail += " (v" + status.ExtensionVersion + ")"
		}
		fix := ""
		if status.PendingBrowserSessions > 0 {
			detail += fmt.Sprintf("; %d other browser(s) waiting", status.PendingBrowserSessions)
			fix = "run 'papio browser sessions' and 'papio browser use' to switch, or disable the papio extension in browsers you don't use"
		}
		if status.BrowserSessionDenied > 0 {
			// Denied hellos with no live pending session mean another browser
			// competed earlier this daemon run (it may have been pruned).
			detail += fmt.Sprintf("; %d hello(s) denied since daemon start", status.BrowserSessionDenied)
			if fix == "" {
				fix = "another browser competed for the papio session; run 'papio browser sessions' to inspect"
			}
		}
		// Connectivity alone is not health: a connected extension below the
		// daemon's floor holds the session but every handoff will refuse.
		// Name the exact skew here instead of letting the user find it at
		// first use.
		if status.ExtensionVersion != "" && semverLess(status.ExtensionVersion, browser.MinExtensionVersion) {
			add("extension", Warn,
				detail+fmt.Sprintf("; below the daemon's minimum %s — handoffs are refused until it updates", browser.MinExtensionVersion),
				"update the papio extension (store installs update automatically once the new version clears review; unpacked builds: load the new dist/)")
		} else {
			add("extension", Pass, detail, fix)
		}
	} else {
		add("extension", Warn, "extension has not connected since daemon start", "install and enable the browser extension, then run papio init to install the native-host manifest")
	}

	runManifestChecks(cfg, deps, add)
	runHostVersionCheck(ctx, cfg, status.Version, deps, add)

	runLibraryChecks(ctx, cfg, deps, add)

	if strings.TrimSpace(cfg.Zotio.Executable) == "" {
		add("zotio", Skip, "not configured (optional — Zotero import disabled)", "")
		runUpdateChecks(ctx, cfg, deps, nil, add)
		return report
	}
	preflight, err := deps.ZotioPreflight(ctx, cfg)
	if err != nil || preflight == nil {
		detail := "zotio preflight returned no result"
		if err != nil {
			detail = err.Error()
		}
		preflight = nil
		add("zotio", Fail, detail, "install or update zotio, then rerun papio doctor")
	} else {
		add("zotio", Pass, fmt.Sprintf("version %s; %d required capabilities available", preflight.Version, len(preflight.Capabilities)), "")
		update.NewZotio(cfg.DataDir).RememberInstalledVersion(preflight.Version)
	}
	runUpdateChecks(ctx, cfg, deps, preflight, add)
	return report
}

func runUpdateChecks(ctx context.Context, cfg config.Config, deps IntegrationDependencies, preflight *zotio.PreflightResult, add func(string, string, string, string)) {
	if !cfg.Updates.Check {
		add("updates (papio)", Skip, "update check disabled ([updates] check = false)", "")
		add("updates (zotio)", Skip, "update check disabled ([updates] check = false)", "")
		return
	}
	runPapioUpdateCheck(ctx, cfg, deps, add)
	runZotioUpdateCheck(ctx, cfg, deps, preflight, add)
}

func runPapioUpdateCheck(ctx context.Context, cfg config.Config, deps IntegrationDependencies, add func(string, string, string, string)) {
	if deps.CheckUpdates == nil {
		add("updates (papio)", Skip, "skipped: update checker is not configured", "")
		return
	}
	info := deps.CheckUpdates(ctx, cfg)
	if info == nil {
		add("updates (papio)", Warn, "could not check for papio updates", "rerun papio doctor later")
		return
	}
	if !update.IsNewer(info.LatestVersion, deps.CLIVersion) {
		add("updates (papio)", Pass, fmt.Sprintf("papio %s is current", deps.CLIVersion), "")
		return
	}
	executable, err := os.Executable()
	if err != nil {
		executable = ""
	}
	add(
		"updates (papio)",
		Warn,
		fmt.Sprintf("papio %s available (you have %s)", info.LatestVersion, deps.CLIVersion),
		update.UpgradeHint(executable, info.URL),
	)
}

func runZotioUpdateCheck(ctx context.Context, cfg config.Config, deps IntegrationDependencies, preflight *zotio.PreflightResult, add func(string, string, string, string)) {
	if strings.TrimSpace(cfg.Zotio.Executable) == "" {
		add("updates (zotio)", Skip, "skipped: zotio is not configured", "")
		return
	}
	if preflight == nil || strings.TrimSpace(preflight.Version) == "" {
		add("updates (zotio)", Skip, "skipped: zotio preflight failed", "")
		return
	}
	if deps.CheckZotioUpdates == nil {
		add("updates (zotio)", Skip, "skipped: update checker is not configured", "")
		return
	}
	info := deps.CheckZotioUpdates(ctx, cfg)
	if info == nil {
		add("updates (zotio)", Warn, "could not check for zotio updates", "rerun papio doctor later")
		return
	}
	if !update.IsNewer(info.LatestVersion, preflight.Version) {
		add("updates (zotio)", Pass, fmt.Sprintf("zotio %s is current", preflight.Version), "")
		return
	}
	add(
		"updates (zotio)",
		Warn,
		fmt.Sprintf("zotio %s available (you have %s)", info.LatestVersion, preflight.Version),
		update.UpgradeHintFor(cfg.Zotio.Executable, "zotio", info.URL),
	)
}

func integrationDependenciesComplete(deps IntegrationDependencies) bool {
	return deps.CLIVersion != "" && deps.LoadConfig != nil && deps.DaemonStatus != nil && deps.ManifestDir != nil && deps.FirefoxDir != nil && deps.ReadFile != nil && deps.ZotioPreflight != nil
}

// runLibraryChecks reports one check per configured holdings source. It is the
// wire-safe way to surface daemon-side state: a new doctor check adds an element
// to a list an existing result already carries, where widening any RPC result
// would make an older CLI reject every response.
func runLibraryChecks(ctx context.Context, cfg config.Config, deps IntegrationDependencies, add func(string, string, string, string)) {
	if len(cfg.Library.Sources) == 0 {
		add("library", Skip, "not configured (optional — non-Zotero de-duplication disabled)", "")
		return
	}
	if deps.LibrarySources == nil {
		add("library", Skip,
			"configured library sources were not probed because this doctor invocation has no library probe",
			"run 'papio doctor' through the papio CLI; if it persists, reinstall papio")
		return
	}
	statuses, err := deps.LibrarySources(ctx, cfg)
	if err != nil {
		add("library", Fail, "library source probe failed", "check library.sources in the configuration, then rerun papio doctor")
		return
	}
	byName := make(map[string]LibrarySourceStatus, len(statuses))
	for _, status := range statuses {
		byName[status.Name] = status
	}
	for _, source := range cfg.Library.Sources {
		name := "library_source:" + source.Name
		status, ok := byName[source.Name]
		if !ok {
			add(name, Fail, "source was not probed", "rerun papio doctor; if it persists, report it")
			continue
		}
		if !status.Complete {
			detail := "could not be read: " + libraryFailureReason(status.FailureCode)
			// A source that has never loaded cannot suppress anything, which is
			// safe; say so, because the user's real question is "why is nothing
			// being skipped?".
			if status.LastSuccess.IsZero() {
				detail += "; never loaded, so nothing is de-duplicated against it"
			}
			add(name, Fail, detail, "check the path and format of library.sources."+source.Name)
			continue
		}
		entries := "entries"
		if status.EntryCount == 1 {
			entries = "entry"
		}
		detail := fmt.Sprintf("%d %s, claim %s", status.EntryCount, entries, source.Claim)
		if !status.LastSuccess.IsZero() {
			detail += fmt.Sprintf(", read %s ago", time.Since(status.LastSuccess).Round(time.Second))
		}
		if status.EntryCount == 0 {
			// Valid and empty is a real state, distinct from unreadable; flag it
			// as a warning because it is usually a mistargeted path.
			add(name, Warn, detail+"; nothing to match against", "confirm this export is the library you meant")
			continue
		}
		add(name, Pass, detail, "")
	}
}

// libraryFailureReason turns a bounded failure code into something a user can
// act on. The codes stay bounded on purpose: a provider inherits the daemon
// environment, so its raw output can carry credentials and never reaches a
// durable report.
func libraryFailureReason(code string) string {
	switch code {
	case ownership.FailureUnreadable:
		return "the file is missing or unreadable"
	case ownership.FailureParse:
		return "the file could not be parsed; the previous contents are still in use"
	case ownership.FailureTruncated:
		return "the file is larger than papio will read"
	case ownership.FailureCountCollapse:
		return "the entry count collapsed, which usually means a truncated write; the previous contents are still in use"
	case ownership.FailureTimeout, ownership.FailureExit:
		return "the source did not answer in time"
	case ownership.FailureNotConfigured:
		return "the source is not usable as configured"
	case "":
		return "no reason was reported"
	default:
		return code
	}
}

func runManifestChecks(cfg config.Config, deps IntegrationDependencies, add func(string, string, string, string)) {
	runManifestCheck("native host (Chrome)", cfg.Browser.ChromiumExtensionIDs(), "chrome-extension://", cfg, deps.ManifestDir, deps.ReadFile, deps.HostExecutableResolves, add)
	var firefoxIDs []string
	if cfg.Browser.FirefoxExtensionID != "" {
		firefoxIDs = []string{cfg.Browser.FirefoxExtensionID}
	}
	runManifestCheck("native host (Firefox)", firefoxIDs, "", cfg, deps.FirefoxDir, deps.ReadFile, deps.HostExecutableResolves, add)
}

// runHostVersionCheck compares the native-host executable's version to the
// running daemon's. One papio binary is CLI, daemon, AND native host, but the
// native-messaging manifest can point at a different copy than the daemon that
// is running: a brew install beside a ~/.local/bin build, or a symlink an
// upgrade left pointing at the previous release.
//
// That skew is not cosmetic and it was invisible before this check. The host
// enforces its own copy of the transport rules — it validates every frame and
// bounds the browser.sync request itself (internal/ipc/client.go) — so a stale
// host keeps rejecting large page-capture relays, and it treats that rejection
// as fatal: goodbye, and the daemon tears down the live browser session. The
// daemon can be perfectly up to date while every large capture still dies.
func runHostVersionCheck(ctx context.Context, cfg config.Config, daemonVersion string, deps IntegrationDependencies, add func(string, string, string, string)) {
	const name = "native host (version)"
	if deps.HostExecutableVersion == nil {
		add(name, Skip, "skipped: the host version cannot be probed from here", "")
		return
	}
	if len(cfg.Browser.ChromiumExtensionIDs()) == 0 {
		add(name, Skip, "skipped: extension ID is not configured", "")
		return
	}
	dir, err := deps.ManifestDir(cfg)
	if err != nil {
		add(name, Skip, "skipped: the native-host manifest location is unknown", "")
		return
	}
	// A missing, unreadable, or malformed manifest is already reported by the
	// manifest check with its own remediation. Skip quietly instead of adding a
	// second failure for one cause.
	data, err := deps.ReadFile(filepath.Join(dir, nativeHostManifestName+".json"))
	if err != nil {
		add(name, Skip, "skipped: the native-host manifest is unavailable", "")
		return
	}
	var manifest nativeHostManifest
	if err := json.Unmarshal(data, &manifest); err != nil || strings.TrimSpace(manifest.Path) == "" {
		add(name, Skip, "skipped: the native-host manifest does not name an executable", "")
		return
	}
	version, err := deps.HostExecutableVersion(ctx, manifest.Path)
	if err != nil {
		add(name, Warn, "could not read the version of "+manifest.Path+" ("+err.Error()+")",
			"papio native-host install")
		return
	}
	version = strings.TrimSpace(version)
	switch {
	case version == "":
		add(name, Warn, manifest.Path+" reported no version", "papio native-host install")
	case version != daemonVersion:
		add(name, Fail,
			fmt.Sprintf("host %s at %s, daemon %s — browsers spawn the stale binary", version, manifest.Path, daemonVersion),
			"papio native-host install, then kill the running papio-native-host process (the browser respawns it)")
	default:
		add(name, Pass, "matches the daemon ("+version+")", "")
	}
}

const nativeHostManifestName = "com.orgmentem.papio"

type nativeHostManifest struct {
	Path              string   `json:"path"`
	AllowedOrigins    []string `json:"allowed_origins"`
	AllowedExtensions []string `json:"allowed_extensions"`
}

func runManifestCheck(name string, extensionIDs []string, originPrefix string, cfg config.Config, manifestDir func(config.Config) (string, error), readFile func(string) ([]byte, error), resolves func(string) bool, add func(string, string, string, string)) {
	if len(extensionIDs) == 0 {
		add(name, Skip, "skipped: extension ID is not configured", "")
		return
	}
	dir, err := manifestDir(cfg)
	if err != nil {
		add(name, Fail, err.Error(), "register the native host manually for this browser")
		return
	}
	path := filepath.Join(dir, nativeHostManifestName+".json")
	data, err := readFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			add(name, Fail, "manifest is missing at "+path, "papio native-host install")
			return
		}
		add(name, Fail, fmt.Sprintf("reading manifest %s: %v", path, err), "papio native-host install")
		return
	}
	var manifest nativeHostManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		add(name, Fail, fmt.Sprintf("parsing manifest %s: %v", path, err), "papio native-host install")
		return
	}
	for _, extensionID := range extensionIDs {
		if originPrefix != "" {
			allowedOrigin := originPrefix + extensionID + "/"
			if !containsString(manifest.AllowedOrigins, allowedOrigin) {
				add(name, Fail, "manifest does not allow "+allowedOrigin, "papio native-host install")
				return
			}
		} else if !containsString(manifest.AllowedExtensions, extensionID) {
			add(name, Fail, "manifest does not allow "+extensionID, "papio native-host install")
			return
		}
	}
	if resolves != nil && manifest.Path != "" && !resolves(manifest.Path) {
		add(name, Fail, "native-host executable is missing at "+manifest.Path+" (dangling symlink? a package upgrade may have removed it)", "papio native-host install")
		return
	}
	add(name, Pass, "manifest allows configured extension", "")
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// semverLess reports a < b for dotted numeric versions (pre-release/build
// suffixes are ignored segment-wise; malformed segments compare as 0). Kept
// deliberately simple: it guards a WARN, not an enforcement decision — the
// bridge owns the authoritative floor check.
func semverLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		av, bv := 0, 0
		if i < len(as) {
			if _, err := fmt.Sscanf(strings.TrimSpace(as[i]), "%d", &av); err != nil {
				av = 0
			}
		}
		if i < len(bs) {
			if _, err := fmt.Sscanf(strings.TrimSpace(bs[i]), "%d", &bv); err != nil {
				bv = 0
			}
		}
		if av != bv {
			return av < bv
		}
	}
	return false
}
