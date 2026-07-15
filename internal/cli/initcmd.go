// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/bootstrap"
	"papio/internal/config"
	"papio/internal/doctor"
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
			_, err := installNativeHost(cfg, "")
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
	var nonInteractive, skipBrowser bool
	var email, zotioPath, attachmentMode string

	command := &cobra.Command{
		Use:   "init",
		Short: "Set up papio for a first run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deps.Bootstrap == nil || deps.CheckZotio == nil || deps.InstallNative == nil || deps.RunDoctor == nil {
				return fmt.Errorf("init command dependencies are incomplete")
			}
			return runInit(cmd, opt, deps, initOptions{
				nonInteractive: nonInteractive,
				skipBrowser:    skipBrowser,
				email:          email,
				zotioPath:      zotioPath,
				attachmentMode: attachmentMode,
				emailSet:       cmd.Flags().Changed("email"),
				zotioPathSet:   cmd.Flags().Changed("zotio-path"),
				attachmentSet:  cmd.Flags().Changed("attachment-mode"),
			})
		},
	}
	command.Flags().BoolVar(&nonInteractive, "non-interactive", false, "do not prompt; retain existing values unless a flag overrides them")
	command.Flags().StringVar(&email, "email", "", "contact email for polite API pools")
	command.Flags().StringVar(&zotioPath, "zotio-path", "", "zotio executable path")
	command.Flags().StringVar(&attachmentMode, "attachment-mode", "", "zotio attachment mode: stored or linked-file")
	command.Flags().BoolVar(&skipBrowser, "skip-browser", false, "skip Chrome extension and native-host setup")
	return command
}

type initOptions struct {
	nonInteractive bool
	skipBrowser    bool
	email          string
	zotioPath      string
	attachmentMode string
	emailSet       bool
	zotioPathSet   bool
	attachmentSet  bool
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

func writeBrowserInstructions(out io.Writer) {
	extensionPath, err := filepath.Abs("extension")
	if err != nil {
		extensionPath = "extension"
	}
	_, _ = fmt.Fprintf(out, "Browser setup:\n  1. Open chrome://extensions.\n  2. Enable Developer mode.\n  3. Click Load unpacked and select %s.\n  4. Open Papio's Details page and grant the optional host permissions only for publisher sites you use.\n  5. If Chrome assigned a new unpacked extension ID, set browser.extension_id to it and rerun papio init to register the native host.\n", extensionPath)
}
