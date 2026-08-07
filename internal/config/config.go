// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package config loads ~/.config/papio/config.toml. The access mode is an
// explicit first-run choice: acquisition refuses to run without one (no silent
// automation default). Every job snapshots the policy it ran under.
package config

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"papio/internal/bibparse"

	toml "github.com/pelletier/go-toml/v2"
)

// Access modes (stack plan "Access profiles").
const (
	ModeConservative = "conservative"
	ModeAssisted     = "assisted"
	ModeDelegated    = "delegated"
)

// Source names used across config, budgets, and resolver registry.
const (
	SourceArXiv            = "arxiv"
	SourceEuropePMC        = "europepmc"
	SourceUnpaywall        = "unpaywall"
	SourceOpenAlex         = "openalex"
	SourceCORE             = "core"
	SourceCrossrefTDM      = "crossref_tdm"
	SourceCrossrefMetadata = "crossref_metadata"
	SourceRetractionWatch  = "retraction_watch"
	SourceSemanticScholar  = "semanticscholar"
	SourceOpenAIRE         = "openaire"
)

// validSourceNames is the exhaustive set of [sources.*] keys papio
// understands, kept adjacent to the constants above so it cannot drift from
// them. validate() uses it to fail closed on a misspelled or reserved-but-
// unimplemented source name instead of silently ignoring it (see the
// discovery.sources and validateLibrary rationale below).
var validSourceNames = map[string]bool{
	SourceArXiv:            true,
	SourceEuropePMC:        true,
	SourceUnpaywall:        true,
	SourceOpenAlex:         true,
	SourceCORE:             true,
	SourceCrossrefTDM:      true,
	SourceCrossrefMetadata: true,
	SourceRetractionWatch:  true,
	SourceSemanticScholar:  true,
	SourceOpenAIRE:         true,
}

// removedSourceNames are names papio shipped in Default() at some point and no
// longer implements. Every config written by `papio init` or `papio config
// save` before the removal lists them, so rejecting one would make the whole
// file unparseable on upgrade — for a name that was already an inert no-op.
// They are therefore accepted and dropped on load, which preserves behaviour
// exactly and lets the next `papio config save` rewrite the file without them.
// Unknown names that were never ours stay a hard error: a silently ignored
// typo suppresses an acquisition route the user asked for.
var removedSourceNames = map[string]bool{
	// Reserved end-to-end (const, defaults row, resolver priority rank, docs)
	// but no adapter was ever written, so enabling it never did anything.
	"openalex_content": true,
}

// validSourceNamesList renders validSourceNames for error messages, in the
// same order as the const block above.
const validSourceNamesList = "arxiv, europepmc, unpaywall, openalex, core, crossref_tdm, crossref_metadata, retraction_watch, semanticscholar, openaire"

// Source is one resolver's policy knobs.
type Source struct {
	Enabled       bool    `toml:"enabled"`
	APIKey        string  `toml:"api_key,omitempty"`
	RatePerSec    float64 `toml:"rate_per_sec,omitempty"`
	Burst         int     `toml:"burst,omitempty"`
	MaxCostUSD    float64 `toml:"max_cost_usd,omitempty"`     // monthly budget for paid sources
	BaseURLForDev string  `toml:"base_url_for_dev,omitempty"` // test/dev override; loopback only
}

// Fetch bounds every artifact download.
type Fetch struct {
	MaxBytes          int64 `toml:"max_bytes"`
	TimeoutSeconds    int   `toml:"timeout_seconds"`
	AllowHTTPLoopback bool  `toml:"allow_http_loopback,omitempty"` // tests/dev only
}

// PDF controls validation and OCR fallback.
type PDF struct {
	OCREnabled          bool    `toml:"ocr_enabled"`
	MinTextChars        int     `toml:"min_text_chars"`
	MaxOCRPages         int     `toml:"max_ocr_pages"`
	TitleMatchThreshold float64 `toml:"title_match_threshold"`
}

// Browser configures the Phase 2 ordinary-Chrome institutional handoff.
// Zero values disable the browser path entirely: no extension ID means the
// native host rejects every origin, and no OpenURL base means jobs never
// route to institutional access.
type Browser struct {
	// ExtensionID is the fixed Chrome extension ID allowed to talk to the
	// native host (32 chars, a-p). Empty disables the bridge.
	ExtensionID string `toml:"extension_id,omitempty"`
	// ExtensionIDs are additional Chrome-family (Chromium) extension IDs
	// allowed to reach the native host alongside ExtensionID — e.g. an Edge
	// Add-ons store copy or a second keyed build, which carry different IDs
	// than the Chrome Web Store package. Each is 32 chars a-p.
	ExtensionIDs []string `toml:"extension_ids,omitempty"`
	// FirefoxExtensionID is the Gecko add-on ID allowed to reach the native
	// host. Empty disables the Firefox bridge.
	FirefoxExtensionID string `toml:"firefox_extension_id,omitempty"`
	// OpenURLBase is the default institution's OpenURL resolver base (https).
	OpenURLBase string `toml:"openurl_base_url,omitempty"`
	// ShibbolethEntityID is the default institution's Shibboleth IdP entityID;
	// empty disables federated login-routing.
	ShibbolethEntityID string `toml:"shibboleth_entity_id,omitempty"`
	// ProquestAccountID is the institution's ProQuest account id used to unlock
	// ProQuest's OpenURL link-resolver; empty disables.
	ProquestAccountID string `toml:"proquest_account_id,omitempty"`
	// LibKeyMode selects the default institution's LibKey routing: "" or
	// "off" disables it; "link" routes DOI/PMID handoffs through the
	// documented keyless LibKey.io institution link ahead of the bare
	// OpenURL resolver (ADR-0016; api mode is not implemented yet).
	LibKeyMode string `toml:"libkey_mode,omitempty"`
	// LibKeyLibraryID is the default institution's numeric Third Iron
	// library id, required when LibKeyMode is "link". It appears in the
	// institution's BrowZine/LibKey.io URL (…/libraries/<id>/…).
	LibKeyLibraryID int64 `toml:"libkey_library_id,omitempty"`
	// Resolvers contains named institutional access profiles. Each named
	// profile carries its own OpenURL base and optional federated-login
	// identity, so a multi-institution user routes each job to the right
	// library. The top-level OpenURLBase / ShibbolethEntityID /
	// ProquestAccountID fields above are the implicit "default" profile.
	Resolvers map[string]Institution `toml:"resolvers,omitempty"`
	// DefaultResolver selects the named browser resolver profile when a request
	// omits resolver. Empty preserves the historical default institution. Older
	// strict-mode daemons reject this unknown field, so config writers should
	// only emit it when talking to a daemon that supports resolver profiles.
	DefaultResolver string `toml:"default_resolver,omitempty"`
	// AdoptionRoot is the directory Chrome downloads into for adoption;
	// the daemon rejects reported paths outside <root>/<job_id>/.
	// Default: <data_dir>/adoptions.
	AdoptionRoot string `toml:"download_adoption_root,omitempty"`
	// ActionExpirySeconds sets browser-offer expiry and the first human-action
	// reminder threshold. Subsequent reminders back off independently per action.
	ActionExpirySeconds int `toml:"action_expiry_seconds,omitempty"`
	// DocumentDelivery configures the default institution's document
	// delivery / ILL route (ADR-0017 Decision 2). Nil disables it — the
	// job falls back to the profile's plain OpenURL route.
	DocumentDelivery *DocumentDelivery `toml:"document_delivery,omitempty"`
}

