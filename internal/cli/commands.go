// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/agentjson"
	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/browser"
	"papio/internal/ipc"
	"papio/internal/job"
)

// jobsFailuresResult decodes the daemon reply. internal/api/failures.go sends
// `since` whenever the caller supplied a resolvable window, so the field must
// be here for ipc.DecodeResult's DisallowUnknownFields to accept the payload
// at all — deleting it does not remove the key from the wire, it just makes
// every `--since`-bearing call fail with "unknown field \"since\"" before
// either renderer runs. It is deliberately NOT re-emitted on the OUTPUT side:
// the page shape is supplied by printPage, and a metadata key that appears
// only on some invocations is exactly the shape drift the one-envelope
// contract exists to remove.
type jobsFailuresResult struct {
	Failures []job.FailureGroup `json:"failures"`
	Since    string             `json:"since,omitempty"`
}

// listJobsPage and listActionsPage prefer the _v2 methods, whose `truncated` is
// a proof: the daemon reached one row past the limit to answer it. An older
// daemon that predates them answers unknown_method, and the v1 fallback's flag
// degrades to agentjson.Capped's "there may be more" — same remedy either way
// (raise --limit), so the fallback is honest rather than absent. Same shape as
// the acquire.submit_v2 fallback in acquire.go.
func listJobsPage(ctx context.Context, opt *options, params map[string]any, effective int) ([]job.Row, bool, error) {
	var page api.JobsPage
	err := opt.call(ctx, "jobs.list_v2", params, &page)
	if err == nil {
		return page.Jobs, page.Truncated, nil
	}
	if !isUnknownMethod(err) {
		return nil, false, err
	}
	var rows []job.Row
	if err := opt.call(ctx, "jobs.list", params, &rows); err != nil {
		return nil, false, err
	}
	capped, truncated := agentjson.Capped(rows, effective)
	return capped, truncated, nil
}

func listActionsPage(ctx context.Context, opt *options, openOnly bool, limit int) ([]job.HumanAction, bool, error) {
	var page api.ActionsPage
	err := opt.call(ctx, "actions.list_v2", map[string]any{"open_only": openOnly, "limit": limit}, &page)
	if err == nil {
		return page.Actions, page.Truncated, nil
	}
	if !isUnknownMethod(err) {
		return nil, false, err
	}
	// actions.list is unbounded, so an older daemon returns the complete set and
	// there is nothing to be truncated against.
	var actions []job.HumanAction
	if err := opt.call(ctx, "actions.list", map[string]bool{"open_only": openOnly}, &actions); err != nil {
		return nil, false, err
	}
	return actions, false, nil
}

func isUnknownMethod(err error) bool {
	var remote *ipc.RemoteError
	return errors.As(err, &remote) && remote.Code == "unknown_method"
}

func daemonUpgradeRequired(method string) error {
	return fmt.Errorf("%s is unavailable because the running daemon predates it; upgrade or restart the daemon from the same installation as this CLI", method)
}

func newJobsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "jobs", Short: "Inspect and control acquisition jobs"}
	receiptCommand := &cobra.Command{
		Use:         "receipt <job-id>",
		Short:       "Show the outcome and component index for one job",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var receipt api.Receipt
			if err := opt.call(cmd.Context(), "jobs.receipt", map[string]string{"job_id": args[0]}, &receipt); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("jobs.receipt")
				}
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(receipt)
			}
			if _, err := fmt.Fprintf(opt.out, "%s\t%s", receipt.JobID, receipt.State); err != nil {
				return err
			}
			if receipt.TerminalReason != "" {
				if _, err := fmt.Fprintf(opt.out, "\t%s", receipt.TerminalReason); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(opt.out); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(opt.out, "principal\t%s\n", receipt.Principal); err != nil {
				return err
			}
			// "none" rather than an empty column: for a job that never reached a
			// source, the absence IS the finding, and a bare trailing tab reads
			// like the value failed to render.
			tiers := "none"
			if len(receipt.AttemptedTiers) != 0 {
				tiers = strings.Join(receipt.AttemptedTiers, ",")
			}
			if _, err := fmt.Fprintf(opt.out, "attempted tiers\t%s\n", tiers); err != nil {
				return err
			}
			for _, component := range receipt.Components {
				if _, err := fmt.Fprintf(opt.out, "%s\t%s\n", component.Role, component.SHA256); err != nil {
					return err
				}
			}
			if receipt.BundleAvailable {
				_, err := fmt.Fprintf(opt.out, "main-artifact provenance bundle: papio bundle export %s --output <dir>\n", receipt.JobID)
				return err
			}
			return nil
		},
	}

	repairAwaitingHuman := &cobra.Command{
		Use:   "repair-awaiting-human <job-id>",
		Short: "Return an orphaned awaiting-human job with no open actions to resolving",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.RepairResult
			if err := opt.call(cmd.Context(), "jobs.repair_awaiting_human", map[string]string{"job_id": args[0]}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("jobs.repair_awaiting_human")
				}
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			if result.State == "" {
				_, err := fmt.Fprintf(opt.out, "%s\t%s\n", result.JobID, result.Outcome)
				return err
			}
			_, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\n", result.JobID, result.Outcome, result.State)
			return err
		},
	}

	var componentRole string
	addComponent := &cobra.Command{
		Use:   "add-component <job-id> <path>",
		Short: "Add a supplement or appendix to a job",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result struct {
				Components []job.Component `json:"components"`
				Truncated  bool            `json:"truncated"`
			}
			if err := opt.call(cmd.Context(), "jobs.add_component", map[string]string{"job_id": args[0], "path": args[1], "role": componentRole}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("jobs.add_component")
				}
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "components", result.Components, result.Truncated)
			}
			for _, component := range result.Components {
				if _, err := fmt.Fprintf(opt.out, "%s\t%s\n", component.Role, component.SHA256); err != nil {
					return err
				}
			}
			return nil
		},
	}
	addComponent.Flags().StringVar(&componentRole, "role", "", "component role: supplement or appendix")
	_ = addComponent.MarkFlagRequired("role")

	var state string
	var limit int
	list := &cobra.Command{
		Use:         "list",
		Short:       "List jobs",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			effective := effectiveLimit(limit, job.ListLimitMax, job.ListLimitDefault)
			rows, truncated, err := listJobsPage(cmd.Context(), opt, map[string]any{"state": state, "limit": effective}, effective)
			if err != nil {
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "jobs", rows, truncated)
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

	newGetCommand := func(verb string) *cobra.Command {
		var wait bool
		command := &cobra.Command{
			Use:         verb + " <job-id>",
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
		command.Flags().BoolVar(&wait, "wait", false, "wait for completion or human action")
		return command
	}
	get := newGetCommand("get")
	show := newGetCommand("show")

	cancel := &cobra.Command{
		Use:   "cancel <job-id>",
		Short: "Cancel a nonterminal job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result map[string]any
			if err := opt.call(cmd.Context(), "jobs.cancel", map[string]string{"job_id": args[0]}, &result); err != nil {
				return err
			}
			// Store.Cancel is a documented idempotent no-op once a job is
			// terminal, which is right for the API and a lie in this sentence:
			// a job that finished a moment earlier reported "Cancelled" and an
			// operator believed they had stopped it. Read the state back and
			// say what actually happened. The result cannot carry the answer
			// because jobs.cancel's shape is fixed for older CLIs.
			// Decoded into the full api.JobDetail deliberately: IPC results
			// reject unknown fields recursively, so a narrow ad-hoc struct
			// declaring only what this line needs is rejected wholesale.
			after := &api.JobDetail{}
			if err := opt.call(cmd.Context(), "jobs.get", map[string]string{"job_id": args[0]}, after); err != nil {
				return opt.printResult(result, "Cancel requested for %s", args[0])
			}
			if after.Job == nil {
				return opt.printResult(result, "Cancel requested for %s", args[0])
			}
			if after.Job.State != string(job.StateCancelled) {
				return opt.printResult(result, "%s was already %s; nothing to cancel", args[0], after.Job.State)
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
	var failuresSince string
	var failuresLimit int
	failures := &cobra.Command{
		Use:         "failures",
		Short:       "Group acquisition jobs that need attention",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			effective := effectiveLimitFloored(failuresLimit, job.FailuresLimitMax, job.FailuresLimitDefault)
			var result jobsFailuresResult
			if err := opt.call(cmd.Context(), "jobs.failures", map[string]any{"since": failuresSince, "limit": effective}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				rows, truncated := agentjson.Capped(result.Failures, effective)
				return printPage(opt, "failures", rows, truncated)
			}
			for _, group := range result.Failures {
				if _, err := fmt.Fprintf(opt.out, "%d | %s | %s | %s (sample: %s)\n", group.Count, group.State, group.Provider, group.Reason, group.Sample); err != nil {
					return err
				}
			}
			return nil
		},
	}
	failures.Flags().StringVar(&failuresSince, "since", "", "include jobs updated since a duration or RFC3339 timestamp")
	failures.Flags().IntVar(&failuresLimit, "limit", 50, "maximum groups (1-200)")

	command.AddCommand(list, get, show, cancel, retry, failures, receiptCommand, repairAwaitingHuman, addComponent)
	return command
}

func newActionsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "actions", Short: "Inspect required human actions"}
	var all bool
	var actionsLimit int
	list := &cobra.Command{
		Use:         "list",
		Short:       "List open human actions",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			openOnly := !all
			effective := effectiveLimit(actionsLimit, job.ListLimitMax, job.ListLimitDefault)
			actions, truncated, err := listActionsPage(cmd.Context(), opt, openOnly, effective)
			if err != nil {
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "actions", actions, truncated)
			}
			for _, action := range actions {
				if _, err := fmt.Fprintf(opt.out, "%d\t%s\t%s\t%s%s\n", action.ID, action.JobID, action.Kind, action.Status, accessHint(action)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include resolved actions")
	list.Flags().IntVar(&actionsLimit, "limit", job.ListLimitDefault, "maximum rows (1-500)")

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

	var revision int64
	dismiss := &cobra.Command{
		Use:   "dismiss <action-id>",
		Short: "Close a stale human action without touching its job",
		Long: "Close a stale human action without touching its job.\n\n" +
			"An advisory on a terminal job has no other way out: cancel refuses a\n" +
			"terminal job, resolve is identity-review only, and the startup sweep\n" +
			"deliberately leaves informational advisories alone so a real trace\n" +
			"survives. Without this, retiring one meant editing the database or\n" +
			"retrying the job purely to cancel it again.\n\n" +
			"--revision guards against dismissing an action that changed after you\n" +
			"listed it; take it from `papio actions list --json`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			actionID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || actionID <= 0 {
				return errors.New("action-id must be a positive integer")
			}
			if revision <= 0 {
				return errors.New("--revision is required; take it from `papio actions list --json`")
			}
			var result map[string]any
			if err := opt.call(cmd.Context(), "actions.dismiss",
				map[string]any{"action_id": actionID, "expected_revision": revision}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "dismissed action %d on %s", actionID, result["job_id"])
		},
	}
	dismiss.Flags().Int64Var(&revision, "revision", 0, "revision the action had when you listed it")

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
			rows, rowsTruncated, err := listJobsPage(cmd.Context(), opt,
				map[string]any{"state": job.StateAwaitingHuman, "limit": job.ListLimitMax}, job.ListLimitMax)
			if err != nil {
				return err
			}
			targets, droppedForMissingJob := actionHandoffTargets(actions, rows, cfg.OpenURLBaseFor, limit)
			urls := make([]string, 0, len(targets))
			untrackedURLs := make([]string, 0, len(targets))
			jobIDs := make([]string, 0, len(targets))
			for _, target := range targets {
				urls = append(urls, target.URL)
				if target.Tracked {
					jobIDs = append(jobIDs, target.JobID)
				} else {
					untrackedURLs = append(untrackedURLs, target.URL)
				}
			}
			urls, urlsTruncated := agentjson.Capped(urls, limit)
			truncated := urlsTruncated || droppedForMissingJob > 0 || rowsTruncated
			if len(urls) == 0 && len(actions) > 0 && !opt.jsonOutput {
				if _, err := fmt.Fprintf(opt.out, "%d open action(s), none openable from here — run 'papio actions list' for details\n", len(actions)); err != nil {
					return err
				}
				return nil
			}
			if dryRun && opt.jsonOutput {
				return printPage(opt, "urls", urls, truncated)
			}
			if err := focusOrOpenActionURLs(cmd.Context(), urls, untrackedURLs, jobIDs, dryRun, opt.out, func(ctx context.Context, ids []string) (api.ActionsOpenResult, error) {
				var result api.ActionsOpenResult
				if err := opt.call(ctx, "actions.open", map[string]any{"job_ids": ids}, &result); err != nil {
					return api.ActionsOpenResult{}, err
				}
				return result, nil
			}, commandExec); err != nil {
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "urls", urls, truncated)
			}
			return nil
		},
	}
	open.Flags().IntVar(&limit, "limit", 0, "maximum actions to open (default all)")
	open.Flags().BoolVar(&dryRun, "dry-run", false, "print URLs without opening them")

	resolve.Flags().BoolVar(&reject, "reject", false, "reject the identity review")
	command.AddCommand(list, resolve, dismiss, open)
	return command
}

