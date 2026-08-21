// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/agentjson"
	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/config"
	"papio/internal/incident"
	"papio/internal/ipc"
	"papio/internal/job"
	"papio/internal/store"
)

// jobsFailuresResult decodes the daemon reply. internal/api/failures.go sends
// `since` whenever the caller supplied a resolvable window, so the field must
// be here for ipc.DecodeResult's DisallowUnknownFields to accept the payload
// at all — deleting it does not remove the key from the wire, it just makes
// every `--since`-bearing call fail with "unknown field \"since\"" before
// either renderer runs. It is deliberately NOT re-emitted on the OUTPUT side:
// the stable output shape is supplied by jobsFailuresPage, and metadata that
// appears only on some invocations is deliberately excluded from that shape.
type jobsFailuresResult struct {
	Failures []job.FailureGroup `json:"failures"`
	Since    string             `json:"since,omitempty"`
}

type jobsIncidentsResult struct {
	Incidents []incident.Group `json:"incidents"`
}

// jobsFailuresPage keeps the established ordinary failure rows under
// "failures" and adds incidents under their own stable collection. Both
// collections are present for every invocation, including incident-only and
// old-daemon fallback responses, so consumers never have to infer the row
// schema from current data.
type jobsFailuresPage struct {
	Failures  []job.FailureGroup `json:"failures"`
	Incidents []incident.Group   `json:"incidents"`
	Truncated bool               `json:"truncated"`
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
	if !errors.As(err, &remote) {
		return false
	}
	switch remote.Code {
	case "unknown_method", "method_not_found", "unsupported_method":
		return true
	default:
		return false
	}
}

// listAttributedJobsPage and listAttributedActionsPage prefer the _v3 methods,
// which carry the consumer that submitted each row (and, for actions, the
// daemon's staleness verdict). They fall back to the older methods so a newer
// CLI still lists against an older daemon — with one exception: a --consumer
// filter is refused rather than silently ignored, because returning every
// consumer's rows to someone who asked for one consumer's is the kind of wrong
// answer that gets believed.
func listAttributedJobsPage(ctx context.Context, opt *options, params map[string]any, consumer string, effective int) ([]api.JobRow, bool, error) {
	var page api.JobsPageV3
	err := opt.call(ctx, "jobs.list_v3", params, &page)
	if err == nil {
		return page.Jobs, page.Truncated, nil
	}
	if !isUnknownMethod(err) {
		return nil, false, err
	}
	if consumer != "" {
		return nil, false, daemonUpgradeRequired("jobs.list_v3")
	}
	delete(params, "consumer")
	rows, truncated, err := listJobsPage(ctx, opt, params, effective)
	if err != nil {
		return nil, false, err
	}
	out := make([]api.JobRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, api.JobRow{Row: row})
	}
	return out, truncated, nil
}

func listAttributedActionsPage(ctx context.Context, opt *options, openOnly bool, consumer string, limit int) ([]api.ActionRow, bool, error) {
	var page api.ActionsPageV3
	params := map[string]any{"open_only": openOnly, "limit": limit}
	if consumer != "" {
		params["consumer"] = consumer
	}
	err := opt.call(ctx, "actions.list_v3", params, &page)
	if err == nil {
		return page.Actions, page.Truncated, nil
	}
	if !isUnknownMethod(err) {
		return nil, false, err
	}
	if consumer != "" {
		return nil, false, daemonUpgradeRequired("actions.list_v3")
	}
	actions, truncated, err := listActionsPage(ctx, opt, openOnly, limit)
	if err != nil {
		return nil, false, err
	}
	out := make([]api.ActionRow, 0, len(actions))
	for _, action := range actions {
		out = append(out, api.ActionRow{HumanAction: action})
	}
	return out, truncated, nil
}

