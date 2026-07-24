// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/api"

	"papio/internal/config"
	"papio/internal/doctor"
	"papio/internal/update"
	"papio/internal/zotio"
)

var errDoctorFailed = errors.New("doctor found failing checks")

type doctorReadinessRunner func(context.Context, config.Config) doctor.Report

func defaultDoctorDependencies(opt *options) doctor.IntegrationDependencies {
	return doctor.IntegrationDependencies{
		CLIVersion: api.Version,
		LoadConfig: opt.loadConfig,
		DaemonStatus: func(ctx context.Context, _ config.Config) (doctor.DaemonStatus, error) {
			var status doctor.DaemonStatus
			if err := opt.call(ctx, "ping", struct{}{}, &status); err != nil {
				return doctor.DaemonStatus{}, err
			}
			return status, nil
		},
		ManifestDir: func(config.Config) (string, error) {
			return defaultManifestDir()
		},
		FirefoxDir: func(config.Config) (string, error) {
			return defaultFirefoxManifestDir()
		},
		ReadFile: os.ReadFile,
		// Follows the symlink; false when the host executable it points at is gone.
		HostExecutableResolves: func(execPath string) bool {
			_, err := os.Stat(execPath)
			return err == nil
		},
		ZotioPreflight: func(ctx context.Context, cfg config.Config) (*zotio.PreflightResult, error) {
			return zotio.New(cfg.Zotio).Preflight(ctx)
		},
		CheckUpdates: func(ctx context.Context, cfg config.Config) (*update.Info, error) {
			return update.New(cfg.DataDir).Check(ctx)
		},
		CheckZotioUpdates: func(ctx context.Context, cfg config.Config) (*update.Info, error) {
			return update.NewZotio(cfg.DataDir).Check(ctx)
		},
	}
}

func daemonReadinessDoctor(opt *options, daemonErr *error) doctorReadinessRunner {
	return func(ctx context.Context, _ config.Config) doctor.Report {
		// A reused command tree (the in-process MCP root) runs doctor many
		// times; a transient failure must not outlive its own run.
		*daemonErr = nil
		var report doctor.Report
		if err := opt.call(ctx, "doctor.run", struct{}{}, &report); err != nil {
			// RunIntegration will render the daemon failure and its single
			// dependent-check skip. Do not add local checks that would obscure it.
			*daemonErr = err
			return doctor.Report{OK: true}
		}
		return report
	}
}

func newDoctorCommand(opt *options) *cobra.Command {
	deps := defaultDoctorDependencies(opt)
	diagnose := deps.DaemonStatus
	var readinessErr error
	deps.DaemonStatus = func(ctx context.Context, cfg config.Config) (doctor.DaemonStatus, error) {
		if readinessErr != nil {
			return doctor.DaemonStatus{}, readinessErr
		}
		return diagnose(ctx, cfg)
	}
	return newDoctorCommandWithDependencies(opt, deps, daemonReadinessDoctor(opt, &readinessErr))
}

func newDoctorCommandWithDependencies(opt *options, deps doctor.IntegrationDependencies, readiness doctorReadinessRunner) *cobra.Command {
	return &cobra.Command{
		Use:         "doctor",
		Short:       "Check acquisition readiness and local integrations",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report := runDoctor(cmd.Context(), deps, readiness)
			if opt.jsonOutput {
				if err := opt.printJSON(report); err != nil {
					return err
				}
			} else if err := renderDoctorReport(opt.out, report); err != nil {
				return err
			}
			if !report.OK {
				return errDoctorFailed
			}
			return nil
		},
	}
}

func runDoctor(ctx context.Context, deps doctor.IntegrationDependencies, readiness doctorReadinessRunner) doctor.Report {
	readinessReport := doctor.Report{OK: true}
	if deps.LoadConfig != nil && readiness != nil {
		cfg, err := deps.LoadConfig()
		if err == nil {
			readinessReport = readiness(ctx, cfg)
		}
	}
	integrationReport := doctor.RunIntegration(ctx, deps)
	return doctor.Report{
		OK:     readinessReport.OK && integrationReport.OK,
		Checks: append(readinessReport.Checks, integrationReport.Checks...),
	}
}

func renderDoctorReport(out io.Writer, report doctor.Report) error {
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out, "%-4s  %-24s %s\n", strings.ToUpper(check.Status), check.Name, check.Detail); err != nil {
			return err
		}
		if check.Remediation != "" {
			if _, err := fmt.Fprintf(out, "      fix: %s\n", check.Remediation); err != nil {
				return err
			}
		}
	}
	return nil
}
