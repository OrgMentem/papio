// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/triage"
)

func newInboxCommand(opt *options) *cobra.Command {
	var limit int
	command := &cobra.Command{
		Use:         "inbox",
		Short:       "Show the triage inbox",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var snapshot triage.Snapshot
			if err := opt.call(cmd.Context(), "triage.snapshot", triage.SnapshotRequest{Limit: limit}, &snapshot); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(snapshot)
			}
			for _, item := range snapshot.Items {
				if err := printInboxItem(opt, item); err != nil {
					return err
				}
			}
			if snapshot.HasMore {
				_, err := fmt.Fprintf(opt.out, "… more items available; use --json for the cursor\n")
				return err
			}
			return nil
		},
	}
	command.Flags().IntVar(&limit, "limit", 0, "maximum items (default 50, maximum 100)")

	counts := &cobra.Command{
		Use:         "counts",
		Short:       "Show complete triage inbox counts",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var result triage.Counts
			if err := opt.call(cmd.Context(), "triage.counts", struct{}{}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result)
			}
			_, err := fmt.Fprintf(opt.out, "pending: %d (watch hits: %d, actions: %d, retractions: %d)\n", result.PendingTotal, result.WatchHits, result.Actions, result.Retractions)
			return err
		},
	}
	command.AddCommand(counts)
	return command
}

func printInboxItem(opt *options, item triage.Item) error {
	switch item.Kind {
	case triage.KindWatchHit:
		labels := make([]string, 0, len(item.WatchHit.Watches))
		for _, watched := range item.WatchHit.Watches {
			labels = append(labels, watched.Label)
		}
		_, err := fmt.Fprintf(opt.out, "%d\twatch hit\t%s\t[%s]\n", item.Rank, item.Title, strings.Join(labels, ", "))
		return err
	case triage.KindHumanAction:
		_, err := fmt.Fprintf(opt.out, "%d\taction\t%s\t%s\n", item.Rank, item.HumanAction.ActionKind, item.HumanAction.JobID)
		return err
	case triage.KindRetraction:
		_, err := fmt.Fprintf(opt.out, "%d\t%s\t%s\n", item.Rank, item.Retraction.Nature, item.Retraction.DOI)
		return err
	default:
		return fmt.Errorf("unsupported triage item kind %q", item.Kind)
	}
}
