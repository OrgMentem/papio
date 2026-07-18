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
	"papio/internal/pdf"
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
			// callExisting deliberately avoids daemon.NewAutostarter: doctor must
			// diagnose a stopped daemon, not hide it by starting one.
			var status doctor.DaemonStatus
			if err := opt.callExisting(ctx, "ping", struct{}{}, &status); err != nil {
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

func runReadinessDoctor(ctx context.Context, cfg config.Config) doctor.Report {
	// Opening the store here could create or migrate the database. Doctor should
	// inspect a local installation without mutating it, so Run emits its
	// established database warning instead.
	return doctor.Run(ctx, cfg, nil, pdf.DetectCapability(), doctor.DefaultWorkerPath())
}

func newDoctorCommand(opt *options) *cobra.Command {
	return newDoctorCommandWithDependencies(opt, defaultDoctorDependencies(opt), runReadinessDoctor)
}

func newDoctorCommandWithDependencies(opt *options, deps doctor.IntegrationDependencies, readiness doctorReadinessRunner) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check acquisition readiness and local integrations",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:  cobra.NoArgs,
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
