// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/triage"
)

// newStatsCommand surfaces the acquisition value read model the daemon already
// computes for the browser extension's stats view. It is a passthrough, not a
// second aggregation: papio status reports the live job board and jobs
// failures reports failure groups, while this reports what the pipeline has
// obtained over its lifetime and under which access basis.
func newStatsCommand(opt *options) *cobra.Command {
	command := &cobra.Command{
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
	command.AddCommand(newStatsPageBulkCommand(opt))
	return command
}

// newStatsPageBulkCommand surfaces store.PageBulkStats: page-bulk scan/submit
// funnel counts and the identifier_yield honest denominator
// (dev/post-build-followups.md item 3). One row per source-origin class —
// today always the single "(unknown origin)" bucket, since
// page_bulk_status_request carries no page-origin field; see
// store.PageBulkStatsRow's OriginClass doc.
func newStatsPageBulkCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:         "page-bulk",
		Short:       "Show page-bulk scan/submit funnel and identifier yield",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var page api.PageBulkStatsPage
			if err := opt.call(cmd.Context(), "stats.page_bulk", struct{}{}, &page); err != nil {
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "origins", page.Origins, false)
			}
			for _, row := range page.Origins {
				origin := row.OriginClass
				if origin == "" {
					origin = "(unknown origin)"
				}
				leverage := "n/a"
				if row.BulkLeverage != nil {
					leverage = fmt.Sprintf("%.1f", *row.BulkLeverage)
				}
				yield := "no denominator"
				if row.IdentifierYield != nil {
					yield = fmt.Sprintf("%.0f%%", *row.IdentifierYield*100)
				}
				if _, err := fmt.Fprintf(opt.out,
					"%s\tscan sessions: %d\tuseful-scan rate: %.0f%%\tbulk leverage: %s\tsubmit conversion: %.0f%%\tidentifier yield: %s\n",
					origin, row.TotalScanSessions, row.UsefulScanRate*100, leverage, row.SubmitConversion*100, yield); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
