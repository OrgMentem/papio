// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/doctor"
	"papio/internal/job"
)

func newJobsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "jobs", Short: "Inspect and control acquisition jobs"}
	var state string
	var limit int
	list := &cobra.Command{
		Use:   "list",
		Short: "List jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var rows []job.Row
			if err := opt.call(cmd.Context(), "jobs.list", map[string]any{"state": state, "limit": limit}, &rows); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(rows)
			}
			for _, row := range rows {
				if _, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\n", row.ID, row.State, row.Work.Describe()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	list.Flags().StringVar(&state, "state", "", "filter by exact job state")
	list.Flags().IntVar(&limit, "limit", 100, "maximum rows (1-500)")

	var wait bool
	get := &cobra.Command{
		Use:   "get <job-id>",
		Short: "Show one job with events and actions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var detail *api.JobDetail
			if wait {
				var err error
				detail, err = waitForJob(cmd.Context(), opt, args[0])
				if err != nil {
					return err
				}
			} else {
				detail = &api.JobDetail{}
				if err := opt.call(cmd.Context(), "jobs.get", map[string]string{"job_id": args[0]}, detail); err != nil {
					return err
				}
			}
			if opt.jsonOutput {
				return opt.printJSON(detail)
			}
			if _, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\n", detail.Job.ID, detail.Job.State, detail.Job.Work.Describe()); err != nil {
				return err
			}
			for _, event := range detail.Events {
				if _, err := fmt.Fprintf(opt.out, "  %v  %v\n", event["at"], event["kind"]); err != nil {
					return err
				}
			}
			return nil
		},
	}
	get.Flags().BoolVar(&wait, "wait", false, "wait for completion or human action")

	cancel := &cobra.Command{
		Use:   "cancel <job-id>",
		Short: "Cancel a nonterminal job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result map[string]any
			if err := opt.call(cmd.Context(), "jobs.cancel", map[string]string{"job_id": args[0]}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "Cancelled %s", args[0])
		},
	}

	retry := &cobra.Command{
		Use:   "retry <job-id>",
		Short: "Explicitly retry a failed, unavailable, or retry-wait job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result map[string]any
			if err := opt.call(cmd.Context(), "jobs.retry", map[string]string{"job_id": args[0]}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "Retrying %s", args[0])
		},
	}
	command.AddCommand(list, get, cancel, retry)
	return command
}

func newActionsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "actions", Short: "Inspect required human actions"}
	var all bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List open human actions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			openOnly := !all
			var actions []job.HumanAction
			if err := opt.call(cmd.Context(), "actions.list", map[string]bool{"open_only": openOnly}, &actions); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(actions)
			}
			for _, action := range actions {
				if _, err := fmt.Fprintf(opt.out, "%d\t%s\t%s\t%s\n", action.ID, action.JobID, action.Kind, action.Status); err != nil {
					return err
				}
			}
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include resolved actions")
	command.AddCommand(list)
	return command
}

func newArtifactsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "artifacts", Short: "Inspect validated immutable artifacts"}
	var sha bool
	get := &cobra.Command{
		Use:   "get <job-id-or-sha256>",
		Short: "Show a validated artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]string{"job_id": args[0]}
			if sha {
				params = map[string]string{"sha256": args[0]}
			}
			var result api.ArtifactResult
			if err := opt.call(cmd.Context(), "artifacts.get", params, &result); err != nil {
				return err
			}
			return opt.printResult(result, "%s\t%s\t%d bytes", result.Artifact.SHA256, result.Artifact.Path, result.Artifact.SizeBytes)
		},
	}
	get.Flags().BoolVar(&sha, "sha256", false, "interpret argument as an artifact hash")
	command.AddCommand(get)
	return command
}

func newBundleCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "bundle", Short: "Export validated acquisition bundles"}
	var outputDir string
	export := &cobra.Command{
		Use:   "export <job-id>",
		Short: "Export an idempotent bundle directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(outputDir) == "" {
				return errors.New("--output is required")
			}
			var result api.BundleResult
			if err := opt.call(cmd.Context(), "bundle.export", map[string]string{"job_id": args[0], "output_dir": outputDir}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "Exported %s", result.Path)
		},
	}
	export.Flags().StringVarP(&outputDir, "output", "o", "", "destination directory")
	command.AddCommand(export)
	return command
}

func newDoctorCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check acquisition-core readiness",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var report doctor.Report
			if err := opt.call(cmd.Context(), "doctor.run", struct{}{}, &report); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(report)
			}
			for _, check := range report.Checks {
				if _, err := fmt.Fprintf(opt.out, "%-4s  %-24s %s\n", strings.ToUpper(check.Status), check.Name, check.Detail); err != nil {
					return err
				}
				if check.Remediation != "" {
					if _, err := fmt.Fprintf(opt.out, "      %s\n", check.Remediation); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
}