// ChromiumExtensionIDs returns the deduplicated Chrome-family extension IDs
// allowed to reach the native host: the primary ExtensionID first, then any
// additional ExtensionIDs. Empty entries are dropped. Every Chromium browser
// (Chrome, Edge, Vivaldi, Brave, Opera, …) shares this same allowlist.
func (b Browser) ChromiumExtensionIDs() []string {
	seen := make(map[string]bool)
	ids := make([]string, 0, 1+len(b.ExtensionIDs))
	for _, id := range append([]string{b.ExtensionID}, b.ExtensionIDs...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// Institution is one library's institutional-access identity: its OpenURL
// link-resolver base plus the optional Shibboleth entityID and ProQuest
// account id used to auto-route provider login walls without a manual WAYF
// selection. Named institutions live in Browser.Resolvers; the default
// institution is expressed by the top-level Browser fields.
type Institution struct {
	// OpenURLBase is the institution's OpenURL resolver base (https).
	OpenURLBase string `toml:"openurl_base_url"`
	// ShibbolethEntityID is the institution's Shibboleth IdP entityID; empty
	// disables federated login-routing for this profile.
	ShibbolethEntityID string `toml:"shibboleth_entity_id,omitempty"`
	// ProquestAccountID unlocks this institution's ProQuest link-resolver;
	// empty disables the accountid append for this profile.
	ProquestAccountID string `toml:"proquest_account_id,omitempty"`
	// LibKeyMode selects this profile's LibKey routing: "" or "off"
	// disables it; "link" routes DOI/PMID handoffs through the keyless
	// LibKey.io institution link ahead of the bare OpenURL resolver
	// (ADR-0016; api mode is not implemented yet).
	LibKeyMode string `toml:"libkey_mode,omitempty"`
	// LibKeyLibraryID is this profile's numeric Third Iron library id,
	// required when LibKeyMode is "link".
	LibKeyLibraryID int64 `toml:"libkey_library_id,omitempty"`
	// DocumentDelivery configures this profile's document delivery / ILL
	// route (ADR-0017 Decision 2). Nil disables it — the job falls back to
	// this profile's plain OpenURL route.
	DocumentDelivery *DocumentDelivery `toml:"document_delivery,omitempty"`
}

// UnmarshalText lets a resolver profile be written as a bare OpenURL base
// string — the shorthand `name = "https://…"` — in addition to a full
// institution table. go-toml routes scalar string values here and decodes
// tables through the struct fields normally (including DisallowUnknownFields),
// so the string form keeps pre-existing single-base resolver configs loading
// without a migration while the table form adds the per-profile login identity.
func (i *Institution) UnmarshalText(text []byte) error {
	i.OpenURLBase = string(text)
	return nil
}

// DocumentDelivery configures one institution profile's document delivery /
// ILL route: an OpenURL request form, a LibKey delivery route, or an
// institution-issued API integration (ADR-0017 Decision 2). Every credential
// here is read only by internal/delivery in the daemon — never sent to,
// stored in, or observable from the extension or the browser wire, the same
// boundary ADR-0013 draws for browser-local vs. daemon-owned state.
type DocumentDelivery struct {
	// Kind selects the delivery adapter: openurl | libkey | illiad | custom.
	// Required — papio never guesses which ILL system an institution runs
	// (ADR-0017 Decision 2).
	Kind string `toml:"kind,omitempty"`
	// BaseURL is the request form or API base (https), used for kind =
	// openurl/custom and as the ILLiad Web Platform base for kind = illiad.
	BaseURL string `toml:"base_url,omitempty"`
	// AllowedHosts restricts which hosts a prefilled request form or API
	// base may reach.
	AllowedHosts []string `toml:"allowed_hosts,omitempty"`
	// SubmitPolicy narrows how a request may be created: never (default,
	// when empty) | prefill_only | auto_if_unconditional. It narrows what
	// the global access_mode permits, never widens it (ADR-0017 Decision 3B
	// condition 1).
	SubmitPolicy string `toml:"submit_policy,omitempty"`
	// RequestClasses lists the request classes this profile is declared
	// for. v1 recognizes only digital_journal_article (ADR-0017 Decision
	// 3A).
	RequestClasses []string `toml:"request_classes,omitempty"`
	// LegalBasis is the operator's declared legal basis for a request:
	// institution_policy | copyright_act_s49 | unknown (empty = unknown).
	// Configured, never inferred from a hostname (ADR-0017 Decision 3A).
	LegalBasis string `toml:"legal_basis,omitempty"`
	// PatronAttestation declares whether a per-request patron attestation
	// is required: not_required | standing_completed | per_request |
	// unknown (empty = unknown). standing_completed counts only when the
	// institution has confirmed a registration-time agreement covers this
	// request class — papio never infers it (ADR-0017 Decision 2).
	PatronAttestation string `toml:"patron_attestation,omitempty"`
	// PatronFeePolicy declares whether the patron is charged: zero_standard
	// | per_request | unknown (empty = unknown). Only zero_standard can
	// ever compile auto_capable (ADR-0017 Decision 3A).
	PatronFeePolicy string `toml:"patron_fee_policy,omitempty"`
	// MonthlyRequestCap bounds auto-submitted requests per month; 0 means
	// no cap declared.
	MonthlyRequestCap int `toml:"monthly_request_cap,omitempty"`
	// StatusPollMinutes sets the delivery status poll cadence; 0 uses
	// internal/delivery's default.
	StatusPollMinutes int `toml:"status_poll_minutes,omitempty"`
	// APIKey is the institution-issued application credential, permitted
	// only for kind = illiad — a key on a form-kind profile is dead config.
	// Read only by internal/delivery in the daemon (ADR-0017 Decision 2).
	// 0600 config only.
	APIKey string `toml:"api_key,omitempty"`
	// PatronRef is a configured, non-secret patron reference used to map
	// papio's requests to the institution's system. Not a secret, but
	// personal identity data: 0600 config only, redacted from events,
	// diagnostics, and delivery provenance (ADR-0017 Decision 2).
	PatronRef string `toml:"patron_ref,omitempty"`
}

// Zotio configures the credential-owning Zotero CLI boundary. papio invokes
// this executable but never reads or stores Zotero credentials itself.
type Zotio struct {
	Executable     string `toml:"executable"`
	TimeoutSeconds int    `toml:"timeout_seconds"`
	AttachmentMode string `toml:"attachment_mode"`
	AutoImport     bool   `toml:"auto_import"`
	AutoEnrich     bool   `toml:"auto_enrich"`
	// ExceptionTags enables the reconciled Zotero exception-tag ledger:
	// papio:needs-action / papio:unavailable written as automatic tags on
	// linked items in the user's personal library. Off by default because it
	// mutates the user's library beyond attaching requested PDFs.
	ExceptionTags bool `toml:"exception_tags"`
	// UnavailableRecheckDays is how long an unavailable outcome parks an item
	// before backfill re-checks it (OA availability drifts upward). Range
	// 1..365, default 14.
	UnavailableRecheckDays int `toml:"unavailable_recheck_days"`
}

// Captures keeps browser diagnostics in papio's own data directory so fixture
// investigation does not scatter unexplained files through Downloads. Config
// decoding is strict, so a config with this new table must deploy with a binary
// that understands it; an older binary otherwise rejects every command.
type Captures struct {
	Enabled bool `toml:"enabled"`
	// MaxPerHost keeps one broken provider page from crowding out every other
	// diagnostic. Range 1..1000, default 10.
	MaxPerHost int `toml:"max_per_host"`
	// MaxAgeDays prevents diagnostic fixtures from silently consuming the
	// volume long after they stop helping. Range 1..365, default 14.
	MaxAgeDays int `toml:"max_age_days"`
}

// Notify configures best-effort notifications from the daemon: local desktop
// notifications and an optional remote webhook. Both are fire-and-forget; a
// delivery failure never fails the work that triggered it.
type Notify struct {
	Enabled bool `toml:"enabled"`
	// WebhookURL, when set, receives every notification as a JSON POST in
	// addition to (not instead of) the local desktop channel.
	WebhookURL string `toml:"webhook_url"`
	// WebhookSecret, when set, is sent as "Authorization: Bearer <secret>".
	WebhookSecret string `toml:"webhook_secret"`
}

// Hooks configures best-effort local commands the daemon runs at job
// lifecycle points. Hooks are fire-and-forget: a failing hook never fails
// the job that triggered it, and papio never retries a hook.
type Hooks struct {
	// OnReady, when set, runs once via the system shell each time a job
	// reaches the ready state (validated artifact). Empty disables it.
	OnReady string `toml:"on_ready"`
	// TimeoutSeconds bounds one hook run. Default 120, range 5..600.
	TimeoutSeconds int `toml:"timeout_seconds"`
}

// Library declares the libraries papio may consult to answer "do I already hold
// this paper?" for users who do not run Zotero. See ADR-0008: a source emits
// only positive holdings claims, and a source papio cannot read makes the answer
// *incomplete* rather than negative — suppressing an acquisition on a failed
// lookup would silently withhold a paper the user asked for.
type Library struct {
	Sources []LibrarySource `toml:"sources"`
}

// LibrarySource is one holdings feed.
type LibrarySource struct {
	// Name identifies the source in health reporting and doctor output. It must
	// be unique: it is the only handle a user has on one feed among several.
	Name string `toml:"name"`
	// Kind is "file" — the only v1 loader. A command loader and a PDF-folder
	// scanner are planned (ADR-0008), so the field exists to keep the config
	// shape final; unknown kinds are rejected rather than ignored.
	Kind string `toml:"kind"`
	// Path is the bibliographic export to read, for kind = "file".
	Path string `toml:"path"`
	// Format names the encoding: bibtex, ris, csl-json, or nbib. Empty means
	// detect from the path and content.
	Format string `toml:"format"`
	// Claim is what this source asserts about every record it emits:
	// "pdf_present" (entries whose full text you hold, so a match may suppress
	// acquisition) or "record_present" (citations only, which may annotate search
	// results but must never suppress). There is deliberately no default:
	// guessing would let papio skip acquisitions a source never vouched for. papio
	// also does not infer this from per-manager attachment fields (BibTeX `file`,
	// papis `files`, CSL `note`) — that is manager convention knowledge this
	// abstraction must not carry, and CSL `note` is free text, not a contract.
	Claim string `toml:"claim"`
}

// Claim values for LibrarySource.Claim.
const (
	LibraryClaimPDFPresent    = "pdf_present"
	LibraryClaimRecordPresent = "record_present"
)

// LibraryKindFile is the only source kind v1 implements.
const LibraryKindFile = "file"

// MaxLibrarySources bounds per-lookup work: every configured source is consulted
// on every search, batch, and watch pass.
const MaxLibrarySources = 8

// Discovery selects which discovery backends serve search and watches, in
// merge-preference order. Empty means OpenAlex only (the historical default).
// Per-backend API keys and dev base URLs live in the existing [sources] map
// under the same name (e.g. sources.semanticscholar.api_key).
type Discovery struct {
	Sources []string `toml:"sources"`
}

// Updates configures the optional daily release check.
type Updates struct {
	Check bool `toml:"check"`
}

// Actions configures how long an open human action may wait before listings
// call it stale.
//
// Separate from browser.action_expiry_seconds on purpose: that value (default
// 30 minutes) is a REMINDER cadence — "nudge me about this again" — and reusing
// it as a staleness threshold would report a handoff queued over lunch as
// abandoned. This one answers "has anyone given up on this?", so its scale is
// days.
type Actions struct {
	// StaleAfterSeconds marks an open action stale once it has waited this
	// long. 0 selects the default; a negative value is rejected. Marking is all
	// it does: nothing expires, cancels, or is swept as a consequence, because
	// abandoning an acquisition is a human's call, not a timer's.
	StaleAfterSeconds int `toml:"stale_after_seconds,omitempty"`
}

// DefaultActionStaleAfterSeconds is a week: long enough that an ordinary
// weekday-to-weekday gap, a conference, or a library ticket in flight does not
// look abandoned, short enough that a handoff nobody ever opened stops hiding
// among the fresh ones.
const DefaultActionStaleAfterSeconds = 7 * 24 * 60 * 60

// EffectiveActionStaleAfter resolves the staleness threshold the way every
// reader must apply it, so a listing and a consumer reading the same config
// cannot disagree about which rows are stale.
func (a Actions) EffectiveActionStaleAfter() time.Duration {
	if a.StaleAfterSeconds <= 0 {
		return DefaultActionStaleAfterSeconds * time.Second
	}
	return time.Duration(a.StaleAfterSeconds) * time.Second
}

// Config is the loaded, validated configuration.
type Config struct {
	AccessMode string            `toml:"access_mode"`
	Email      string            `toml:"email"`
	DataDir    string            `toml:"data_dir"`
	Fetch      Fetch             `toml:"fetch"`
	PDF        PDF               `toml:"pdf"`
	Browser    Browser           `toml:"browser"`
	Captures   Captures          `toml:"captures"`
	Zotio      Zotio             `toml:"zotio"`
	Notify     Notify            `toml:"notify"`
	Hooks      Hooks             `toml:"hooks"`
	Library    Library           `toml:"library"`
	Updates    Updates           `toml:"updates"`
	Discovery  Discovery         `toml:"discovery"`
	Actions    Actions           `toml:"actions"`
	Sources    map[string]Source `toml:"sources"`

	// Path this config was loaded from ("" for defaults).
	Path string `toml:"-"`
}

// LibraryFingerprint identifies the normalized generic holdings sources. It is
// stable across declaration order and changes whenever a source's semantics do.
// An empty library has no generic source fingerprint.
func (c Config) LibraryFingerprint() string {
	if len(c.Library.Sources) == 0 {
		return ""
	}

	sources := slices.Clone(c.Library.Sources)
	slices.SortFunc(sources, func(a, b LibrarySource) int {
		return strings.Compare(a.Name, b.Name)
	})

	hash := sha256.New()
	var length [8]byte
	for _, source := range sources {
		for _, value := range [...]string{
			source.Name,
			source.Kind,
			normalizeLibrarySourcePath(source.Path),
			source.Format,
			source.Claim,
		} {
			binary.BigEndian.PutUint64(length[:], uint64(len(value)))
			_, _ = hash.Write(length[:])
			_, _ = hash.Write([]byte(value))
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Dir returns the papio config directory, honoring PAPIO_CONFIG_DIR for tests.
func Dir() string {
	if d := os.Getenv("PAPIO_CONFIG_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "papio")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".config", "papio")
}

// defaultDataDir is the baseline data directory: %LOCALAPPDATA%\papio on Windows
// (non-roaming, the right home for a database), ~/.local/share/papio elsewhere.
func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			return filepath.Join(localAppData, "papio")
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "papio")
}

// Default returns the baseline configuration. AccessMode is deliberately empty:
// callers that acquire must see ErrAccessModeUnset until the user chooses.
func Default() Config {
	return Config{
		DataDir:  defaultDataDir(),
		Fetch:    Fetch{MaxBytes: 100 << 20, TimeoutSeconds: 120},
		PDF:      PDF{OCREnabled: true, MinTextChars: 400, MaxOCRPages: 4, TitleMatchThreshold: 0.6},
		Browser:  Browser{ActionExpirySeconds: 1800},
		Captures: Captures{Enabled: true, MaxPerHost: 10, MaxAgeDays: 14},
		Actions:  Actions{StaleAfterSeconds: DefaultActionStaleAfterSeconds},
		Zotio:    Zotio{Executable: "zotio", TimeoutSeconds: 120, AttachmentMode: "stored", AutoImport: false, AutoEnrich: true, UnavailableRecheckDays: 14},
		Notify:   Notify{Enabled: true},
		Hooks:    Hooks{TimeoutSeconds: 120},
		Sources: map[string]Source{
			SourceArXiv:            {Enabled: true, RatePerSec: 1, Burst: 1},
			SourceEuropePMC:        {Enabled: true, RatePerSec: 2, Burst: 2},
			SourceUnpaywall:        {Enabled: true, RatePerSec: 1, Burst: 1},
			SourceOpenAlex:         {Enabled: false, RatePerSec: 2, Burst: 2},
			SourceCORE:             {Enabled: false, RatePerSec: 0.4, Burst: 1},
			SourceCrossrefTDM:      {Enabled: false, RatePerSec: 1, Burst: 1},
			SourceCrossrefMetadata: {Enabled: true, RatePerSec: 1, Burst: 1},
			SourceRetractionWatch:  {Enabled: true, RatePerSec: 1, Burst: 1},
			SourceSemanticScholar:  {Enabled: true, RatePerSec: 1, Burst: 1},
			// OpenAIRE's keyless public limit is 60 requests/hour; a personal
			// token (api_key) raises the ceiling and can justify a higher
			// rate_per_sec in the user's config.
			SourceOpenAIRE: {Enabled: true, RatePerSec: 0.016, Burst: 1},
		},
	}
}

// ErrAccessModeUnset is returned by RequireAccessMode until first-run setup.
type ErrAccessModeUnset struct{ Path string }

func (e *ErrAccessModeUnset) Error() string {
	return fmt.Sprintf("access_mode is not set in %s: choose conservative, assisted, or delegated (explicit first-run decision; no silent automation default)", e.Path)
}

// Load reads path (or the default location when path is empty), layering the
// file over Default(). A missing file yields defaults with AccessMode unset.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = filepath.Join(Dir(), "config.toml")
	}
	cfg.Path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config %s: %w", path, err)
	}
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var missing *toml.StrictMissingError
		if errors.As(err, &missing) {
			fields := make([]string, 0, len(missing.Errors))
			for _, decodeErr := range missing.Errors {
				fields = append(fields, strings.Join(decodeErr.Key(), "."))
			}
			return cfg, fmt.Errorf("config %s contains fields this papio build does not recognize (%s). This usually means the config was written for a newer papio — update papio, or remove those fields: %w", path, strings.Join(fields, ", "), err)
		}
		return cfg, fmt.Errorf("parsing config %s (unknown fields are rejected): %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	for name := range removedSourceNames {
		delete(cfg.Sources, name)
	}
	cfg.DataDir = expandHome(cfg.DataDir)
	cfg.Browser.AdoptionRoot = expandHome(cfg.Browser.AdoptionRoot)
	cfg.Zotio.Executable = expandHome(cfg.Zotio.Executable)
	return cfg, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func normalizeLibrarySourcePath(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	path = expandHome(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return path
}

func (c *Config) validate() error {
	switch c.AccessMode {
	case "", ModeConservative, ModeAssisted, ModeDelegated:
	default:
		return fmt.Errorf("invalid access_mode %q (conservative, assisted, or delegated)", c.AccessMode)
	}
	if c.Fetch.MaxBytes < 1<<20 {
		return fmt.Errorf("fetch.max_bytes %d below 1 MiB floor", c.Fetch.MaxBytes)
	}
	if c.Fetch.TimeoutSeconds < 5 {
		return fmt.Errorf("fetch.timeout_seconds %d below 5s floor", c.Fetch.TimeoutSeconds)
	}
	if c.PDF.TitleMatchThreshold <= 0 || c.PDF.TitleMatchThreshold > 1 {
		return fmt.Errorf("pdf.title_match_threshold must be in (0,1]")
	}
	if c.Browser.ExtensionID != "" && !extensionIDRE.MatchString(c.Browser.ExtensionID) {
		return fmt.Errorf("browser.extension_id must be 32 chars a-p")
	}
	for _, id := range c.Browser.ExtensionIDs {
		if !extensionIDRE.MatchString(id) {
			return fmt.Errorf("browser.extension_ids entries must each be 32 chars a-p")
		}
	}
	if c.Browser.FirefoxExtensionID != "" && !firefoxExtensionIDRE.MatchString(c.Browser.FirefoxExtensionID) {
		return fmt.Errorf("browser.firefox_extension_id must be a Gecko email-like ID or braced GUID")
	}
	if c.Browser.OpenURLBase != "" {
		if err := validateOpenURLBase(c.Browser.OpenURLBase); err != nil {
			return fmt.Errorf("browser.openurl_base_url %w", err)
		}
	}
	if c.Browser.ShibbolethEntityID != "" {
		if err := validateOpenURLBase(c.Browser.ShibbolethEntityID); err != nil {
			return fmt.Errorf("browser.shibboleth_entity_id %w", err)
		}
	}
	if c.Browser.ProquestAccountID != "" && (len(c.Browser.ProquestAccountID) > 64 || !proquestAccountIDRE.MatchString(c.Browser.ProquestAccountID)) {
		return fmt.Errorf("browser.proquest_account_id must be digits (max 64)")
	}
	if err := validateLibKey(c.Browser.LibKeyMode, c.Browser.LibKeyLibraryID); err != nil {
		return fmt.Errorf("browser.%w", err)
	}
	if c.Browser.LibKeyMode == "link" && strings.TrimSpace(c.Browser.OpenURLBase) == "" {
		// Without a resolver base the default profile never opens an
		// institutional handoff, so a LibKey route configured here would sit
		// validated but unreachable — the silently-dead-config shape again.
		return fmt.Errorf("browser.libkey_mode \"link\" requires browser.openurl_base_url: LibKey augments the institutional route, and without a resolver base the default profile opens no handoff to route")
	}
	if err := validateDocumentDelivery("browser.document_delivery.", c.Browser.DocumentDelivery); err != nil {
		return err
	}
	for name, inst := range c.Browser.Resolvers {
		// "default" is the implicit top-level institution, not a valid map key:
		// InstitutionFor resolves name == "default" to the top-level Browser
		// fields (OpenURLBase, ShibbolethEntityID, ProquestAccountID) before
		// any map lookup, so a profile under this key can never be reached —
		// it is silently dead and, when the top-level fields are also populated,
		// causes ResolverNames() to duplicate "default" and
		// ResolverProfileForOrigin to misroute jobs.
		if name == "default" {
			return fmt.Errorf("browser.resolvers.default is a reserved name (use the top-level browser.openurl_base_url, browser.shibboleth_entity_id, and browser.proquest_account_id fields for the default institution)")
		}
		if !resolverNameRE.MatchString(name) {
			return fmt.Errorf("browser.resolvers.%s name must be lowercase alphanumeric", name)
		}
		if err := validateOpenURLBase(inst.OpenURLBase); err != nil {
			return fmt.Errorf("browser.resolvers.%s.openurl_base_url %w", name, err)
		}
		if inst.ShibbolethEntityID != "" {
			if err := validateOpenURLBase(inst.ShibbolethEntityID); err != nil {
				return fmt.Errorf("browser.resolvers.%s.shibboleth_entity_id %w", name, err)
			}
		}
		if inst.ProquestAccountID != "" && (len(inst.ProquestAccountID) > 64 || !proquestAccountIDRE.MatchString(inst.ProquestAccountID)) {
			return fmt.Errorf("browser.resolvers.%s.proquest_account_id must be digits (max 64)", name)
		}
		if err := validateLibKey(inst.LibKeyMode, inst.LibKeyLibraryID); err != nil {
			return fmt.Errorf("browser.resolvers.%s.%w", name, err)
		}
		if err := validateDocumentDelivery(fmt.Sprintf("browser.resolvers.%s.document_delivery.", name), inst.DocumentDelivery); err != nil {
			return err
		}
	}
	if defaultResolver := strings.TrimSpace(c.Browser.DefaultResolver); defaultResolver != "" {
		if _, ok := c.InstitutionFor(defaultResolver); !ok {
			names := c.ResolverNames()
			if len(names) == 0 {
				return fmt.Errorf("browser.default_resolver %q is not configured (configured profiles: none)", defaultResolver)
			}
			return fmt.Errorf("browser.default_resolver %q is not configured (configured profiles: %s)", defaultResolver, strings.Join(names, ", "))
		}
		c.Browser.DefaultResolver = defaultResolver
	}
	if c.Browser.ActionExpirySeconds < 0 {
		return fmt.Errorf("browser.action_expiry_seconds must be >= 0")
	}
	if c.Actions.StaleAfterSeconds < 0 {
		return fmt.Errorf("actions.stale_after_seconds must be >= 0")
	}
	if c.Captures.MaxPerHost < 1 || c.Captures.MaxPerHost > 1000 {
		return fmt.Errorf("captures.max_per_host must be in 1..1000")
	}
	if c.Captures.MaxAgeDays < 1 || c.Captures.MaxAgeDays > 365 {
		return fmt.Errorf("captures.max_age_days must be in 1..365")
	}
	if strings.TrimSpace(c.Zotio.Executable) == "" && c.Zotio.AutoImport {
		return fmt.Errorf("zotio.auto_import requires zotio.executable")
	}
	if c.Zotio.TimeoutSeconds < 5 || c.Zotio.TimeoutSeconds > 600 {
		return fmt.Errorf("zotio.timeout_seconds must be in 5..600")
	}
	if c.Zotio.AttachmentMode != "stored" && c.Zotio.AttachmentMode != "linked-file" {
		return fmt.Errorf("zotio.attachment_mode must be stored or linked-file")
	}
	if c.Zotio.UnavailableRecheckDays < 1 || c.Zotio.UnavailableRecheckDays > 365 {
		return fmt.Errorf("zotio.unavailable_recheck_days must be in 1..365")
	}
	if strings.TrimSpace(c.Zotio.Executable) == "" && c.Zotio.ExceptionTags {
		return fmt.Errorf("zotio.exception_tags requires zotio.executable")
	}
	for name, s := range c.Sources {
		// A misspelled or reserved-but-unimplemented [sources.*] name is
		// otherwise accepted and silently does nothing, suppressing an
		// acquisition route the user asked for — the same failure mode
		// validateLibrary's doc comment argues must be a startup error.
		if !validSourceNames[name] && !removedSourceNames[name] {
			return fmt.Errorf("sources.%s is not a recognized source name (valid names: %s)", name, validSourceNamesList)
		}
		if s.BaseURLForDev != "" && !strings.HasPrefix(s.BaseURLForDev, "http://127.0.0.1") && !strings.HasPrefix(s.BaseURLForDev, "http://localhost") {
			return fmt.Errorf("sources.%s.base_url_for_dev must be loopback", name)
		}
		// A negative value does not throttle harder, it removes the throttle:
		// takeToken treats rate <= 0 as unlimited and reserve treats a limit
		// <= 0 as unmetered. So a typed minus sign silently deletes the exact
		// protection it appears to configure, and the operator has no way to
		// tell from the outside. Zero keeps its documented meaning — no
		// pacing, no ceiling — because that is a deliberate choice someone can
		// state; a negative number never is.
		if s.RatePerSec < 0 {
			return fmt.Errorf("sources.%s.rate_per_sec must not be negative (0 disables pacing)", name)
		}
		if s.Burst < 0 {
			return fmt.Errorf("sources.%s.burst must not be negative", name)
		}
		if s.MaxCostUSD < 0 {
			return fmt.Errorf("sources.%s.max_cost_usd must not be negative (0 means unmetered)", name)
		}
	}
	if c.Notify.WebhookURL != "" {
		u, err := url.Parse(c.Notify.WebhookURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("notify.webhook_url must be an absolute http(s) URL")
		}
	}
	if c.Notify.WebhookSecret != "" && c.Notify.WebhookURL == "" {
		return fmt.Errorf("notify.webhook_secret is set but notify.webhook_url is empty")
	}
	if c.Hooks.OnReady != "" && (c.Hooks.TimeoutSeconds < 5 || c.Hooks.TimeoutSeconds > 600) {
		return fmt.Errorf("hooks.timeout_seconds must be in 5..600")
	}
	if err := c.validateLibrary(); err != nil {
		return err
	}
	seenDiscovery := map[string]bool{}
	for _, name := range c.Discovery.Sources {
		if name != SourceOpenAlex && name != SourceSemanticScholar {
			return fmt.Errorf("discovery.sources entry %q must be %s or %s", name, SourceOpenAlex, SourceSemanticScholar)
		}
		if seenDiscovery[name] {
			return fmt.Errorf("discovery.sources lists %q twice", name)
		}
		seenDiscovery[name] = true
	}
	return nil
}

// validateLibrary is fail-closed on every field. A misconfigured holdings source
// must be a startup error, not a silently degraded one: the failure mode of a
// source papio half-understands is suppressing an acquisition the user wanted.
func (c *Config) validateLibrary() error {
	if len(c.Library.Sources) > MaxLibrarySources {
		return fmt.Errorf("library.sources lists %d sources, maximum %d", len(c.Library.Sources), MaxLibrarySources)
	}
	seen := make(map[string]bool, len(c.Library.Sources))
	for i, source := range c.Library.Sources {
		if source.Name == "" {
			return fmt.Errorf("library.sources[%d].name is required", i)
		}
		if strings.TrimSpace(source.Name) != source.Name {
			return fmt.Errorf("library.sources[%d].name must not have surrounding whitespace", i)
		}
		name := source.Name
		if seen[name] {
			return fmt.Errorf("library.sources lists name %q twice", name)
		}
		seen[name] = true
		switch source.Kind {
		case LibraryKindFile:
			source.Path = normalizeLibrarySourcePath(source.Path)
			c.Library.Sources[i].Path = source.Path
			if strings.TrimSpace(source.Path) == "" {
				return fmt.Errorf("library.sources[%q].path is required for kind %q", name, LibraryKindFile)
			}
			if !filepath.IsAbs(source.Path) {
				return fmt.Errorf("library.sources[%q].path must be absolute", name)
			}
		case "":
			return fmt.Errorf("library.sources[%q].kind is required (%q)", name, LibraryKindFile)
		default:
			return fmt.Errorf("library.sources[%q].kind %q is not supported (only %q in v1; command sources are planned for v1.1)", name, source.Kind, LibraryKindFile)
		}
		switch source.Format {
		case "", string(bibparse.FormatBibTeX), string(bibparse.FormatRIS), string(bibparse.FormatCSLJSON), string(bibparse.FormatNBIB):
		default:
			return fmt.Errorf("library.sources[%q].format %q must be empty or one of %s, %s, %s, %s",
				name, source.Format,
				bibparse.FormatBibTeX, bibparse.FormatRIS, bibparse.FormatCSLJSON, bibparse.FormatNBIB)
		}
		switch source.Claim {
		case LibraryClaimPDFPresent, LibraryClaimRecordPresent:
		default:
			return fmt.Errorf("library.sources[%q].claim must be %q or %q", name, LibraryClaimPDFPresent, LibraryClaimRecordPresent)
		}
	}
	return nil
}

// extensionIDRE matches Chrome's a-p base16 extension ID alphabet.
var extensionIDRE = regexp.MustCompile(`^[a-p]{32}$`)

// firefoxExtensionIDRE matches Gecko's email-like or braced-GUID add-on ID.
var firefoxExtensionIDRE = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+$|^\{[0-9a-fA-F-]{36}\}$`)

var resolverNameRE = regexp.MustCompile(`^[a-z0-9]+$`)
var proquestAccountIDRE = regexp.MustCompile(`^[0-9]+$`)

// validateLibKey rejects unusable LibKey routing config instead of letting it
// sit silently dead (the [sources.unpaywal] lesson: config that parses cleanly
// and does nothing). The %w chains under a "browser." or
// "browser.resolvers.<name>." prefix, so messages start with the field name.
func validateLibKey(mode string, libraryID int64) error {
	switch mode {
	case "", "off":
		if libraryID != 0 {
			return fmt.Errorf("libkey_library_id is set but libkey_mode is not \"link\"")
		}
		return nil
	case "link":
		if libraryID <= 0 {
			return fmt.Errorf("libkey_mode \"link\" requires a positive libkey_library_id (the number in the institution's BrowZine/LibKey.io URL)")
		}
		return nil
	case "api":
		return fmt.Errorf("libkey_mode \"api\" is not implemented; use \"link\" (ADR-0016)")
	default:
		return fmt.Errorf("libkey_mode must be \"off\" or \"link\"")
	}
}

func validateOpenURLBase(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || strings.TrimSpace(base) != base {
		return fmt.Errorf("must be an absolute https URL")
	}
	return nil
}

// documentDeliveryKinds is the exhaustive set of document_delivery.kind
// values v1's adapter has shipped (ADR-0017 Decision 2): a kind whose
// implementation does not exist must not parse, the same fail-closed rule
// validSourceNames applies to sources.
var documentDeliveryKinds = map[string]bool{
	"openurl": true,
	"libkey":  true,
	"illiad":  true,
	"custom":  true,
}

// documentDeliveryAutoCapableKinds is v1's set of kinds that can compile
// auto_capable (ADR-0017 Decision 3A): only source-controlled API
// integrations with a deterministic submission-and-reconciliation contract.
// openurl, libkey, and custom route to a form and are permanently
// prefill-only.
var documentDeliveryAutoCapableKinds = map[string]bool{
	"illiad": true,
}

// validateDocumentDelivery is fail-closed on every field (ADR-0017 Decision
// 2 and 3A): a misconfigured or half-declared document-delivery profile must
// be a startup error, not a route that silently compiles prefill_only, or
// worse, silently auto-submits. prefix already ends in "." and names the
// document_delivery table (e.g. "browser.document_delivery."), so every
// message below starts with the field name.
func validateDocumentDelivery(prefix string, d *DocumentDelivery) error {
	if d == nil {
		return nil
	}
	kind := strings.TrimSpace(d.Kind)
	switch {
	case kind == "":
		return fmt.Errorf("%skind is required (openurl, libkey, illiad, or custom)", prefix)
	case kind == "oclc" || kind == "rapido":
		return fmt.Errorf("%skind %q is not implemented; the ADR names them as intended providers", prefix, kind)
	case !documentDeliveryKinds[kind]:
		return fmt.Errorf("%skind %q is not a recognized document-delivery kind (openurl, libkey, illiad, or custom)", prefix, kind)
	}
	if d.BaseURL != "" {
		if err := validateOpenURLBase(d.BaseURL); err != nil {
			return fmt.Errorf("%sbase_url %w", prefix, err)
		}
	}
	switch d.SubmitPolicy {
	case "", "never", "prefill_only", "auto_if_unconditional":
	default:
		return fmt.Errorf("%ssubmit_policy must be never, prefill_only, or auto_if_unconditional", prefix)
	}
	if d.SubmitPolicy == "auto_if_unconditional" && !documentDeliveryAutoCapableKinds[kind] {
		// V1's only source-controlled API integration with a deterministic
		// submission-and-reconciliation contract is illiad; openurl, libkey,
		// and custom route to a form and supply no such contract, so they
		// are permanently prefill-only (ADR-0017 Decision 3A).
		return fmt.Errorf("%ssubmit_policy \"auto_if_unconditional\" requires kind \"illiad\" (openurl, libkey, and custom route to a form and are permanently prefill-only; ADR-0017 Decision 3A)", prefix)
	}
	switch d.LegalBasis {
	case "", "institution_policy", "copyright_act_s49", "unknown":
	default:
		return fmt.Errorf("%slegal_basis must be institution_policy, copyright_act_s49, or unknown", prefix)
	}
	switch d.PatronAttestation {
	case "", "not_required", "standing_completed", "per_request", "unknown":
	default:
		return fmt.Errorf("%spatron_attestation must be not_required, standing_completed, per_request, or unknown", prefix)
	}
	switch d.PatronFeePolicy {
	case "", "zero_standard", "per_request", "unknown":
	default:
		return fmt.Errorf("%spatron_fee_policy must be zero_standard, per_request, or unknown", prefix)
	}
	for _, class := range d.RequestClasses {
		if class != "digital_journal_article" {
			return fmt.Errorf("%srequest_classes entry %q is not modelled yet (only digital_journal_article)", prefix, class)
		}
	}
	if d.MonthlyRequestCap < 0 {
		return fmt.Errorf("%smonthly_request_cap must be >= 0", prefix)
	}
	if d.StatusPollMinutes < 0 {
		return fmt.Errorf("%sstatus_poll_minutes must be >= 0 (0 uses the default)", prefix)
	}
	if d.APIKey != "" && kind != "illiad" {
		// A key on a form-kind profile is dead config: openurl, libkey, and
		// custom route to a form the browser opens and never call an API
		// with a credential (ADR-0017 Decision 2).
		return fmt.Errorf("%sapi_key is set but kind is %q, not \"illiad\" (a key on a form-kind profile is dead config)", prefix, kind)
	}
	return nil
}

// EffectiveAdoptionRoot returns the configured adoption root or its default.
func (c *Config) EffectiveAdoptionRoot() string {
	if c.Browser.AdoptionRoot != "" {
		return c.Browser.AdoptionRoot
	}
	return filepath.Join(c.DataDir, "adoptions")
}

// RequireAccessMode returns the mode or ErrAccessModeUnset.
func (c *Config) RequireAccessMode() (string, error) {
	if c.AccessMode == "" {
		return "", &ErrAccessModeUnset{Path: c.Path}
	}
	return c.AccessMode, nil
}

// EffectiveAccessMode resolves the access mode governing one job, given that
// job's snapshotted Policy.AccessMode.
//
// Two rules compose here, and both are needed.
//
// The snapshot is honoured, because Submit records the mode in force for the
// job including a per-request access_mode_override, and a decision path that
// read c.AccessMode directly silently discarded the override. That was a live
// defect: the override was validated, snapshotted, and printed by diagnose
// while every code path that actually decided whether to open an institutional
// handoff consulted the daemon-wide default instead.
//
// The snapshot is also re-clamped against the current configuration, because
// the daemon-wide mode is the operator's standing decision and must keep
// restraining work already in the queue. Honouring the snapshot alone would
// make the ceiling apply only at submit, so an operator tightening
// delegated -> conservative would not stop jobs already recorded from opening
// the handoff tabs they just revoked — inverting the invariant the clamp
// exists to protect. Re-clamping is monotone: it can only ever lower the mode.
//
// A row written before policies carried a mode falls back to the configured
// value, as does a job whose pinned mode Retry released.
func (c *Config) EffectiveAccessMode(policyMode string) string {
	return c.NarrowAccessMode(c.AccessMode, strings.TrimSpace(policyMode))
}

// accessModeRank orders the modes by how much papio will do without a human.
var accessModeRank = map[string]int{ModeConservative: 0, ModeAssisted: 1, ModeDelegated: 2}

// NarrowAccessMode returns whichever of the configured mode and a per-request
// override does less without a human.
//
// A per-request override may narrow, never widen. The daemon-wide access_mode
// is the operator's standing decision — first-run setup refuses to select one
// silently — and it is also the only brake that exists: papio has no admission
// control, no queue cap, and no drain, so under assisted or delegated an
// exhausted work parks on a human action that never expires. If a submitter
// could raise the mode, any process reaching the owner-only IPC socket,
// including an agent driving the MCP surface, could mint unbounded handoff tabs
// on a daemon whose operator deliberately opted out of opening any.
//
// Narrowing stays fully supported, which is what the override is actually for:
// a cohort submitter asking for conservative on a delegated daemon gets
// conservative. An operator who genuinely wants more automation edits their
// configuration, which is a deliberate act rather than a request parameter.
func (c *Config) NarrowAccessMode(configured, override string) string {
	configuredRank, knownConfigured := accessModeRank[configured]
	if !knownConfigured {
		// No operator ceiling on record. RequireAccessMode refuses to submit in
		// this state, so reaching it on a read means the mode was removed or
		// corrupted after the job was recorded — and deleting access_mode is the
		// most drastic tightening an operator can perform. Returning the job's
		// own mode here would make that gesture the one setting that WIDENS,
		// leaving already-recorded delegated jobs unrestrained. Fail closed.
		return ModeConservative
	}
	if override == "" {
		return configured
	}
	overrideRank, knownOverride := accessModeRank[override]
	if !knownOverride {
		// An unreadable snapshot is not evidence of permission. Submit validates
		// the enum, so a value that is not a known mode means corrupt or
		// hand-edited policy_json, and resolving it to the daemon-wide default
		// would let corruption raise a job to whatever the daemon allows.
		return ModeConservative
	}
	if overrideRank >= configuredRank {
		return configured
	}
	return override
}

// FetchTimeout is Fetch.TimeoutSeconds as a duration.
func (c *Config) FetchTimeout() time.Duration {
	return time.Duration(c.Fetch.TimeoutSeconds) * time.Second
}

// SourcePolicy returns the effective source policy (zero value when absent).
func (c *Config) SourcePolicy(name string) Source {
	return c.Sources[name]
}

// InstitutionFor returns the institutional-access identity for a resolver
// profile. The empty name (and the explicit "default" name) select the default
// institution expressed by the top-level Browser fields; any other name selects
// a named profile from Browser.Resolvers. The boolean reports whether a usable
// profile exists (default: a non-empty OpenURL base; named: map presence).
func (c *Config) InstitutionFor(name string) (Institution, bool) {
	if name == "" || name == "default" {
		inst := Institution{
			OpenURLBase:        c.Browser.OpenURLBase,
			ShibbolethEntityID: c.Browser.ShibbolethEntityID,
			ProquestAccountID:  c.Browser.ProquestAccountID,
			LibKeyMode:         c.Browser.LibKeyMode,
			LibKeyLibraryID:    c.Browser.LibKeyLibraryID,
			DocumentDelivery:   c.Browser.DocumentDelivery,
		}
		return inst, inst.OpenURLBase != ""
	}
	inst, ok := c.Browser.Resolvers[name]
	return inst, ok
}

// OpenURLBaseFor returns the configured OpenURL base for a resolver profile.
// Its boolean result distinguishes a configured profile from one that is merely
// unavailable because no base has been configured.
func (c *Config) OpenURLBaseFor(name string) (string, bool) {
	inst, ok := c.InstitutionFor(name)
	return inst.OpenURLBase, ok
}

// ResolverNames returns the selectable resolver profiles in stable order.
func (c *Config) ResolverNames() []string {
	names := make([]string, 0, len(c.Browser.Resolvers)+1)
	if c.Browser.OpenURLBase != "" {
		names = append(names, "default")
	}
	for name := range c.Browser.Resolvers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ResolverOrigins returns the distinct https origins of every configured OpenURL
// resolver base (default plus each named profile), sorted. The extension needs a
// host permission on a resolver origin to steer its "full text options" menu; it
// requests exactly these origins and drops any already covered by a static
// host_permission. Institution identity therefore lives only in the user's
// config, never in extension code.
func (c *Config) ResolverOrigins() []string {
	seen := make(map[string]struct{})
	origins := make([]string, 0, len(c.Browser.Resolvers)+1)
	add := func(base string) {
		if base == "" {
			return
		}
		u, err := url.Parse(base)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return
		}
		host := strings.ToLower(u.Hostname())
		if host == "" {
			return
		}
		origin := "https://" + host
		if port := u.Port(); port != "" && port != "443" {
			n, convErr := strconv.Atoi(port)
			if convErr != nil || n < 1 || n > 65535 {
				return
			}
			origin += ":" + port
		}
		if _, dup := seen[origin]; dup {
			return
		}
		seen[origin] = struct{}{}

		origins = append(origins, origin)
	}
	add(c.Browser.OpenURLBase)
	for _, inst := range c.Browser.Resolvers {
		add(inst.OpenURLBase)
	}
	slices.Sort(origins)
	// Match the protocol cap so every accepted config yields a valid hello_ack.
	if len(origins) > 32 {
		origins = origins[:32]
	}
	return origins
}

// ResolverProfileForOrigin maps a validated resolver origin to its configured
// profile. Matching includes the effective HTTPS port because two profiles on
// the same hostname may legitimately use different resolver listeners. An
// origin shared by multiple profiles is ambiguous and fails closed.
func (c *Config) ResolverProfileForOrigin(origin string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	originHost := strings.ToLower(u.Hostname())
	originPort := u.Port()
	if originPort == "" {
		originPort = "443"
	}
	match := ""
	for _, name := range c.ResolverNames() {
		inst, ok := c.InstitutionFor(name)
		if !ok || inst.OpenURLBase == "" {
			continue
		}
		base, parseErr := url.Parse(inst.OpenURLBase)
		if parseErr != nil || base.Scheme != "https" || base.Hostname() == "" {
			continue
		}
		basePort := base.Port()
		if basePort == "" {
			basePort = "443"
		}
		if !strings.EqualFold(base.Hostname(), originHost) || basePort != originPort {
			continue
		}
		if match != "" && match != name {
			return "", false
		}
		match = resolverProfileName(name)
	}
	return match, match != ""
}

func resolverProfileName(name string) string {
	if name == "" || name == "default" {
		return "default"
	}
	return name
}

// Save validates and atomically writes cfg as a user-only TOML file. An empty
// path uses the default config location. API keys may be present, so neither
// temporary nor final files are group/world-readable.
func Save(cfg Config, path string) error {
	if path == "" {
		path = filepath.Join(Dir(), "config.toml")
	}
	if _, err := cfg.RequireAccessMode(); err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	cfg.Path = path
	return nil
}
