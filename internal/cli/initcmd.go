// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/institution"
)

const zotioVersionTimeout = 5 * time.Second

type initBootstrapper func(context.Context, config.Config) (io.Closer, error)
type initZotioChecker func(context.Context, string) error
type initNativeInstaller func(config.Config) error
type initDoctorRunner func(context.Context, *options) (doctor.Report, error)

type initDependencies struct {
	Bootstrap     initBootstrapper
	CheckZotio    initZotioChecker
	InstallNative initNativeInstaller
	RunDoctor     initDoctorRunner
}

func defaultInitDependencies() initDependencies {
	return initDependencies{
		Bootstrap: func(ctx context.Context, cfg config.Config) (io.Closer, error) {
			return bootstrap.New(ctx, cfg)
		},
		CheckZotio: checkZotioVersion,
		InstallNative: func(cfg config.Config) error {
			_, err := installNativeHost(cfg, "", "")
			return err
		},
		RunDoctor: func(ctx context.Context, opt *options) (doctor.Report, error) {
			// The daemon RPC covers readiness checks only; the extension,
			// native-host, and zotio checks the summary and gating rely on
			// come from the CLI-side integration pass. opt.call above has
			// already autostarted the daemon, so the existing-daemon probe
			// used by the integration dependencies succeeds.
			var report doctor.Report
			if err := opt.call(ctx, "doctor.run", struct{}{}, &report); err != nil {
				return report, err
			}
			integration := doctor.RunIntegration(ctx, defaultDoctorDependencies(opt))
			report.Checks = append(report.Checks, integration.Checks...)
			report.OK = report.OK && integration.OK
			return report, nil
		},
	}
}

// newInitCommand builds the guided, idempotent first-run setup command.
func newInitCommand(opt *options) *cobra.Command {
	return newInitCommandWithDependencies(opt, defaultInitDependencies())
}

func newInitCommandWithDependencies(opt *options, deps initDependencies) *cobra.Command {
	var checkUpdates, nonInteractive, skipBrowser bool
	var email, zotioPath, attachmentMode string
	var institutionURL, openurlBase, shibbolethEntityID, proquestAccountID string
	var extensionID, firefoxExtensionID string

	command := &cobra.Command{
		Use:         "init",
		Short:       "Set up papio for a first run",
		Annotations: map[string]string{"mcp:hidden": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deps.Bootstrap == nil || deps.CheckZotio == nil || deps.InstallNative == nil || deps.RunDoctor == nil {
				return fmt.Errorf("init command dependencies are incomplete")
			}
			return runInit(cmd, opt, deps, initOptions{
				nonInteractive:     nonInteractive,
				skipBrowser:        skipBrowser,
				email:              email,
				zotioPath:          zotioPath,
				attachmentMode:     attachmentMode,
				institutionURL:     institutionURL,
				openurlBase:        openurlBase,
				shibbolethEntityID: shibbolethEntityID,
				proquestAccountID:  proquestAccountID,
				extensionID:        extensionID,
				firefoxExtensionID: firefoxExtensionID,
				checkUpdates:       checkUpdates,
				emailSet:           cmd.Flags().Changed("email"),
				zotioPathSet:       cmd.Flags().Changed("zotio-path"),
				attachmentSet:      cmd.Flags().Changed("attachment-mode"),
				institutionURLSet:  cmd.Flags().Changed("institution-url"),
				openurlBaseSet:     cmd.Flags().Changed("openurl-base"),
				entityIDSet:        cmd.Flags().Changed("shibboleth-entity-id"),
				proquestSet:        cmd.Flags().Changed("proquest-account-id"),
				extensionIDSet:     cmd.Flags().Changed("extension-id"),
				firefoxIDSet:       cmd.Flags().Changed("firefox-extension-id"),
				checkUpdatesSet:    cmd.Flags().Changed("check-updates"),
			})
		},
	}
	command.Flags().BoolVar(&nonInteractive, "non-interactive", false, "do not prompt; retain existing values unless a flag overrides them")
	command.Flags().StringVar(&email, "email", "", "contact email for polite API pools")
	command.Flags().StringVar(&zotioPath, "zotio-path", "", "zotio executable path")
	command.Flags().StringVar(&attachmentMode, "attachment-mode", "", "zotio attachment mode: stored or linked-file")
	command.Flags().StringVar(&institutionURL, "institution-url", "", "library discovery or resolver URL; papio derives the OpenURL base")
	command.Flags().StringVar(&openurlBase, "openurl-base", "", "institution OpenURL resolver base URL")
	command.Flags().StringVar(&shibbolethEntityID, "shibboleth-entity-id", "", "Shibboleth IdP entityID for federated login-routing")
	command.Flags().StringVar(&proquestAccountID, "proquest-account-id", "", "ProQuest account id, or a ProQuest URL containing accountid=")
	command.Flags().StringVar(&extensionID, "extension-id", "", "Chrome extension ID allowed to reach the native host, or an unpacked extension folder path (papio computes its ID)")
	command.Flags().StringVar(&firefoxExtensionID, "firefox-extension-id", "", "Firefox add-on ID allowed to reach the native host")
	command.Flags().BoolVar(&skipBrowser, "skip-browser", false, "skip Chrome extension and native-host setup")
	command.Flags().BoolVar(&checkUpdates, "check-updates", true, "check for papio and zotio updates once a day via GitHub releases (sends nothing else)")
	return command
}

