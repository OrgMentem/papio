// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"

	"papio/internal/api"
	"papio/internal/app"
	"papio/internal/job"
	"papio/internal/store"

	"github.com/spf13/cobra"
)

func newManagedFilingCommand(opt *options) *cobra.Command {
	parent := &cobra.Command{Use: "filing", Short: "Inspect and retry durable managed folder filing"}
	for _, operation := range []string{"list", "inspect"} {
		operation := operation
		var limit int
		command := &cobra.Command{Use: operation, Short: "Inspect managed destination receipts and retry state", Annotations: map[string]string{"mcp:read-only": "true"}, Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				params := map[string]any{"limit": effectiveLimit(limit, job.ListLimitMax, job.ListLimitDefault)}
				if operation == "inspect" {
					params["job_id"] = args[0]
				}
				var page api.ManagedFilingsPage
				if err := opt.call(cmd.Context(), "managed_filing."+operation, params, &page); err != nil {
					if isUnknownMethod(err) {
						return daemonUpgradeRequired("managed_filing." + operation)
					}
					return err
				}
				if opt.jsonOutput {
					return printPage(opt, "filings", page.Filings, page.Truncated)
				}
				for _, row := range page.Filings {
					if _, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\tattempts=%d\tretry=%s\terror=%s\t%s\n", row.JobID, row.State, row.Disposition, row.Attempts, row.RetryAt, row.ErrorCode, store.StripTerminalControls(row.ReceiptPath)); err != nil {
						return err
					}
				}
				return truncationNotice(opt, page.Truncated, len(page.Filings), "filings")
			},
		}
		if operation == "inspect" {
			command.Use = "inspect <job-id>"
			command.Args = cobra.ExactArgs(1)
		}
		command.Flags().IntVar(&limit, "limit", job.ListLimitDefault, "maximum receipts (1-500)")
		parent.AddCommand(command)
	}
	var destination string
	retry := &cobra.Command{Use: "retry <job-id>", Short: "Safely replay a journaled managed folder destination", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var result app.ManagedFiling
			if err := opt.call(cmd.Context(), "managed_filing.retry", map[string]any{"job_id": args[0], "destination": destination}, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("managed_filing.retry")
				}
				return err
			}
			if err := opt.printResult(result, "%s: %s (attempts %d, receipt %s)", result.JobID, result.State, result.Attempts, store.StripTerminalControls(result.ReceiptPath)); err != nil {
				return err
			}
			if result.State != "filed" {
				return errors.New("managed filing failed: " + result.ErrorCode)
			}
			return nil
		},
	}
	retry.Flags().StringVar(&destination, "destination", "", "journaled folder destination; default is the configured folder")
	parent.AddCommand(retry)
	return parent
}
