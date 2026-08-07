// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/api"
)

// newDeliveryCommand is ADR-0017 Decision 1's new CLI surface for document
// delivery / ILL: get, submit, and cancel where the provider supports it,
// plus Decision 4's three reconciliation operations for a job stuck in
// unknown_outcome (history, confirm-exists, confirm-absent). Per papio's
// existing pattern, the command-derived MCP facade exposes the same
// operations without separate MCP-side work.
func newDeliveryCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "delivery", Short: "Manage document-delivery and ILL requests"}
	command.AddCommand(
		newDeliveryGetCommand(opt),
		newDeliverySubmitCommand(opt),
		newDeliveryCancelCommand(opt),
		newDeliveryHistoryCommand(opt),
		newDeliveryConfirmExistsCommand(opt),
		newDeliveryConfirmAbsentCommand(opt),
	)
	return command
}

func newDeliveryGetCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:         "get <job-id>",
		Short:       "Show a job's document-delivery request and compiled gate",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var detail api.DeliveryDetail
			if err := opt.call(cmd.Context(), "delivery.get", map[string]string{"job_id": args[0]}, &detail); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(detail)
			}
			return printDeliveryDetail(opt, args[0], detail)
		},
	}
}

// printDeliveryDetail is the shared human-readable renderer for `delivery
// get` and `delivery history`: both surface the same durable row, compiled
// gate summary, and last recorded evaluation (ADR-0017 Decision 3C).
func printDeliveryDetail(opt *options, jobID string, detail api.DeliveryDetail) error {
	req := detail.Request
	if req == nil {
		_, err := fmt.Fprintf(opt.out, "%s: no delivery request\n", jobID)
		return err
	}
	if _, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\t%s\n", jobID, req.Provider, req.RequestClass, req.State); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(opt.out, "  gate: %s\n", detail.Gate.Class); err != nil {
		return err
	}
	for _, blocker := range detail.Gate.Blockers {
		if _, err := fmt.Fprintf(opt.out, "    blocked: %s — %s\n", blocker.Code, blocker.Evidence); err != nil {
			return err
		}
	}
	if detail.LastEvaluation != nil {
		if _, err := fmt.Fprintf(opt.out, "  last evaluated: %s\n", detail.LastEvaluation.Decision); err != nil {
			return err
		}
	}
	if req.ProviderReference != "" {
		if _, err := fmt.Fprintf(opt.out, "  provider reference: %s\n", req.ProviderReference); err != nil {
			return err
		}
	}
	if req.NextCheckAt != "" {
		if _, err := fmt.Fprintf(opt.out, "  next check: %s\n", req.NextCheckAt); err != nil {
			return err
		}
	}
	return nil
}

func newDeliverySubmitCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:   "submit <job-id>",
		Short: "Run the document-delivery Branch/gate decision for a job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.DeliverySubmitResult
			if err := opt.call(cmd.Context(), "delivery.submit", map[string]string{"job_id": args[0]}, &result); err != nil {
				return err
			}
			if !result.Configured {
				return opt.printResult(result, "%s: no document-delivery route configured for this job's institution profile", args[0])
			}
			if len(result.Blockers) > 0 {
				return opt.printResult(result, "%s: %s (%s)", args[0], result.Action, strings.Join(result.Blockers, ", "))
			}
			return opt.printResult(result, "%s: %s", args[0], result.Action)
		},
	}
}

func newDeliveryCancelCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <job-id>",
		Short: "Cancel a document-delivery request, where the provider supports it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.DeliveryCancelResult
			if err := opt.call(cmd.Context(), "delivery.cancel", map[string]string{"job_id": args[0]}, &result); err != nil {
				return err
			}
			if result.Cancelled {
				return opt.printResult(result, "Cancelled delivery request for %s", args[0])
			}
			return opt.printResult(result, "%s: not cancelled (%s)", args[0], result.Reason)
		},
	}
}

func newDeliveryHistoryCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:         "history <job-id>",
		Short:       "Show a delivery request's history for reconciliation (Decision 4's open_request_history)",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.DeliveryActionResult
			params := map[string]string{"job_id": args[0], "operation": "open_request_history"}
			if err := opt.call(cmd.Context(), "delivery.action", params, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			if result.Detail == nil {
				_, err := fmt.Fprintf(opt.out, "%s: no delivery request\n", args[0])
				return err
			}
			return printDeliveryDetail(opt, args[0], *result.Detail)
		},
	}
}

func newDeliveryConfirmExistsCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:   "confirm-exists <job-id> <provider-reference>",
		Short: "Confirm a lodged request exists at the provider and resume polling",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.DeliveryActionResult
			params := map[string]string{"job_id": args[0], "operation": "confirm_request_exists", "provider_reference": args[1]}
			if err := opt.call(cmd.Context(), "delivery.action", params, &result); err != nil {
				return err
			}
			return opt.printResult(result, "%s: confirmed pending with reference %s; job is now %s", args[0], args[1], result.JobState)
		},
	}
}

func newDeliveryConfirmAbsentCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:   "confirm-absent <job-id>",
		Short: "Confirm no request exists at the provider, cancel the stale row, and reopen reconciliation for a deliberate decision (v1 never auto-resubmits)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result api.DeliveryActionResult
			params := map[string]string{"job_id": args[0], "operation": "confirm_request_absent"}
			if err := opt.call(cmd.Context(), "delivery.action", params, &result); err != nil {
				return err
			}
			return opt.printResult(result, "%s: closed the stale request; job is now %s", args[0], result.JobState)
		},
	}
}
