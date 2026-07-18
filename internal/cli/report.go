// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/batch"
	"papio/internal/protocol"
)

func newBatchCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "batch", Short: "Inspect persisted acquisition batches"}
	var markdown bool
	report := &cobra.Command{
		Use:   "report <batch-id|latest>",
		Short: "Join a batch manifest with live acquisition outcomes",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if markdown && opt.jsonOutput {
				return fmt.Errorf("--markdown cannot be combined with --json")
			}
			var result batch.Report
			if err := opt.call(cmd.Context(), "acquire.report", api.AcquireReportParams{BatchID: args[0]}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			if markdown {
				_, err := fmt.Fprint(opt.out, batch.Markdown(&result))
				return err
			}
			return printBatchReport(opt, &result)
		},
	}
	report.Flags().BoolVar(&markdown, "markdown", false, "emit an agent-ready Markdown digest")
	command.AddCommand(report)
	return command
}

func printBatchReport(opt *options, report *batch.Report) error {
	if _, err := fmt.Fprintf(opt.out, "Batch %s (%d works)\n", report.BatchID, report.Summary.Total); err != nil {
		return err
	}
	if report.Label != "" {
		if _, err := fmt.Fprintf(opt.out, "Label: %s\n", report.Label); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(opt.out, "OUTCOME\tJOB\tWORK\tDETAIL"); err != nil {
		return err
	}
	for _, item := range report.Works {
		detail := item.Reason
		if item.FailureClass != "" {
			detail = item.FailureClass
		}
		if item.ErrorClass != "" {
			detail = item.ErrorClass
			if item.ErrorHint != "" {
				detail += ": " + item.ErrorHint
			}
		}
		if item.ParentKey != "" || item.AttachmentKey != "" {
			keys := strings.Trim(strings.Join([]string{item.ParentKey, item.AttachmentKey}, "/"), "/")
			if detail != "" {
				detail += " "
			}
			detail += keys
		}
		if item.Collection != "" {
			if detail != "" {
				detail += " "
			}
			detail += "collection=" + item.Collection
		}
		if item.FilingStatus == "file_failed" {
			if detail != "" {
				detail += " "
			}
			detail += "collection_filing_failed"
			if item.FilingError != "" {
				detail += "=" + item.FilingError
			}
		}
		if _, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\t%s\n", item.Outcome, item.JobID, reportWorkDescription(item.Work), detail); err != nil {
			return err
		}
	}
	return nil
}

func reportWorkDescription(request protocol.WorkRequest) string {
	if request.Title != "" {
		return request.Title
	}
	if request.Identifiers == nil {
		return request.RequestID
	}
	for _, value := range []string{
		request.Identifiers.DOI,
		request.Identifiers.ArXiv,
		request.Identifiers.PMID,
		request.Identifiers.ISBN,
		request.Identifiers.OpenAlex,
	} {
		if value != "" {
			return value
		}
	}
	return request.RequestID
}
