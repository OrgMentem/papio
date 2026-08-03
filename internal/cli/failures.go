// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/store"
)

func newFailuresCommand(opt *options) *cobra.Command {
	var limit int
	var byProvider bool
	command := &cobra.Command{
		Use:         "failures",
		Short:       "Aggregate unavailable and parked acquisition reasons",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			effective := store.EffectiveFailureSummaryLimit(limit)
			var page api.FailuresPage
			if err := opt.call(cmd.Context(), "failures.list_v1", map[string]any{
				"limit":       effective,
				"by_provider": byProvider,
			}, &page); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("failures.list_v1")
				}
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "failures", page.Failures, page.Truncated)
			}
			if _, err := fmt.Fprintln(opt.out, "provider/host | reason | count | example job id"); err != nil {
				return err
			}
			for _, failure := range page.Failures {
				if _, err := fmt.Fprintf(opt.out, "%s | %s | %d | %s\n",
					failure.Provider, failure.Reason, failure.Count, failure.ExampleJobID); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().IntVar(&limit, "limit", store.FailureSummaryLimitDefault,
		"maximum aggregate rows (1-200)")
	command.Flags().BoolVar(&byProvider, "by-provider", false,
		"group by provider host/source instead of reason")
	return command
}
