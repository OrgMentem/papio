// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/triage"
)

// newStatsCommand surfaces the acquisition value read model the daemon already
// computes for the browser extension's stats view. It is a passthrough, not a
// second aggregation: papio status reports the live job board and jobs
// failures reports failure groups, while this reports what the pipeline has
// obtained over its lifetime and under which access basis.
func newStatsCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:         "stats",
		Short:       "Show lifetime acquisition totals by access basis",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var stats triage.Stats
			if err := opt.call(cmd.Context(), "stats.get", struct{}{}, &stats); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(stats)
			}
			if _, err := fmt.Fprintf(opt.out, "acquired: %d (open access: %d, institutional: %d, licensed api: %d, other: %d)\n",
				stats.AcquiredTotal, stats.Access.OpenAccess, stats.Access.Institutional,
				stats.Access.LicensedAPI, stats.Access.Other); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(opt.out, "failed: %d\nhandoffs required: %d\n",
				stats.FailedTotal, stats.HandoffsRequired); err != nil {
				return err
			}
			for _, bucket := range stats.Series {
				if _, err := fmt.Fprintf(opt.out, "week %s\t%d\n",
					bucket.PeriodStart.UTC().Format(time.DateOnly), bucket.Acquired); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