// jobDetailV3 prefers jobs.get_v3, which adds the delivery-request section
// (ADR-0017 Decision 5) on top of jobs.get_v2's attribution and staleness. It
// falls back through jobDetail (jobs.get_v2, then jobs.get) for an older
// daemon, same chain shape as listAttributedJobsPage over listJobsPage. It
// stays separate from jobDetail/waitForJob rather than widening their return
// type: those are shared with `papio acquire --wait` and `papio export job`,
// which Decision 5 names no delivery obligation for, and jobs.get_v3 is a new
// method precisely so an older reader of jobs.get_v2 never has to change.
func jobDetailV3(ctx context.Context, opt *options, id string) (*api.JobDetailV3, error) {
	params := map[string]string{"job_id": id}
	var detail api.JobDetailV3
	err := opt.call(ctx, "jobs.get_v3", params, &detail)
	if err == nil {
		return &detail, nil
	}
	if !isUnknownMethod(err) {
		return nil, err
	}
	v2, err := jobDetail(ctx, opt, id)
	if err != nil {
		return nil, err
	}
	return &api.JobDetailV3{Job: v2.Job, Events: v2.Events, Actions: v2.Actions}, nil
}

// waitForJobV3 is `jobs get --wait`'s poll loop: identical to waitForJob, but
// reading through jobDetailV3 so --wait renders the same delivery-aware shape
// as a bare `papio jobs get`.
func waitForJobV3(ctx context.Context, opt *options, id string) (*api.JobDetailV3, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		detail, err := jobDetailV3(ctx, opt, id)
		if err != nil {
			return nil, err
		}
		if detail.Job == nil {
			return nil, fmt.Errorf("daemon returned no job for %s", id)
		}
		if job.Terminal(detail.Job.State) || detail.Job.State == job.StateAwaitingHuman || detail.Job.State == job.StateNeedsReview {
			return detail, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// printDeliverySection renders `jobs get`'s human-output delivery lines: one
// labeled line per fact (provider, reference, state, next check, gate class
// with blockers), the same WORKING-style shape ADR-0017 Decision 5 describes
// for `papio status`. Absent (nil) prints nothing, matching the JSON side's
// omitempty.
func printDeliverySection(opt *options, delivery *api.DeliverySummary) error {
	if delivery == nil {
		return nil
	}
	if _, err := fmt.Fprintf(opt.out, "  delivery provider: %s\n", delivery.Provider); err != nil {
		return err
	}
	if delivery.Reference != "" {
		if _, err := fmt.Fprintf(opt.out, "  delivery reference: %s\n", delivery.Reference); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(opt.out, "  delivery state: %s\n", delivery.State); err != nil {
		return err
	}
	if delivery.NextCheckAt != "" {
		if _, err := fmt.Fprintf(opt.out, "  delivery next check: %s\n", delivery.NextCheckAt); err != nil {
			return err
		}
	}
	if delivery.GateClass != "" {
		gate := delivery.GateClass
		if len(delivery.GateBlockers) > 0 {
			gate = fmt.Sprintf("%s (blocked by: %s)", gate, strings.Join(delivery.GateBlockers, ", "))
		}
		if _, err := fmt.Fprintf(opt.out, "  delivery gate: %s\n", gate); err != nil {
			return err
		}
	}
	return nil
}

// truncationNotice states on the human surface what the --json envelope has
// always stated in its `truncated` key. A listing that quietly stopped at the
// limit and looked complete is how a consumer concludes the daemon holds a
// fraction of the rows it holds; the JSON side learned that lesson already.
func truncationNotice(opt *options, truncated bool, rows int, noun string) error {
	if !truncated || opt.jsonOutput {
		return nil
	}
	_, err := fmt.Fprintf(opt.out,
		"showing %d %s; more exist behind this page — raise --limit (max %d)\n",
		rows, noun, job.ListLimitMax)
	return err
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
	var jobsConsumer string
	list := &cobra.Command{
		Use:         "list",
		Short:       "List jobs",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			effective := effectiveLimit(limit, job.ListLimitMax, job.ListLimitDefault)
			consumer := strings.TrimSpace(jobsConsumer)
			params := map[string]any{"state": state, "limit": effective}
			if consumer != "" {
				params["consumer"] = consumer
			}
			rows, truncated, err := listAttributedJobsPage(cmd.Context(), opt, params, consumer, effective)
			if err != nil {
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "jobs", rows, truncated)
			}
			for _, row := range rows {
				consumerColumn := ""
				if row.Consumer != nil {
					consumerColumn = "\t" + *row.Consumer
				}
				// row.Work.Describe() renders the title/DOI/identifier that
				// identifies this job: Describe() falls back to a raw Title when
				// no strong identifier is set, and that Title is third-party
				// bibliographic metadata normalized with only strings.TrimSpace
				// (see the search-row comment in search.go). Route it through
				// StripTerminalControls on this JSON-free branch, or a poisoned
				// title injects ANSI/OSC escapes into `papio jobs list`. The
				// consumer label needs no strip: validConsumer pins it to a
				// control-free charset at submission.
				if _, err := fmt.Fprintf(opt.out, "%s\t%s\t%s%s\n", row.ID, row.State, store.StripTerminalControls(row.Work.Describe()), consumerColumn); err != nil {
					return err
				}
			}
			return truncationNotice(opt, truncated, len(rows), "job(s)")
		},
	}
	list.Flags().StringVar(&state, "state", "", "filter by exact job state")
	list.Flags().IntVar(&limit, "limit", 100, "maximum rows (1-500)")
	list.Flags().StringVar(&jobsConsumer, "consumer", "", "only jobs submitted under this consumer name")

	newGetCommand := func(verb string) *cobra.Command {
		var wait bool
		command := &cobra.Command{
			Use:         verb + " <job-id>",
			Short:       "Show one job with events and actions",
			Annotations: map[string]string{"mcp:read-only": "true"},
			Args:        cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				var detail *api.JobDetailV3
				var err error
				if wait {
					detail, err = waitForJobV3(cmd.Context(), opt, args[0])
				} else {
					detail, err = jobDetailV3(cmd.Context(), opt, args[0])
				}
				if err != nil {
					return err
				}
				if opt.jsonOutput {
					return opt.printJSON(detail)
				}
				consumerColumn := ""
				if detail.Job.Consumer != nil {
					consumerColumn = "\t" + *detail.Job.Consumer
				}
				// detail.Job.Work.Describe() carries the same third-party title
				// fallback as the jobs-list row above; strip it on this
				// JSON-free branch too.
				if _, err := fmt.Fprintf(opt.out, "%s\t%s\t%s%s\n", detail.Job.ID, detail.Job.State, store.StripTerminalControls(detail.Job.Work.Describe()), consumerColumn); err != nil {
					return err
				}
				for _, event := range detail.Events {
					if _, err := fmt.Fprintf(opt.out, "  %v  %v\n", event["at"], event["kind"]); err != nil {
						return err
					}
				}
				if err := printDeliverySection(opt, detail.Delivery); err != nil {
					return err
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
		Use:   "failures",
		Short: "Group acquisition jobs that need attention",
		Long: "Group acquisition jobs that need attention.\n\n" +
			"Incident fingerprints omit raw hosts and identifiers and are keyed per " +
			"installation; local output intentionally includes bounded safety_domain " +
			"and registrable host_family labels for diagnosis.",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			effective := effectiveLimitFloored(failuresLimit, job.FailuresLimitMax, job.FailuresLimitDefault)
			var result jobsFailuresResult
			if err := opt.call(cmd.Context(), "jobs.failures", map[string]any{"since": failuresSince, "limit": effective}, &result); err != nil {
				return err
			}
			var incidentsResult jobsIncidentsResult
			if err := opt.call(cmd.Context(), "jobs.incidents", map[string]any{"since": failuresSince, "limit": effective}, &incidentsResult); err != nil {
				if !isUnknownMethod(err) {
					return err
				}
				// Older daemons have no incident read model. Keep the
				// incidents collection empty rather than replacing or
				// suppressing the ordinary failure collection.
				incidentsResult.Incidents = []incident.Group{}
			}
			failureRows, failureTruncated := agentjson.Capped(result.Failures, effective)
			incidentRows, incidentTruncated := agentjson.Capped(incidentsResult.Incidents, effective)
			if opt.jsonOutput {
				return opt.printJSON(jobsFailuresPage{
					Failures:  failureRows,
					Incidents: incidentRows,
					Truncated: failureTruncated || incidentTruncated,
				})
			}
			for _, group := range failureRows {
				if _, err := fmt.Fprintf(opt.out, "%d | %s | %s | %s (sample: %s)\n",
					group.Count, group.State, group.Provider, group.Reason, group.Sample); err != nil {
					return err
				}
			}
			for _, group := range incidentRows {
				if _, err := fmt.Fprintf(opt.out, "%s | %s | %s | %s | %d | %s | %s\n",
					group.Fingerprint, group.SafetyDomain, group.HostFamily, group.Outcome,
					group.Jobs, group.FirstSeen.Format(time.RFC3339Nano), group.LastSeen.Format(time.RFC3339Nano)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	failures.Flags().StringVar(&failuresSince, "since", "", "include jobs updated since a duration or RFC3339 timestamp")
	failures.Flags().IntVar(&failuresLimit, "limit", 50, "maximum groups (1-200)")
	var incidentsSince string
	var incidentsLimit int
	incidents := &cobra.Command{
		Use:   "incidents",
		Short: "Group decisive provider incidents",
		Long: "Group decisive provider incidents by keyed failure shape.\n\n" +
			"The fingerprint omits raw hosts and identifiers and is keyed per " +
			"installation; local output intentionally includes bounded safety_domain " +
			"and registrable host_family labels for diagnosis.",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			effective := effectiveLimitFloored(incidentsLimit, job.FailuresLimitMax, job.FailuresLimitDefault)
			var result jobsIncidentsResult
			if err := opt.call(cmd.Context(), "jobs.incidents", map[string]any{"since": incidentsSince, "limit": effective}, &result); err != nil {
				if isUnknownMethod(err) {
					// Older daemons have no incident read model. Preserve the
					// separate surface with an empty page rather than
					// substituting legacy FailureGroup rows.
					if opt.jsonOutput {
						return printPage(opt, "incidents", []incident.Group{}, false)
					}
					return nil
				}
				return err
			}
			rows, truncated := agentjson.Capped(result.Incidents, effective)
			if opt.jsonOutput {
				return printPage(opt, "incidents", rows, truncated)
			}
			for _, group := range rows {
				if _, err := fmt.Fprintf(opt.out, "%s | %s | %s | %s | %d | %s | %s\n",
					group.Fingerprint, group.SafetyDomain, group.HostFamily, group.Outcome,
					group.Jobs, group.FirstSeen.Format(time.RFC3339Nano), group.LastSeen.Format(time.RFC3339Nano)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	incidents.Flags().StringVar(&incidentsSince, "since", "", "include incidents recorded since a duration or RFC3339 timestamp")
	incidents.Flags().IntVar(&incidentsLimit, "limit", 50, "maximum groups (1-200)")

	diagnose := newJobsDiagnoseCommand(opt)

	command.AddCommand(list, get, show, diagnose, cancel, retry, failures, incidents, receiptCommand, repairAwaitingHuman, addComponent)
	return command
}

func newActionsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "actions", Short: "Inspect required human actions"}
	var all bool
	var actionsLimit int
	var actionsConsumer string
	list := &cobra.Command{
		Use:   "list",
		Short: "List open human actions",
		Long: "List open human actions.\n\n" +
			"An action queued long enough to look abandoned is reported stale — " +
			"`age_seconds` and `stale` in --json, a trailing marker in the text " +
			"listing — against the configured actions.stale_after_seconds. " +
			"Staleness is a label and nothing else: no handoff is ever cancelled " +
			"on a timer, because giving up on an acquisition is your call.",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			openOnly := !all
			effective := effectiveLimit(actionsLimit, job.ListLimitMax, job.ListLimitDefault)
			actions, truncated, err := listAttributedActionsPage(cmd.Context(), opt, openOnly, strings.TrimSpace(actionsConsumer), effective)
			if err != nil {
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "actions", actions, truncated)
			}
			for _, action := range actions {
				if _, err := fmt.Fprintf(opt.out, "%d\t%s\t%s\t%s%s%s%s\n", action.ID, action.JobID, action.Kind, action.Status,
					consumerHint(action.Consumer), accessHint(action.HumanAction), staleHint(action)); err != nil {
					return err
				}
			}
			return truncationNotice(opt, truncated, len(actions), "action(s)")
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include resolved actions")
	list.Flags().IntVar(&actionsLimit, "limit", job.ListLimitDefault, "maximum rows (1-500)")
	list.Flags().StringVar(&actionsConsumer, "consumer", "", "only actions whose job was submitted under this consumer name")

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
	var openJobID string
	var openActionID int64
	open := &cobra.Command{
		Use:   "open",
		Short: "Open the current browser handoff queue",
		Long: "Open the current browser handoff queue.\n\n" +
			"With no selector this opens the whole openable queue, newest first.\n" +
			"--job or --action opens exactly one row instead, for a caller that\n" +
			"ranked the queue itself and wants the row it chose. A job holding\n" +
			"several open actions is refused with their ids rather than resolved by\n" +
			"picking one, and a selector naming no open action is an error: falling\n" +
			"back to the head of the queue would open somebody else's handoff and\n" +
			"report success.\n\n" +
			"The selector is for choosing a row, not for iterating the queue. A\n" +
			"background caller that loops it over every row has built the autonomous\n" +
			"drain ADR-0009 does not ratify: your browser is one serial surface, and\n" +
			"filling it with tabs nobody asked for is not acquisition progress.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 0 {
				return errors.New("--limit must be non-negative")
			}
			selector, err := newActionSelector(cmd, strings.TrimSpace(openJobID), openActionID)
			if err != nil {
				return err
			}
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			var actions []job.HumanAction
			if err := opt.call(cmd.Context(), "actions.list", map[string]bool{"open_only": true}, &actions); err != nil {
				return err
			}
			actions, err = selector.apply(actions)
			if err != nil {
				return err
			}
			rows, rowsTruncated, err := listJobsPage(cmd.Context(), opt,
				map[string]any{"state": job.StateAwaitingHuman, "limit": job.ListLimitMax}, job.ListLimitMax)
			if err != nil {
				return err
			}
			targets, droppedForMissingJob := actionHandoffTargets(actions, rows, cfg.InstitutionFor, limit)
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
				if _, err := fmt.Fprintf(opt.out, "%s, none openable from here — run 'papio actions list' for details\n", selector.describe(len(actions))); err != nil {
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
	open.Flags().StringVar(&openJobID, "job", "", "open only this job's open action")
	open.Flags().Int64Var(&openActionID, "action", 0, "open only this action id")

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
// The live database measured all 34 currently-open manual_download rows with a
// non-empty blocker (29 landing_page, 4 paywall, 1 anti_bot) on 2026-08-21, so
// this gap is not currently firing in the field. Keep the guard below
// defensive anyway: producers such as browser adoption can create an
// unblocked action, and a real next step must not disappear.
func accessHint(action job.HumanAction) string {
	next := app.HumanActionNextStepFor(action)
	if action.BlockedBy == "" && next.Instruction == "" && next.Command == "" {
		return ""
	}
	switch {
	case next.Instruction != "" && next.Command != "" && next.RequiresInstitutionalLogin:
		return "\t'" + next.Command + "', sign in to your institution, then " + next.Instruction
	case next.Instruction != "" && next.Command != "":
		return "\t'" + next.Command + "', then " + next.Instruction + "; no login needed"
	case next.Instruction != "" && next.RequiresInstitutionalLogin:
		return "\tsign in to your institution, then " + next.Instruction
	case next.Instruction != "":
		return "\t" + next.Instruction + "; no login needed"
	case next.RequiresInstitutionalLogin && next.Command != "":
		return "\tsign in to your institution first, then '" + next.Command + "'"
	case next.RequiresInstitutionalLogin:
		return "\tsign in to your institution first"
	case next.Command != "":
		// An open-access row with a command still needs the command named. It
		// used to read "open access — no login needed" and stop there, which
		// tells the reader nothing about how to fetch the paper.
		return "\topen access — no login needed; run '" + next.Command + "'"
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

func actionHandoffTargets(actions []job.HumanAction, rows []job.Row, instFor func(string) (config.Institution, bool), limit int) (targets []actionHandoffTarget, droppedForMissingJob int) {
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
		target, ok := actionURL(action, row, instFor)
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
func actionURLs(actions []job.HumanAction, rows []job.Row, instFor func(string) (config.Institution, bool), limit int) (urls []string, droppedForMissingJob int) {
	targets, droppedForMissingJob := actionHandoffTargets(actions, rows, instFor, limit)
	urls = make([]string, 0, len(targets))
	for _, target := range targets {
		urls = append(urls, target.URL)
	}
	return urls, droppedForMissingJob
}

func actionURL(action job.HumanAction, row job.Row, instFor func(string) (config.Institution, bool)) (string, bool) {
	return app.ResolveHumanActionURL(action, row, instFor)
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
		// The daemon skips a parked job it cannot express as a handoff offer.
		// Falling back to the OS browser for those would open exactly the
		// institutional access the mode forbids, so they are reported instead.
		// Silently opening nothing was the prior behaviour and it told the user
		// papio had acted when it had not. The shortfall does not name one cause:
		// the daemon also skips a job that is no longer awaiting a human, one
		// whose download already settled, and one held by a safety latch. Naming
		// access mode as the reason sent a reader chasing a consumed
		// authorization that did not exist.
		if skipped := len(jobIDs) - result.Queued; skipped > 0 {
			if _, err := fmt.Fprintf(out,
				"%d of %d handoffs were not opened: papio holds no institutional handoff it can offer for them right now - see papio jobs show <id> and papio doctor\n",
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
	validation := &cobra.Command{
		Use:         "validation <job-id>",
		Short:       "Print the full validation report for every candidate a job validated",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Long: "Print the full validation report for every candidate a job validated.\n\n" +
			"`artifacts get` returns the shared, content-addressed artifact row —\n" +
			"which is all it can return, because an artifact belongs to every job\n" +
			"that obtained the same bytes (ADR-0007). This is the per-job evidence:\n" +
			"the payload gate, the structural parse, text extraction, and the\n" +
			"identity decision, each with the reasons and capability evidence behind\n" +
			"it, for the candidates that were kept AND the ones that were rejected.\n\n" +
			"Each report is a versioned document (validation-report/1). A job\n" +
			"validated before this evidence was recorded lists no reports; that\n" +
			"is an absence, not an empty verdict.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.ValidationResult
			if err := opt.call(cmd.Context(), "artifacts.validation",
				map[string]string{"job_id": args[0]}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("artifacts.validation")
				}
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			if len(result.Reports) == 0 {
				_, err := fmt.Fprintf(opt.out,
					"no validation evidence recorded for %s — the job predates recorded reports, or nothing has been validated yet\n",
					result.JobID)
				return err
			}
			for _, report := range result.Reports {
				accepted := ""
				if report.Accepted {
					accepted = "\taccepted"
				}
				if _, err := fmt.Fprintf(opt.out, "candidate %d\t%s\t%s%s\n%s\n",
					report.CandidateID, report.Outcome, report.RecordedAt, accepted, report.Document); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.AddCommand(locate)
	command.AddCommand(get)
	command.AddCommand(validation)
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