type initOptions struct {
	nonInteractive     bool
	skipBrowser        bool
	email              string
	zotioPath          string
	attachmentMode     string
	institutionURL     string
	openurlBase        string
	shibbolethEntityID string
	proquestAccountID  string
	extensionID        string
	firefoxExtensionID string
	checkUpdates       bool
	emailSet           bool
	zotioPathSet       bool
	attachmentSet      bool
	institutionURLSet  bool
	openurlBaseSet     bool
	entityIDSet        bool
	proquestSet        bool
	extensionIDSet     bool
	firefoxIDSet       bool
	checkUpdatesSet    bool
}

func runInit(cmd *cobra.Command, opt *options, deps initDependencies, input initOptions) error {
	path := opt.configPath
	if path == "" {
		path = filepath.Join(config.Dir(), "config.toml")
	}
	cfg, exists, err := initConfig(path)
	if err != nil {
		return initRequiredFailure(opt.out, "Configuration", err)
	}
	if err := applyInitConfig(cmd, opt.out, &cfg, exists, &input); err != nil {
		return initRequiredFailure(opt.out, "Configuration", err)
	}
	if err := config.Save(cfg, path); err != nil {
		return initRequiredFailure(opt.out, "Configuration", err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		return initRequiredFailure(opt.out, "Configuration", err)
	}
	if !input.nonInteractive {
		fmt.Fprintln(opt.out)
	}
	initLine(opt.out, true, "Configuration", "wrote "+cfg.Path)

	system, err := deps.Bootstrap(cmd.Context(), cfg)
	if err != nil {
		return initRequiredFailure(opt.out, "Data", fmt.Errorf("apply migrations: %w", err))
	}
	if err := system.Close(); err != nil {
		return initRequiredFailure(opt.out, "Data", fmt.Errorf("close migration bootstrap: %w", err))
	}
	initLine(opt.out, true, "Data", "created "+cfg.DataDir+" and applied migrations")

	if err := deps.CheckZotio(cmd.Context(), cfg.Zotio.Executable); err != nil {
		initLine(opt.out, false, "zotio", fmt.Sprintf("%v; Zotero features are disabled", err))
	} else {
		initLine(opt.out, true, "zotio", "available at "+cfg.Zotio.Executable)
	}

	browserInstalled := false
	if input.skipBrowser {
		initLine(opt.out, true, "Browser", "skipped")
	} else if err := deps.InstallNative(cfg); err != nil {
		initLine(opt.out, false, "Browser", fmt.Sprintf("native-host install: %v", err))
		writeBrowserInstructions(opt.out, cfg)
	} else {
		initLine(opt.out, true, "Browser", "native messaging host installed")
		browserInstalled = true
	}

	report, err := deps.RunDoctor(cmd.Context(), opt)
	if err != nil {
		initLine(opt.out, false, "Daemon and doctor", fmt.Sprintf("%v", err))
		if browserInstalled {
			writeBrowserInstructions(opt.out, cfg)
		}
		fmt.Fprintln(opt.out, "\nNext: papio doctor --start")
		return nil
	}
	if report.OK {
		initLine(opt.out, true, "Daemon and doctor", "daemon autostarted")
	} else {
		initLine(opt.out, false, "Daemon and doctor", "daemon autostarted; doctor reported failures")
	}
	// Setup instructions only when the extension is not already talking to
	// the daemon — a healthy re-run should not re-print install steps.
	if browserInstalled && !initExtensionHealthy(report) {
		writeBrowserInstructions(opt.out, cfg)
	}
	// Init already succeeded or failed step by step above; the full PASS table
	// is noise here. Summarize, show only what needs attention, and point at
	// `papio doctor` for the rest.
	writeInitDoctorSummary(opt.out, report)
	fmt.Fprintln(opt.out, "\nNext: "+initNextAction(input, report))
	return nil
}

// initExtensionHealthy reports whether doctor saw a connected, current
// extension.
func initExtensionHealthy(report doctor.Report) bool {
	for _, check := range report.Checks {
		if check.Name == "extension" {
			return check.Status == doctor.Pass
		}
	}
	return false
}

// writeInitDoctorSummary prints one summary line plus any WARN/FAIL checks.
// SKIPs are deliberate states (optional integrations, disabled update
// checks), so they are counted but never presented as needing attention.
func writeInitDoctorSummary(out io.Writer, report doctor.Report) {
	passed, skipped := 0, 0
	attention := make([]doctor.Check, 0, 4)
	for _, check := range report.Checks {
		switch check.Status {
		case doctor.Pass:
			passed++
		case doctor.Skip:
			skipped++
		default:
			attention = append(attention, check)
		}
	}
	fmt.Fprintf(out, "doctor: %d checks passed", passed)
	if skipped > 0 {
		fmt.Fprintf(out, ", %d skipped", skipped)
	}
	switch len(attention) {
	case 0:
		fmt.Fprintln(out, "")
		return
	case 1:
		fmt.Fprint(out, ", 1 check needs attention (full table: papio doctor)\n")
	default:
		fmt.Fprintf(out, ", %d checks need attention (full table: papio doctor)\n", len(attention))
	}
	for _, check := range attention {
		fmt.Fprintf(out, "  %-4s %s — %s\n", strings.ToUpper(check.Status), check.Name, check.Detail)
		if check.Remediation != "" {
			fmt.Fprintf(out, "       fix: %s\n", check.Remediation)
		}
	}
}

// initNextAction picks exactly one suggested next step, in order of what the
// report proves: failures trump everything, a healthy extension means
// acquisition works regardless of how this run was invoked, a missing or
// unhealthy extension routes to setup, and only then does --skip-browser
// imply an OA-only footing.
func initNextAction(input initOptions, report doctor.Report) string {
	extension := ""
	for _, check := range report.Checks {
		if check.Status == doctor.Fail {
			return "papio doctor"
		}
		if check.Name == "extension" {
			extension = check.Status
		}
	}
	switch {
	case extension == doctor.Pass:
		return `papio acquire "<doi>" --wait   (or: papio status)`
	case input.skipBrowser:
		return `papio acquire "<doi>" --wait   (OA-only until browser integration is set up)`
	case extension != "":
		return "install the papio extension (see Browser setup above), then: papio doctor"
	case input.skipBrowser:
		return `papio acquire "<doi>" --wait   (OA-only until browser integration is set up)`
	}
	return `papio acquire "<doi>" --wait   (or: papio status)`
}

func initConfig(path string) (config.Config, bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		cfg, err := config.Load(path)
		return cfg, true, err
	case os.IsNotExist(err):
		cfg := config.Default()
		// The guided setup's conservative default keeps acquisition policy explicit
		// and valid without enabling automation beyond the safest baseline.
		cfg.AccessMode = config.ModeConservative
		return cfg, false, nil
	default:
		return config.Config{}, false, fmt.Errorf("stat config %s: %w", path, err)
	}
}