// accessHint renders the access classification as the user's actual next step,
// so a row in `papio actions list` is self-contained — that listing has no
// detail column, so this line is the whole instruction.
//
// It uses app.HumanActionNextStepFor so a replacement manual-download action
// cannot inherit its old handoff's command just because both require a login.
func accessHint(action job.HumanAction) string {
	if action.BlockedBy == "" {
		return ""
	}
	next := app.HumanActionNextStepFor(action)
	switch {
	case next.Instruction != "" && next.RequiresInstitutionalLogin:
		return "\tsign in to your institution, then " + next.Instruction
	case next.Instruction != "":
		return "\t" + next.Instruction + "; no login needed"
	case next.RequiresInstitutionalLogin && next.Command != "":
		return "\tsign in to your institution first, then '" + next.Command + "'"
	case next.RequiresInstitutionalLogin:
		return "\tsign in to your institution first"
	default:
		return "\topen access — no login needed"
	}
}

const openURLTimeout = 5 * time.Second

type commandRunner func(context.Context, string, ...string) error

// actionHandoffTargets resolves the openable handoffs and retains their job IDs
// newest actions first, up to limit (0 = unbounded). droppedForMissingJob
// counts open actions whose job id was not present in rows: either the job
// has moved past awaiting_human since the action was recorded (a routine,
// self-resolving race the caller cannot act on) or rows itself was bounded
// and omitted a still-awaiting_human job. Both mean the caller cannot see
// the complete open-action picture, so a nonzero count should fold into the
// caller's own `truncated` signal.
type actionHandoffTarget struct {
	JobID   string
	URL     string
	Tracked bool
}

