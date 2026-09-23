// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"errors"
	"strconv"

	"github.com/spf13/cobra"

	"papio/internal/api"
)

func publisherRetryCommand(opt *options) *cobra.Command {
	var revision int64
	command := &cobra.Command{
		Use:   "retry-publisher <action-id>",
		Short: "Retry a failed route through the DOI or an upgraded institution adapter",
		Long: "Retry a failed browser route through the paper's DOI in your browser.\n\n" +
			"If the DOI retry has already failed, this command re-offers the original\n" +
			"institutional route only when the refusing adapter has since been upgraded.\n" +
			"Use the action id and revision from 'papio actions list --json'. The failed\n" +
			"action and its evidence remain in the job history. Each route can be retried\n" +
			"once. Pending sign-ins, challenges, terms, downloads, and unresolved browser\n" +
			"effects must be handled first. Neither retry establishes entitlement.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
				return errors.New("action-id must be a positive integer")
			}
			if revision <= 0 {
				return errors.New("--revision is required; take it from 'papio actions list --json'")
			}
			var result api.SubmitResult
			if err := opt.call(cmd.Context(), "actions.retry_publisher", map[string]any{"action_id": id, "expected_revision": revision}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "route retry queued for %s", result.JobID)
		},
	}
	command.Flags().Int64Var(&revision, "revision", 0, "revision the failed action had when you listed it")
	return command
}