func applyInitConfig(cmd *cobra.Command, out io.Writer, cfg *config.Config, exists bool, input *initOptions) error {
	if input.attachmentSet && input.attachmentMode != "stored" && input.attachmentMode != "linked-file" {
		return fmt.Errorf("--attachment-mode must be stored or linked-file")
	}
	if input.institutionURLSet && input.openurlBaseSet {
		return fmt.Errorf("--institution-url and --openurl-base cannot be used together")
	}

	reader := bufio.NewReader(cmd.InOrStdin())
	sections := newInitSections(out, input.skipBrowser)
	if !input.nonInteractive {
		sections.header("Contact", "Used for polite API pools (Unpaywall, OpenAlex).")
		if !input.emailSet {
			value, err := initPrompt(reader, out, "email", cfg.Email)
			if err != nil {
				return err
			}
			cfg.Email = value
		}
		if !exists && cfg.AccessMode == "" {
			cfg.AccessMode = config.ModeConservative
		}
	}
	if input.emailSet {
		cfg.Email = strings.TrimSpace(input.email)
	}
	if err := validateInitEmail(cfg.Email); err != nil {
		return err
	}

	if !input.nonInteractive {
		sections.header("zotio", "The Zotero boundary: imports are previewed and confirmed there. The stored mode copies PDFs into Zotero; linked-file mode references papio's copy on disk.")
		if !input.zotioPathSet {
			zotioDefault, zotioSource := cfg.Zotio.Executable, ""
			// A bare command name is autodiscovered so the prompt shows the
			// real binary instead of asking the user to know their PATH.
			if zotioDefault == "" || !strings.Contains(zotioDefault, string(os.PathSeparator)) {
				lookup := zotioDefault
				if lookup == "" {
					lookup = "zotio"
				}
				if resolved, err := exec.LookPath(lookup); err == nil {
					zotioDefault, zotioSource = resolved, "found on PATH"
				}
			}
			value, err := initPromptSourced(reader, out, "zotio executable", zotioDefault, zotioSource)
			if err != nil {
				return err
			}
			cfg.Zotio.Executable = value
		}
		if !input.attachmentSet {
			value, err := initPrompt(reader, out, "attachment mode (stored/linked-file)", cfg.Zotio.AttachmentMode)
			if err != nil {
				return err
			}
			cfg.Zotio.AttachmentMode = value
		}
	}
	if input.zotioPathSet {
		cfg.Zotio.Executable = strings.TrimSpace(input.zotioPath)
	}
	if input.attachmentSet {
		cfg.Zotio.AttachmentMode = input.attachmentMode
	}

	if !input.nonInteractive && !input.skipBrowser {
		explain := "Institutional access runs in a supported signed-in browser: Chrome, Edge, Brave, Vivaldi, Opera, Chromium, or Firefox."
		if detected := detectBrowsersBounded(300 * time.Millisecond); len(detected) > 0 {
			explain += " Detected: " + strings.Join(detected, ", ") + "."
		}
		sections.header("Browser", explain)
		yes, err := initYesNo(reader, out, "Install browser integration?", true)
		if err != nil {
			return err
		}
		if !yes {
			input.skipBrowser = true
			// The Institution section is skipped with the browser; advance
			// its slot so the final section still renders [5/5].
			sections.index++
		}
	}

	// Browser extension identity: the native host only accepts these exact
	// extension IDs. Both browsers now have fixed IDs for the published
	// packages — the Chrome Web Store item and the built Firefox add-on — so
	// both default to the known value and work on the first run. Only an
	// unpacked development build carries a machine-specific Chrome ID; paste
	// the one shown at chrome://extensions (or pass --extension-id) in that
	// case. An empty value leaves that browser's bridge disabled.
	if !input.nonInteractive && !input.skipBrowser {
		_, dim := initStyle(out)
		fmt.Fprintf(out, "  %s\n", dim("Web Store installs use the default ID; unpacked builds: paste the ID from chrome://extensions, or the extension folder path and papio computes it."))
		if !input.extensionIDSet {
			chromeDefault := cfg.Browser.ExtensionID
			if chromeDefault == "" {
				chromeDefault = defaultChromeExtensionID
			}
			value, err := initPrompt(reader, out, "Chrome extension ID", chromeDefault)
			if err != nil {
				return err
			}
			id, computedFrom, err := resolveChromeExtensionID(value)
			if err != nil {
				return err
			}
			if computedFrom != "" {
				initLine(out, true, "Browser", "computed unpacked extension ID "+id+" from "+computedFrom)
			}
			cfg.Browser.ExtensionID = id
		}
		if !input.firefoxIDSet {
			firefoxDefault := cfg.Browser.FirefoxExtensionID
			if firefoxDefault == "" {
				firefoxDefault = defaultFirefoxExtensionID
			}
			value, err := initPrompt(reader, out, "Firefox add-on ID (blank to skip Firefox)", firefoxDefault)
			if err != nil {
				return err
			}
			cfg.Browser.FirefoxExtensionID = value
		}
	}

	// Institution: derive a library's OpenURL resolver and optional ProQuest
	// account id from either an explicit discovery URL or guided input.
	if input.institutionURLSet {
		discovery, err := institution.Discover(input.institutionURL)
		if err != nil {
			return err
		}
		if discovery.OpenURLBase == "" {
			return fmt.Errorf("%s", discovery.Note)
		}
		cfg.Browser.OpenURLBase = discovery.OpenURLBase
		if !input.proquestSet && cfg.Browser.ProquestAccountID == "" && discovery.ProquestAccountID != "" {
			cfg.Browser.ProquestAccountID = discovery.ProquestAccountID
		}
		initLine(out, true, "Institution", discovery.Note)
	}

	if !input.nonInteractive && !input.skipBrowser {
		sections.header("Institution", "Paste your library's discovery/search URL — papio derives the resolver. Blank to skip.")
		if !input.openurlBaseSet && !input.institutionURLSet {
			defaultValue, defaultSource := cfg.Browser.OpenURLBase, ""
			if defaultValue != "" {
				defaultSource = "keep current"
			} else if resolver, ok := institution.ZoteroResolver(); ok {
				defaultValue, defaultSource = resolver, "from Zotero"
			}
			value, err := initPromptSourced(reader, out, "resolver", defaultValue, defaultSource)
			if err != nil {
				return err
			}
			if value != "" {
				discovery, err := institution.Discover(value)
				if err == nil && discovery.OpenURLBase != "" {
					if discovery.OpenURLBase != value {
						initLine(out, true, "Institution", discovery.Note)
					}
					cfg.Browser.OpenURLBase = discovery.OpenURLBase
					if !input.proquestSet && cfg.Browser.ProquestAccountID == "" && discovery.ProquestAccountID != "" {
						cfg.Browser.ProquestAccountID = discovery.ProquestAccountID
					}
				} else {
					guidance := discovery.Note
					if err != nil {
						guidance = err.Error()
					}
					retry, err := initPrompt(reader, out, guidance, "")
					if err != nil {
						return err
					}
					if retry == "" {
						cfg.Browser.OpenURLBase = ""
					} else if !isHTTPSURL(retry) {
						return fmt.Errorf("%s", guidance)
					} else {
						cfg.Browser.OpenURLBase = retry
					}
				}
			}
		}
		if cfg.Browser.OpenURLBase != "" {
			if !input.entityIDSet {
				value, err := initPrompt(reader, out, "Shibboleth IdP entityID (blank to skip)", cfg.Browser.ShibbolethEntityID)
				if err != nil {
					return err
				}
				cfg.Browser.ShibbolethEntityID = value
			}
			if !input.proquestSet {
				raw, err := initPrompt(reader, out, "ProQuest account id or URL with accountid= (blank to skip)", cfg.Browser.ProquestAccountID)
				if err != nil {
					return err
				}
				id, err := proquestAccountIDFromInput(raw)
				if err != nil {
					return err
				}
				cfg.Browser.ProquestAccountID = id
			}
		}
	}
	if input.openurlBaseSet {
		cfg.Browser.OpenURLBase = strings.TrimSpace(input.openurlBase)
	}
	if input.entityIDSet {
		cfg.Browser.ShibbolethEntityID = strings.TrimSpace(input.shibbolethEntityID)
	}
	if input.proquestSet {
		id, err := proquestAccountIDFromInput(input.proquestAccountID)
		if err != nil {
			return err
		}
		cfg.Browser.ProquestAccountID = id
	}
	if input.extensionIDSet {
		id, computedFrom, err := resolveChromeExtensionID(input.extensionID)
		if err != nil {
			return err
		}
		if computedFrom != "" {
			initLine(out, true, "Browser", "computed unpacked extension ID "+id+" from "+computedFrom)
		}
		cfg.Browser.ExtensionID = id
	}
	if input.firefoxIDSet {
		cfg.Browser.FirefoxExtensionID = strings.TrimSpace(input.firefoxExtensionID)
	}
	if input.nonInteractive || input.checkUpdatesSet {
		cfg.Updates.Check = input.checkUpdates
	} else {
		sections.header("Updates", "Queries GitHub releases only; nothing else is sent.")
		enabled, err := initYesNo(reader, out, "Check for papio and zotio updates once a day?", true)
		if err != nil {
			return err
		}
		cfg.Updates.Check = enabled
	}
	return nil
}