func actionHandoffTargets(actions []job.HumanAction, rows []job.Row, baseFor func(string) (string, bool), limit int) (targets []actionHandoffTarget, droppedForMissingJob int) {
	jobs := make(map[string]job.Row, len(rows))
	for _, row := range rows {
		jobs[row.ID] = row
	}
	targets = make([]actionHandoffTarget, 0, len(actions))
	for _, action := range actions {
		if action.Status != "open" {
			continue
		}
		row, ok := jobs[action.JobID]
		if !ok {
			droppedForMissingJob++
			continue
		}
		if row.State != job.StateAwaitingHuman {
			continue
		}
		target, ok := actionURL(action, row, baseFor)
		if !ok {
			continue
		}
		targets = append(targets, actionHandoffTarget{JobID: action.JobID, URL: target, Tracked: action.Kind == "openurl_handoff"})
		if limit > 0 && len(targets) >= limit {
			break
		}
	}
	return targets, droppedForMissingJob
}

// actionURLs preserves the URL-only helper used by dry-run and JSON rendering.
func actionURLs(actions []job.HumanAction, rows []job.Row, baseFor func(string) (string, bool), limit int) (urls []string, droppedForMissingJob int) {
	targets, droppedForMissingJob := actionHandoffTargets(actions, rows, baseFor, limit)
	urls = make([]string, 0, len(targets))
	for _, target := range targets {
		urls = append(urls, target.URL)
	}
	return urls, droppedForMissingJob
}

func actionURL(action job.HumanAction, row job.Row, baseFor func(string) (string, bool)) (string, bool) {
	if direct, ok := app.OABrowserHandoffURL(action.Detail); ok {
		return direct, true
	}
	if detail := strings.TrimSpace(action.Detail); validOpenURL(detail) {
		return detail, true
	}
	if app.HumanActionNextStepFor(action).Command == "" {
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

type focusHandoffs func(context.Context, []string) (api.ActionsOpenResult, error)

var errNoConnectedBrowserSession = errors.New("no connected browser extension session - open Chrome with the papio extension enabled; check papio doctor")

func focusOrOpenActionURLs(ctx context.Context, urls, untrackedURLs, jobIDs []string, dryRun bool, out io.Writer, focus focusHandoffs, run commandRunner) error {
	if dryRun {
		return openActionURLs(ctx, urls, true, out, run)
	}
	if len(jobIDs) == 0 {
		return openActionURLs(ctx, urls, false, out, run)
	}
	result, err := focus(ctx, jobIDs)
	if err == nil && result.SessionLive {
		// The daemon skips a parked job whose access mode cannot be expressed
		// as a handoff offer. Falling back to the OS browser for those would
		// open exactly the institutional access the mode forbids, so they are
		// reported instead. Silently opening nothing was the prior behaviour
		// and it told the user papio had acted when it had not.
		if skipped := len(jobIDs) - result.Queued; skipped > 0 {
			if _, err := fmt.Fprintf(out,
				"%d of %d handoffs were not opened: the job's access mode does not permit an institutional handoff\n",
				skipped, len(jobIDs)); err != nil {
				return err
			}
		}
		return openActionURLs(ctx, untrackedURLs, false, out, run)
	}
	if err == nil {
		return errNoConnectedBrowserSession
	}
	if err != nil {
		var remote *ipc.RemoteError
		if !errors.As(err, &remote) || remote.Code != "unknown_method" {
			return err
		}
		// A newer CLI can meet an older daemon that predates this additive RPC;
		// fall back rather than making that ordinary skew strand the handoff.
	}
	return openActionURLs(ctx, urls, false, out, run)
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
		name, args := browserOpenCommand(target)
		err := run(bounded, name, args...)
		cancel()
		if err != nil {
			return fmt.Errorf("browser handoff could not open — open your browser with the papio extension enabled (papio doctor), then retry: %w", err)
		}
	}
	return nil
}

// browserOpenCommand returns the platform launcher for a handoff URL. macOS
// pins Chrome — the papio extension lives there and handoff tabs must open
// where the extension can adopt their downloads. Other platforms hand the URL
// to the default browser; papio doctor verifies the extension host.
func browserOpenCommand(target string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{"-b", chromeBundleID, target}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		return "xdg-open", []string{target}
	}
}

