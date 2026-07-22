// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/mail"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
			var report doctor.Report
			err := opt.call(ctx, "doctor.run", struct{}{}, &report)
			return report, err
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
	command.Flags().StringVar(&extensionID, "extension-id", "", "Chrome extension ID allowed to reach the native host")
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
		initLine(opt.out, false, "Zotio", fmt.Sprintf("%v; Zotero features are disabled", err))
	} else {
		initLine(opt.out, true, "Zotio", "available at "+cfg.Zotio.Executable)
	}

	if input.skipBrowser {
		initLine(opt.out, true, "Browser", "skipped")
	} else if err := deps.InstallNative(cfg); err != nil {
		initLine(opt.out, false, "Browser", fmt.Sprintf("native-host install: %v", err))
		writeBrowserInstructions(opt.out)
	} else {
		initLine(opt.out, true, "Browser", "native messaging host installed")
		writeBrowserInstructions(opt.out)
	}

	report, err := deps.RunDoctor(cmd.Context(), opt)
	if err != nil {
		initLine(opt.out, false, "Daemon and doctor", fmt.Sprintf("%v", err))
	} else {
		if !report.OK {
			initLine(opt.out, false, "Daemon and doctor", "daemon autostarted; doctor reported failures")
		} else {
			initLine(opt.out, true, "Daemon and doctor", "daemon autostarted")
		}
		if err := renderDoctorReport(opt.out, report); err != nil {
			return err
		}
	}
	return nil
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
	if !input.nonInteractive {
		fmt.Fprintln(out, "Configuration")
		if !input.emailSet {
			value, err := initPrompt(reader, out, "Contact email for polite API pools", cfg.Email)
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
		fmt.Fprintln(out, "Zotio")
		if !input.zotioPathSet {
			value, err := initPrompt(reader, out, "zotio executable", cfg.Zotio.Executable)
			if err != nil {
				return err
			}
			cfg.Zotio.Executable = value
		}
		if !input.attachmentSet {
			value, err := initPrompt(reader, out, "Attachment mode (stored or linked-file)", cfg.Zotio.AttachmentMode)
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
		value, err := initPrompt(reader, out, "Install browser integration (yes/no)", "yes")
		if err != nil {
			return err
		}
		switch strings.ToLower(value) {
		case "yes", "y":
		case "no", "n":
			input.skipBrowser = true
		default:
			return fmt.Errorf("browser integration choice must be yes or no")
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
		fmt.Fprintln(out, "Browser extension")
		if !input.extensionIDSet {
			chromeDefault := cfg.Browser.ExtensionID
			if chromeDefault == "" {
				chromeDefault = defaultChromeExtensionID
			}
			value, err := initPrompt(reader, out, "Chrome extension ID (Web Store install uses the default; unpacked builds: paste the ID from chrome://extensions)", chromeDefault)
			if err != nil {
				return err
			}
			cfg.Browser.ExtensionID = value
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
		fmt.Fprintln(out, "Institution")
		if !input.openurlBaseSet && !input.institutionURLSet {
			defaultValue := cfg.Browser.OpenURLBase
			if defaultValue == "" {
				if resolver, ok := institution.ZoteroResolver(); ok {
					defaultValue = resolver
					initLine(out, true, "Institution", "found an OpenURL resolver in your Zotero settings")
				}
			}
			value, err := initPrompt(reader, out, "Library OpenURL resolver base, or paste your library's discovery/search URL (blank to skip)", defaultValue)
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
				value, err := initPrompt(reader, out, "Shibboleth IdP entityID for auto login-routing (blank to skip)", cfg.Browser.ShibbolethEntityID)
				if err != nil {
					return err
				}
				cfg.Browser.ShibbolethEntityID = value
			}
			if !input.proquestSet {
				raw, err := initPrompt(reader, out, "ProQuest account id, or paste a ProQuest URL with accountid= (blank to skip)", cfg.Browser.ProquestAccountID)
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
		cfg.Browser.ExtensionID = strings.TrimSpace(input.extensionID)
	}
	if input.firefoxIDSet {
		cfg.Browser.FirefoxExtensionID = strings.TrimSpace(input.firefoxExtensionID)
	}
	if input.nonInteractive || input.checkUpdatesSet {
		cfg.Updates.Check = input.checkUpdates
	} else {
		enabled, err := initUpdateCheckPrompt(reader, out)
		if err != nil {
			return err
		}
		cfg.Updates.Check = enabled
	}
	return nil
}

func initPrompt(reader *bufio.Reader, out io.Writer, label, defaultValue string) (string, error) {
	if _, err := fmt.Fprintf(out, "%s [%s]: ", label, defaultValue); err != nil {
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

func initUpdateCheckPrompt(reader *bufio.Reader, out io.Writer) (bool, error) {
	const prompt = "Check for papio and zotio updates once a day? Queries GitHub releases only; nothing else is sent. [Y/n]"
	if _, err := fmt.Fprint(out, prompt+" "); err != nil {
		return false, err
	}
	value, err := reader.ReadString('\n')
	if err != nil && len(value) == 0 {
		return false, fmt.Errorf("reading update check choice: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("update check choice must be yes or no")
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

func writeBrowserInstructions(out io.Writer) {
	extensionPath, err := filepath.Abs("extension")
	if err != nil {
		extensionPath = "extension"
	}
	_, _ = fmt.Fprintf(out, "Browser setup:\n  Chrome:\n    1. Install papio from the Chrome Web Store: https://chromewebstore.google.com/detail/papio/%s\n       (unpacked development builds instead: chrome://extensions -> Developer mode -> Load unpacked -> %s, then re-run: papio init --extension-id <id shown on the card>).\n    2. Open papio's Details page and grant optional host permissions only for publisher sites you use.\n  Firefox:\n    1. Open about:debugging#/runtime/this-firefox and click Load Temporary Add-on (AMO listing pending review).\n    2. Select %s/firefox/manifest.json (its add-on ID %s is set by default).\n    3. On papio's options page, grant the Library resolver access permission.\n", defaultChromeExtensionID, extensionPath, extensionPath, defaultFirefoxExtensionID)
}