// initStyle returns ANSI wrappers when out is an interactive terminal and
// NO_COLOR is unset; otherwise both are identity. Tests write to buffers and
// therefore always see plain text.
func initStyle(out io.Writer) (bold, dim func(string) string) {
	plain := func(s string) string { return s }
	bold, dim = plain, plain
	f, ok := out.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" {
		return bold, dim
	}
	if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return bold, dim
	}
	bold = func(s string) string { return "\x1b[1m" + s + "\x1b[0m" }
	dim = func(s string) string { return "\x1b[2m" + s + "\x1b[0m" }
	return bold, dim
}

// initSections numbers the guided steps. The total is computed from the flags
// at entry; a mid-flow opt-out (declining browser integration) skips numbers
// rather than renumbering what was already printed.
type initSections struct {
	out   io.Writer
	index int
	total int
}

func newInitSections(out io.Writer, skipBrowser bool) *initSections {
	total := 5 // Contact, zotio, Browser, Institution, Updates
	if skipBrowser {
		total = 3
	}
	return &initSections{out: out, total: total}
}

// header prints a numbered section header with an optional one-line
// explanation beneath it, separated from the previous section by a blank line.
func (s *initSections) header(title, explain string) {
	bold, dim := initStyle(s.out)
	s.index++
	fmt.Fprintf(s.out, "\n%s\n", bold(fmt.Sprintf("[%d/%d] %s", s.index, s.total, title)))
	if explain != "" {
		fmt.Fprintf(s.out, "  %s\n", dim(explain))
	}
}

