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
	command.AddCommand(newWatchAddCommand(opt), newWatchListCommand(opt), newWatchRemoveCommand(opt), newWatchRunCommand(opt))
	return command
}

func newWatchAddCommand(opt *options) *cobra.Command {
	var label, collection, cadence string
	var perRunCap, yearFrom, yearTo int
	var oaOnly bool
	command := &cobra.Command{
		Use:   "add <query>",
		Short: "Add a scheduled discovery watch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cadenceHours, err := parseWatchCadence(cadence)
			if err != nil {
				return err
			}
			input := watch.CreateInput{
				Label: label, Query: strings.TrimSpace(args[0]), Collection: collection,
				Filters:      watch.Filters{YearFrom: yearFrom, YearTo: yearTo, OAOnly: oaOnly},
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
	flags.StringVar(&collection, "collection", "", "Zotio collection for queued papers")
	flags.StringVar(&cadence, "cadence", "daily", "daily, weekly, or Nh")
	flags.IntVar(&perRunCap, "limit-per-run", watch.DefaultPerRunCap, "maximum new papers queued per run (1-50)")
	flags.IntVar(&yearFrom, "year-from", 0, "minimum publication year")
	flags.IntVar(&yearTo, "year-to", 0, "maximum publication year")
	flags.BoolVar(&oaOnly, "oa-only", false, "return only open-access works")
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
				if _, err := fmt.Fprintf(opt.out, "%d | %s | every %dh | %s\n", item.ID, item.Label, item.CadenceHours, state); err != nil {
					return err
				}
			}
			return nil
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
			return opt.printResult(result, "Watch %d queued %d paper(s)", result.WatchID, result.Queued)
		},
	}
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