// chromeBundleID pins macOS handoff tabs to the browser hosting the papio
// extension. The native-messaging host manifests are Chrome-scoped today; if
// other Chromium channels are ever supported this becomes configuration.
const chromeBundleID = "com.google.Chrome"

func commandExec(ctx context.Context, name string, args ...string) error {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		if trimmed := strings.TrimSpace(string(output)); trimmed != "" {
			if len(trimmed) > 200 {
				trimmed = trimmed[:200]
			}
			return fmt.Errorf("%s: %w: %s", name, err, trimmed)
		}
		// Name the command even without output: a bare "exit status 1" is
		// undebuggable from the CLI surface.
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
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
	locate := &cobra.Command{
		Use:   "locate <job-id>",
		Short: "Print where one job's validated artifact bytes live",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.ArtifactLocation
			if err := opt.call(cmd.Context(), "artifacts.locate",
				map[string]string{"job_id": args[0]}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("artifacts.locate")
				}
				return err
			}
			return opt.printResult(result, "%s\t%s\t%d bytes", result.SHA256, result.Path, result.SizeBytes)
		},
	}
	command.AddCommand(locate)
	command.AddCommand(get)
	return command
}

func newBundleCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "bundle", Short: "Read and export validated acquisition bundles"}
	var outputDir string
	export := &cobra.Command{
		Use:   "export <job-id>",
		Short: "Export an idempotent bundle directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(outputDir) == "" {
				return errors.New("--output is required")
			}
			result, err := exportBundleResult(cmd.Context(), opt, args[0], outputDir)
			if err != nil {
				return err
			}
			return opt.printResult(result, "Exported %s", result.Path)
		},
	}
	export.Flags().StringVarP(&outputDir, "output", "o", "", "destination directory")
	document := &cobra.Command{
		Use:   "document <job-id>",
		Short: "Print the acquisition bundle without writing it anywhere",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.DocumentResult
			if err := opt.call(cmd.Context(), "bundle.document",
				map[string]string{"job_id": args[0]}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("bundle.document")
				}
				return err
			}
			// The document already ends with the newline bundle.json is written
			// with, so it is emitted verbatim rather than through printResult,
			// which would append a second one. `papio bundle document <job> >
			// bundle.json` must reproduce the exported file byte for byte.
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			_, err := io.WriteString(opt.out, result.Document)
			return err
		},
	}
	command.AddCommand(document)
	command.AddCommand(export)
	return command
}

// exportBundleResult prefers bundle.export_v2, which carries the exported
// document in its result. bundle.export deliberately withholds that body so an
// older CLI can decode a newer daemon's response, so a newer CLI must ask for
// it by name — and fall back when the daemon predates the method, exactly as
// submitAcquire does for acquire.submit_v2.
func exportBundleResult(ctx context.Context, opt *options, jobID, outputDir string) (api.BundleResult, error) {
	params := map[string]string{"job_id": jobID, "output_dir": outputDir}
	var result api.BundleResult
	err := opt.call(ctx, "bundle.export_v2", params, &result)
	if err == nil {
		return result, nil
	}
	var remote *ipc.RemoteError
	if !errors.As(err, &remote) || remote.Code != "unknown_method" {
		return api.BundleResult{}, err
	}
	var legacy api.BundleResult
	if err := opt.call(ctx, "bundle.export", params, &legacy); err != nil {
		return api.BundleResult{}, err
	}
	return legacy, nil
}