// initDisplayDefault renders a prompt default. Long values (stored resolver
// URLs) are middle-ellipsized so the question stays readable; the full value
// is still what a blank answer keeps. A non-empty source names where the
// default came from ("keep current" = existing config, "from Zotero", …).
func initDisplayDefault(value, source string) string {
	display := value
	trimmed := strings.TrimPrefix(value, "https://")
	if len(trimmed) > 48 {
		display = trimmed[:28] + "…" + trimmed[len(trimmed)-16:]
		if source == "" {
			source = "keep current"
		}
	}
	if source == "" {
		return display
	}
	return source + ": " + display
}

func initPrompt(reader *bufio.Reader, out io.Writer, label, defaultValue string) (string, error) {
	return initPromptSourced(reader, out, label, defaultValue, "")
}

// initPromptSourced is initPrompt with a named default source shown inside
// the bracket, e.g. `resolver [from Zotero: …]`.
func initPromptSourced(reader *bufio.Reader, out io.Writer, label, defaultValue, source string) (string, error) {
	display := initDisplayDefault(defaultValue, source)
	if _, err := fmt.Fprintf(out, "  › %s [%s]: ", label, display); err != nil {
		return "", err
	}
	value, err := reader.ReadString('\n')
	if err != nil && len(value) == 0 {
		return "", fmt.Errorf("reading %s: %w", strings.ToLower(label), err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultValue, nil
	}
	return value, nil
}

// chromeUnpackedID computes the extension ID Chrome assigns an unpacked
// build: the first 16 bytes of SHA-256 over the absolute load directory,
// each nibble mapped 0→a…f→p. Lets init accept a folder path where an ID
// is expected. Verified against a live chrome://extensions card.
func chromeUnpackedID(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	encoded := hex.EncodeToString(sum[:16])
	id := make([]byte, len(encoded))
	for i := 0; i < len(encoded); i++ {
		c := encoded[i]
		if c >= '0' && c <= '9' {
			id[i] = 'a' + (c - '0')
		} else {
			id[i] = 'a' + (c - 'a') + 10
		}
	}
	return string(id)
}

// chromeExtensionIDRE is the only shape a literal Chromium extension ID can
// have: 32 characters, a-p (hex nibbles mapped into the a-p alphabet).
var chromeExtensionIDRE = regexp.MustCompile(`^[a-p]{32}$`)

// resolveChromeExtensionID turns prompt/flag input into an extension ID.
// A well-formed literal ID passes through; anything else is treated as an
// unpacked extension folder (absolute, relative, or ~-prefixed) and must be
// a directory containing manifest.json — classification is by validity, not
// punctuation, so `--extension-id extension` works from a checkout.
func resolveChromeExtensionID(raw string) (id string, computedFrom string, err error) {
	value := strings.TrimSpace(raw)
	if value == "" || chromeExtensionIDRE.MatchString(value) {
		return value, "", nil
	}
	if runtime.GOOS == "windows" {
		// Chromium hashes the normalized UTF-16 path on Windows; hashing the
		// UTF-8 string would yield a plausible-looking but wrong ID and a
		// native-host allowlist no extension can match. Fail loud instead.
		return "", "", fmt.Errorf("%q is not a valid extension ID; on Windows paste the literal ID from chrome://extensions (folder-path computation is not supported)", value)
	}
	if strings.HasPrefix(value, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", fmt.Errorf("expanding %q: %w", value, err)
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~"))
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", "", fmt.Errorf("resolving %q: %w", value, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", "", fmt.Errorf("%q is neither a 32-character extension ID nor an extension folder: %w", raw, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("extension path %s is not a directory", absolute)
	}
	if _, err := os.Stat(filepath.Join(absolute, "manifest.json")); err != nil {
		return "", "", fmt.Errorf("%s does not contain manifest.json; point at the folder Chrome loads", absolute)
	}
	return chromeUnpackedID(absolute), absolute, nil
}

// detectBrowsersBounded caps detection latency: the result is decorative,
// and a stat against a wedged network mount must not stall the prompt flow.
func detectBrowsersBounded(timeout time.Duration) []string {
	result := make(chan []string, 1)
	go func() { result <- detectBrowsers() }()
	select {
	case found := <-result:
		return found
	case <-time.After(timeout):
		return nil
	}
}

// detectBrowsers names installed browsers, best-effort, for the Browser
// section explanation. Empty when nothing is detectable.
func detectBrowsers() []string {
	found := []string{}
	if runtime.GOOS == "darwin" {
		apps := []struct{ name, path string }{
			{"Chrome", "Google Chrome.app"},
			{"Edge", "Microsoft Edge.app"},
			{"Brave", "Brave Browser.app"},
			{"Vivaldi", "Vivaldi.app"},
			{"Opera", "Opera.app"},
			{"Firefox", "Firefox.app"},
		}
		home, _ := os.UserHomeDir()
		for _, app := range apps {
			for _, root := range []string{"/Applications", filepath.Join(home, "Applications")} {
				if _, err := os.Stat(filepath.Join(root, app.path)); err == nil {
					found = append(found, app.name)
					break
				}
			}
		}
		return found
	}
	binaries := []struct{ name, binary string }{
		{"Chrome", "google-chrome"}, {"Chromium", "chromium"}, {"Edge", "microsoft-edge"},
		{"Brave", "brave-browser"}, {"Vivaldi", "vivaldi"}, {"Firefox", "firefox"},
	}
	for _, b := range binaries {
		if _, err := exec.LookPath(b.binary); err == nil {
			found = append(found, b.name)
		}
	}
	return found
}

// initYesNo is the single yes/no grammar for the guided flow: [Y/n] or [y/N],
// blank keeps the default, anything else is an error.
func initYesNo(reader *bufio.Reader, out io.Writer, label string, defaultYes bool) (bool, error) {
	hint := "[Y/n]"
	if !defaultYes {
		hint = "[y/N]"
	}
	if _, err := fmt.Fprintf(out, "  › %s %s: ", label, hint); err != nil {
		return false, err
	}
	value, err := reader.ReadString('\n')
	if err != nil && len(value) == 0 {
		return false, fmt.Errorf("reading %s: %w", strings.ToLower(label), err)
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return defaultYes, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be yes or no", strings.ToLower(label))
	}
}

func isHTTPSURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && strings.EqualFold(parsed.Scheme, "https") && parsed.Host != ""
}

// accountIDParamRE captures an accountid=<digits> query parameter from anywhere
// in a pasted ProQuest URL. proquestAccountDigitsRE matches a bare numeric id.
var accountIDParamRE = regexp.MustCompile(`[?&]accountid=([0-9]+)`)
var proquestAccountDigitsRE = regexp.MustCompile(`^[0-9]+$`)

// proquestAccountIDFromInput turns first-run input into a ProQuest account id.
// It accepts a bare numeric id, or any URL/string containing accountid=<digits>
// (as seen in a ProQuest link-resolver URL after logging in through a library),
// so a user who does not know the numeric id can paste a URL from their browser.
// Blank input yields an empty id (feature disabled).
func proquestAccountIDFromInput(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", nil
	}
	if proquestAccountDigitsRE.MatchString(s) {
		return s, nil
	}
	if m := accountIDParamRE.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("no ProQuest account id found in %q: paste a URL containing accountid=NNNN or the numeric id", s)
}

