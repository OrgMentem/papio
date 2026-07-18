// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"papio/internal/zotio"
)

func newZotioCommand(opt *options) *cobra.Command {
	command := &cobra.Command{
		Use:   "zotio",
		Short: "Preview and apply Zotero integration through zotio",
	}
	preflight := &cobra.Command{
		Use:   "preflight",
		Short: "Verify the configured zotio version and capabilities",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result zotio.PreflightResult
			if err := opt.call(cmd.Context(), "zotio.preflight", struct{}{}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "Zotio %s ready", result.Version)
		},
	}
	plan := &cobra.Command{
		Use:   "plan <job-id> [job-id...]",
		Short: "Export ready jobs and preview exact zotio mutations",
		Args:  cobra.RangeArgs(1, 50),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result struct {
				Plans []*zotio.Plan `json:"plans"`
			}
			if err := opt.call(cmd.Context(), "zotio.plan", map[string]any{"job_ids": args}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			for _, prepared := range result.Plans {
				if _, err := fmt.Fprintf(opt.out, "%s  %s  %s  %s\n", prepared.ID, prepared.ConfirmationSHA256, prepared.Route, prepared.JobID); err != nil {
					return err
				}
			}
			return nil
		},
	}
	var confirmation string
	apply := &cobra.Command{
		Use:   "apply <plan-id>",
		Short: "Apply one immutable zotio plan after SHA-256 confirmation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result zotio.ApplyResult
			params := map[string]any{"plan_id": args[0], "confirmation_sha256": confirmation}
			if err := opt.call(cmd.Context(), "zotio.apply", params, &result); err != nil {
				return err
			}
			return opt.printResult(result, "Zotio %s: parent=%s attachment=%s", result.Status, result.ParentKey, result.AttachmentKey)
		},
	}
	apply.Flags().StringVar(&confirmation, "confirm-sha256", "", "Exact confirmation SHA-256 printed by `papio zotio plan`")
	_ = apply.MarkFlagRequired("confirm-sha256")

	command.AddCommand(plan, apply)
	command.AddCommand(preflight)
	return command
}
