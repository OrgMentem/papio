// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/watch"
)

func newWatchCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "watch", Short: "Manage scheduled discovery watchlists"}
	command.AddCommand(
		newWatchAddCommand(opt),
		newWatchListCommand(opt),
		newWatchDigestCommand(opt),
		newWatchRemoveCommand(opt),
		newWatchRunCommand(opt),
	)
	return command
}

func newWatchAddCommand(opt *options) *cobra.Command {
	var label, collection, cadence, kind, mode string
	var cites, citedBy, relatedTo string
	var perRunCap, yearFrom, yearTo int
	var oaOnly bool
	command := &cobra.Command{
		Use:   "add [query]",
		Short: "Add a scheduled discovery watch",
		Long: "Add a scheduled discovery watch. Backfill watches take no query. " +
			"Alert-mode discovery watches report new works without acquiring them.",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := cobra.MaximumNArgs(1)(cmd, args); err != nil {
				return err
			}
			switch kind {
			case watch.KindBackfill:
				if len(args) != 0 {
					return fmt.Errorf("backfill watches take no query")
				}
			case watch.KindDiscovery:
				if (len(args) == 0 || strings.TrimSpace(args[0]) == "") &&
					strings.TrimSpace(cites) == "" &&
					strings.TrimSpace(citedBy) == "" && strings.TrimSpace(relatedTo) == "" {
					return fmt.Errorf("query is required unless a citation snowball DOI is supplied")
				}
			default:
				return fmt.Errorf("unknown watch kind %q", kind)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cadenceHours, err := parseWatchCadence(cadence)
			if err != nil {
				return err
			}
			query := ""
			if len(args) == 1 {
				query = strings.TrimSpace(args[0])
			}
			input := watch.CreateInput{
				Label: label, Kind: kind, Mode: mode, Query: query, Collection: collection,
				Filters: watch.Filters{
					YearFrom: yearFrom, YearTo: yearTo, OAOnly: oaOnly,
					Cites: cites, CitedBy: citedBy, RelatedTo: relatedTo,
				},
				CadenceHours: cadenceHours, PerRunCap: perRunCap,
			}
			var created watch.Watch
			if err := opt.call(cmd.Context(), "watch.add", input, &created); err != nil {
				return err
			}
			return opt.printResult(created, "Added watch %d: %s", created.ID, created.Label)
		},
	}
	flags := command.Flags()
	flags.StringVar(&label, "label", "", "human label (defaults to query)")
	flags.StringVar(&collection, "collection", "", "zotio collection for queued papers")
	flags.StringVar(&kind, "kind", watch.KindDiscovery, "watch kind: discovery or backfill")
	flags.StringVar(&mode, "mode", watch.ModeAcquire, "discovery mode: acquire or alert")
	flags.StringVar(&cadence, "cadence", "daily", "daily, weekly, or Nh")
	flags.IntVar(&perRunCap, "limit-per-run", watch.DefaultPerRunCap, "maximum new papers queued per run (1-50)")
	flags.IntVar(&yearFrom, "year-from", 0, "minimum publication year")
	flags.IntVar(&yearTo, "year-to", 0, "maximum publication year")
	flags.BoolVar(&oaOnly, "oa-only", false, "return only open-access works")
	flags.StringVar(&cites, "cites", "", "DOI to find papers citing it (forward citations; OpenAlex cites: filter)")
	flags.StringVar(&citedBy, "cited-by", "", "DOI to find papers it cites (backward references; OpenAlex cited_by: filter)")
	flags.StringVar(&relatedTo, "related-to", "", "DOI to find OpenAlex-related papers (related_to: filter)")
	return command
}

func newWatchListCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List scheduled discovery watches", Args: cobra.NoArgs, Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			var watches []watch.Watch
			if err := opt.call(cmd.Context(), "watch.list", struct{}{}, &watches); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(watches)
			}
			for _, item := range watches {
				state := "enabled"
				if !item.Enabled {
					state = "disabled"
				}
				filters := watchFilterSummary(item.Filters)
				if filters != "" {
					state += " | " + filters
				}
				if _, err := fmt.Fprintf(opt.out, "%d | %s | every %dh | %s\n", item.ID, item.Label, item.CadenceHours, state); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newWatchDigestCommand(opt *options) *cobra.Command {
	var limit int
	command := &cobra.Command{
		Use:         "digest <id>",
		Short:       "Show recently reported works from an alert watch",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseWatchID(args[0])
			if err != nil {
				return err
			}
			var result struct {
				WatchID int64               `json:"watch_id"`
				Entries []watch.DigestEntry `json:"entries"`
			}
			if err := opt.call(cmd.Context(), "watch.digest", map[string]any{"id": id, "limit": limit}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				return opt.printJSON(result.Entries)
			}
			for _, entry := range result.Entries {
				if _, err := fmt.Fprintf(
					opt.out, "%d | %s | %s | %s | %s\n",
					entry.Year, entry.Authors, entry.Title, entry.DOI, oaMarker(entry.IsOA),
				); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().IntVar(&limit, "limit", 100, "maximum digest entries to show (1-500)")
	command.AddCommand(newWatchDigestClearCommand(opt))
	return command
}

func newWatchDigestClearCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:   "clear <id>",
		Short: "Clear pending works from an alert watch digest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseWatchID(args[0])
			if err != nil {
				return err
			}
			var result struct {
				Cleared int `json:"cleared"`
			}
			if err := opt.call(cmd.Context(), "watch.digest_clear", map[string]any{"id": id}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "Cleared %d digest work(s)", result.Cleared)
		},
	}
}

func newWatchRemoveCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use: "remove <id>", Short: "Remove a scheduled discovery watch", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseWatchID(args[0])
			if err != nil {
				return err
			}
			var result struct {
				ID      int64 `json:"id"`
				Removed bool  `json:"removed"`
			}
			if err := opt.call(cmd.Context(), "watch.remove", watch.IDInput{ID: id}, &result); err != nil {
				return err
			}
			return opt.printResult(result, "Removed watch %d", result.ID)
		},
	}
}

func newWatchRunCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use: "run <id>", Short: "Force-run a scheduled discovery watch now", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseWatchID(args[0])
			if err != nil {
				return err
			}
			var result watch.RunResult
			if err := opt.call(cmd.Context(), "watch.run", watch.IDInput{ID: id}, &result); err != nil {
				return err
			}
			if result.Reported > 0 {
				return opt.printResult(result, "Watch %d reported %d new work(s) — papio watch digest %d", result.WatchID, result.Reported, result.WatchID)
			}
			return opt.printResult(result, "Watch %d queued %d paper(s)", result.WatchID, result.Queued)
		},
	}
}

func watchFilterSummary(filters watch.Filters) string {
	parts := make([]string, 0, 6)
	if filters.YearFrom != 0 || filters.YearTo != 0 {
		parts = append(parts, fmt.Sprintf("years %d-%d", filters.YearFrom, filters.YearTo))
	}
	if filters.OAOnly {
		parts = append(parts, "open access")
	}
	if filters.Cites != "" {
		parts = append(parts, "cites "+filters.Cites)
	}
	if filters.CitedBy != "" {
		parts = append(parts, "cited by "+filters.CitedBy)
	}
	if filters.RelatedTo != "" {
		parts = append(parts, "related to "+filters.RelatedTo)
	}
	return strings.Join(parts, ", ")
}

func parseWatchCadence(value string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "daily":
		return 24, nil
	case "weekly":
		return 7 * 24, nil
	}
	value = strings.TrimSpace(value)
	if !strings.HasSuffix(strings.ToLower(value), "h") {
		return 0, fmt.Errorf("invalid cadence %q (use daily, weekly, or Nh)", value)
	}
	hours, err := strconv.Atoi(strings.TrimSpace(value[:len(value)-1]))
	if err != nil || hours <= 0 {
		return 0, fmt.Errorf("invalid cadence %q (use daily, weekly, or Nh)", value)
	}
	return hours, nil
}

func parseWatchID(value string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("watch id must be a positive integer")
	}
	return id, nil
}
