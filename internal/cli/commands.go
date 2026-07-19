// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/browser"
	"papio/internal/job"
)

func newJobsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "jobs", Short: "Inspect and control acquisition jobs"}
	var state string
	var limit int
	list := &cobra.Command{
		Use:         "list",
		Short:       "List jobs",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
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
		Use:         "get <job-id>",
		Short:       "Show one job with events and actions",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
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
		Use:         "list",
		Short:       "List open human actions",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
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

	var accept, reject bool
	resolve := &cobra.Command{
		Use:   "resolve <action-id>",
		Short: "Accept or reject a parked identity review",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if accept == reject {
				return errors.New("exactly one of --accept or --reject is required")
			}
			actionID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || actionID <= 0 {
				return errors.New("action-id must be a positive integer")
			}
			verdict := "reject"
			if accept {
				verdict = "accept"
			}
			var result map[string]any
			if err := opt.call(cmd.Context(), "actions.resolve",
				map[string]any{"action_id": actionID, "verdict": verdict}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "%s\t%s", result["job_id"], result["state"])
		},
	}
	resolve.Flags().BoolVar(&accept, "accept", false, "accept the identity review")

	var limit int
	var dryRun bool
	open := &cobra.Command{
		Use:   "open",
		Short: "Open the current browser handoff queue",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 0 {
				return errors.New("--limit must be non-negative")
			}
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			var actions []job.HumanAction
			if err := opt.call(cmd.Context(), "actions.list", map[string]bool{"open_only": true}, &actions); err != nil {
				return err
			}
			var rows []job.Row
			if err := opt.call(cmd.Context(), "jobs.list", map[string]any{"limit": 500}, &rows); err != nil {
				return err
			}
			urls := actionURLs(actions, rows, cfg.OpenURLBaseFor, limit)
			if dryRun && opt.jsonOutput {
				return opt.printJSON(urls)
			}
			if err := openActionURLs(cmd.Context(), urls, dryRun, opt.out, commandExec); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(urls)
			}
			return nil
		},
	}
	open.Flags().IntVar(&limit, "limit", 0, "maximum actions to open (default all)")
	open.Flags().BoolVar(&dryRun, "dry-run", false, "print URLs without opening them")

	resolve.Flags().BoolVar(&reject, "reject", false, "reject the identity review")
	command.AddCommand(list, resolve, open)
	return command
}

const openURLTimeout = 5 * time.Second

type commandRunner func(context.Context, string, ...string) error

func actionURLs(actions []job.HumanAction, rows []job.Row, baseFor func(string) (string, bool), limit int) []string {
	jobs := make(map[string]job.Row, len(rows))
	for _, row := range rows {
		jobs[row.ID] = row
	}
	urls := make([]string, 0, len(actions))
	for _, action := range actions {
		row, ok := jobs[action.JobID]
		if !ok || action.Status != "open" || row.State != job.StateAwaitingHuman {
			continue
		}
		target, ok := actionURL(action, row, baseFor)
		if !ok {
			continue
		}
		urls = append(urls, target)
		if limit > 0 && len(urls) >= limit {
			break
		}
	}
	return urls
}

func actionURL(action job.HumanAction, row job.Row, baseFor func(string) (string, bool)) (string, bool) {
	if direct, ok := app.OABrowserHandoffURL(action.Detail); ok {
		return direct, true
	}
	if detail := strings.TrimSpace(action.Detail); validOpenURL(detail) {
		return detail, true
	}
	if action.Kind != "openurl_handoff" {
		return "", false
	}
	// Honor the job's resolver profile: a Example Institute-routed job must never open the
	// default (Example University) resolver.
	base, ok := baseFor(row.Policy.Resolver)
	if !ok || base == "" {
		return "", false
	}
	target := browser.OpenURL(base, row.Work)
	return target, validOpenURL(target)
}

func validOpenURL(value string) bool {
	if len(value) == 0 || len(value) > 4000 {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}

func openActionURLs(ctx context.Context, urls []string, dryRun bool, out io.Writer, run commandRunner) error {
	for _, target := range urls {
		if dryRun {
			if _, err := fmt.Fprintln(out, target); err != nil {
				return err
			}
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, openURLTimeout)
		// The papio extension lives in Chrome; the OS default browser may not.
		// Handoff tabs must open where the extension can adopt their downloads.
		err := run(bounded, "open", "-b", chromeBundleID, target)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

// chromeBundleID pins handoff tabs to the browser hosting the papio
// extension. The native-messaging host manifests are Chrome-scoped today; if
// other Chromium channels are ever supported this becomes configuration.
const chromeBundleID = "com.google.Chrome"

func commandExec(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

func newArtifactsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "artifacts", Short: "Inspect validated immutable artifacts"}
	var sha bool
	get := &cobra.Command{
		Use:         "get <job-id-or-sha256>",
		Short:       "Show a validated artifact",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
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
