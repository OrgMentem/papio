// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/job"
	"papio/internal/store"
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
	command.AddCommand(newStatsPageBulkCommand(opt), newStatsProducersCommand(opt))
	return command
}

// newStatsProducersCommand reports who produced the artifacts promoted in a
// period — adapter, agent, viewer capture, daemon fetch, manual or unknown —
// and how many needed a person, from each promotion's artifact.producer
// record. Sign-in is counted apart from other interventions, so a paced run
// that only needed an institutional login does not read as operator work.
func newStatsProducersCommand(opt *options) *cobra.Command {
	var since, until string
	command := &cobra.Command{
		Use:         "producers",
		Short:       "Show who produced each acquired artifact and how many needed a person",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, err := parseSinceInstant(since)
			if err != nil {
				return err
			}
			params := map[string]string{"since": from.UTC().Format(time.RFC3339Nano)}
			if until != "" {
				to, err := time.Parse(time.RFC3339, until)
				if err != nil {
					return fmt.Errorf("--until must be an RFC3339 instant, got %q", until)
				}
				params["until"] = to.UTC().Format(time.RFC3339Nano)
			}
			var stats job.ProducerStats
			if err := opt.call(cmd.Context(), "stats.producers_v1", params, &stats); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(stats)
			}
			if _, err := fmt.Fprintf(opt.out, "acquired %s to %s: %d (unattended: %d, sign-in only: %d, intervened: %d, no producer record: %d)\n",
				stats.Since, stats.Until, stats.Acquired, stats.Unattended, stats.SignInOnly, stats.Intervened, stats.Unrecorded); err != nil {
				return err
			}
			for _, producer := range job.Producers {
				if _, err := fmt.Fprintf(opt.out, "  %s\t%d\n", producer, stats.Producers[producer]); err != nil {
					return err
				}
			}
			for _, intervention := range job.Interventions {
				if _, err := fmt.Fprintf(opt.out, "  intervention %s\t%d\n", intervention, stats.Interventions[intervention]); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().StringVar(&since, "since", "24h", "count promotions since this RFC3339 instant or within this Go duration (e.g. 24h)")
	command.Flags().StringVar(&until, "until", "", "count promotions before this RFC3339 instant (default now)")
	return command
}

// producerEventSummary renders an artifact.producer event's record on the
// jobs show event line, so a person reading one job sees who produced its
// artifact without --json. Every other event renders nothing extra.
func producerEventSummary(event map[string]any) string {
	if event["kind"] != job.ArtifactProducerEvent {
		return ""
	}
	detail, _ := event["detail"].(map[string]any)
	text := func(key string) string {
		value, _ := detail[key].(string)
		return store.StripTerminalControls(value)
	}
	parts := []string{"producer=" + text("producer")}
	for _, key := range []string{"adapter_id", "adapter_version", "agent_decision_id", "opened_by"} {
		if value := text(key); value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	interventions, _ := detail["interventions"].([]any)
	names := make([]string, 0, len(interventions))
	for _, intervention := range interventions {
		if name, ok := intervention.(string); ok {
			names = append(names, store.StripTerminalControls(name))
		}
	}
	if len(names) == 0 {
		parts = append(parts, "unattended")
	} else {
		parts = append(parts, "interventions="+strings.Join(names, ","))
	}
	return "  " + strings.Join(parts, " ")
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