func validateInitEmail(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("an email is required for the OpenAlex polite pool")
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value {
		return fmt.Errorf("email %q is not a valid address", value)
	}
	return nil
}

func checkZotioVersion(ctx context.Context, executable string) error {
	path, err := exec.LookPath(executable)
	if err != nil {
		return fmt.Errorf("locate %q: %w", executable, err)
	}
	bounded, cancel := context.WithTimeout(ctx, zotioVersionTimeout)
	defer cancel()
	if output, err := exec.CommandContext(bounded, path, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("run %s --version: %w (%s)", path, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func initRequiredFailure(out io.Writer, step string, err error) error {
	initLine(out, false, step, err.Error())
	return fmt.Errorf("init: required %s step failed: %w", strings.ToLower(step), err)
}

func initLine(out io.Writer, pass bool, step, detail string) {
	mark := "✗"
	if pass {
		mark = "✓"
	}
	_, _ = fmt.Fprintf(out, "%s %s: %s\n", mark, step, detail)
}

// defaultChromeExtensionID is the Chrome Web Store package's fixed item id
// (https://chromewebstore.google.com/detail/papio/npccengdhjmpojpjmjoeeclpdhcjelhf).
// It is the init default so a store-installed extension works on the first run;
// unpacked development builds carry a different, machine-specific ID.
const defaultChromeExtensionID = "npccengdhjmpojpjmjoeeclpdhcjelhf"

// defaultFirefoxExtensionID is the built Firefox add-on's fixed gecko id (see
// extension/build.ts). It is the init default so Firefox works on the first run
// without the user discovering an ID.
const defaultFirefoxExtensionID = "papio@orgmentem.com"

// writeBrowserInstructions prints one line per browser the config actually
// enables. Store install is the path; development builds live in the docs.
func writeBrowserInstructions(out io.Writer, cfg config.Config) {
	fmt.Fprintln(out, "Browser setup:")
	if cfg.Browser.ExtensionID == defaultChromeExtensionID {
		fmt.Fprintf(out, "  Chrome: install papio — https://chromewebstore.google.com/detail/papio/%s — then grant host permissions on its Details page for publisher sites you use.\n", defaultChromeExtensionID)
	} else if cfg.Browser.ExtensionID != "" {
		fmt.Fprintln(out, "  Chrome: development build configured — load it unpacked from chrome://extensions (docs: guide/getting-started).")
	}
	if cfg.Browser.FirefoxExtensionID != "" {
		fmt.Fprintln(out, "  Firefox: about:debugging#/runtime/this-firefox → Load Temporary Add-on → extension/firefox/manifest.json, then grant resolver access on the options page (AMO review pending).")
	}
}
