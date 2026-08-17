// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"papio/internal/zotio"
)

func newZotioImportBackfillCommand(opt *options) *cobra.Command {
	var apply bool
	var includeNotRequested bool
	var limit int
	var cursor string
	command := &cobra.Command{
		Use:   "import-backfill",
		Short: "Backfill stranded ready papers into Zotero (dry-run by default)",
		Long: `Deliver validated ready jobs whose Zotero import never succeeded.

Dry-run is the default: the command reports what it would import, which
papers are already owned (and would be marked duplicate), and which are
expected to fail (for example bundle validation on an empty title). Pass
--apply to write to your library.

Jobs submitted without policy.auto_import are excluded unless you pass
--include-not-requested, because importing them was never requested.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			effective := zotio.ImportBackfillLimitDefault
			if limit > 0 {
				effective = limit
			}
			if effective > 50 {
				effective = 50
			}
			var result zotio.ImportBackfillResult
			params := map[string]any{
				"apply":                 apply,
				"include_not_requested": includeNotRequested,
				"limit":                 effective,
			}
			if cursor != "" {
				params["cursor"] = cursor
			}
			if err := opt.call(cmd.Context(), "zotio.import_backfill", params, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			mode := "dry-run"
			if !result.DryRun {
				mode = "apply"
			}
			if _, err := fmt.Fprintf(opt.out,
				"import-backfill %s: selected=%d would_import=%d already_owned=%d expected_fail=%d applied=%d failed=%d truncated=%t\n",
				mode,
				result.Summary.Selected,
				result.Summary.WouldImport,
				result.Summary.AlreadyOwned,
				result.Summary.ExpectedFail,
				result.Summary.Applied,
				result.Summary.Failed,
				result.Truncated,
			); err != nil {
				return err
			}
			if result.Summary.NotRequestedExcluded > 0 {
				if _, err := fmt.Fprintf(opt.out,
					"%d ready paper(s) omitted because auto_import was not requested; re-run with --include-not-requested to include them\n",
					result.Summary.NotRequestedExcluded,
				); err != nil {
					return err
				}
			}
			if result.Cursor != "" {
				if _, err := fmt.Fprintf(opt.out, "cursor: %s\n", result.Cursor); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&apply, "apply", false, "apply imports to the configured Zotero library (default is dry-run)")
	command.Flags().BoolVar(&includeNotRequested, "include-not-requested", false, "include ready jobs that did not request policy.auto_import")
	command.Flags().IntVar(&limit, "limit", zotio.ImportBackfillLimitDefault, "maximum jobs per invocation (1-50)")
	command.Flags().StringVar(&cursor, "cursor", "", "resume after the cursor returned by a previous invocation")
	return command
}
