// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/agentjson"
	"papio/internal/api"
	"papio/internal/ipc"
	"papio/internal/store"
	"papio/internal/watch"
)

func newWatchCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "watch", Short: "Manage scheduled discovery watchlists"}
	command.AddCommand(
		newWatchAddCommand(opt),
		newWatchListCommand(opt),
		newWatchDigestCommand(opt),
		newWatchRemoveCommand(opt),
		newWatchResumeCommand(opt),
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
			"Alert-mode discovery watches report new works without acquiring them.\n\n" +
			"By default (--mode acquire, --cadence daily) a discovery watch runs on its\n" +
			"own schedule and submits up to --limit-per-run new works each run, and\n" +
			"every job it submits asks for automatic Zotero import once it is ready,\n" +
			"whatever zotio.auto_import says (zotio.auto_import_paused still pauses\n" +
			"it). Use --mode alert to only report new works to the watch digest and\n" +
			"acquire them on demand.",
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
				return printPage(opt, "watches", watches, false)
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
				// last_error carries the last run's failure, or why its
				// discovery scan was incomplete (failed source, page limit
				// reached). Backend failure text is sanitized upstream but
				// still third-party, so it goes through the same terminal
				// control stripping as digest rows.
				if item.LastError != "" {
					state += " | last run: " + store.StripTerminalControls(item.LastError)
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
			effective := effectiveLimit(limit, watch.DigestLimitMax, watch.DigestLimitDefault)
			var result struct {
				WatchID int64               `json:"watch_id"`
				Entries []watch.DigestEntry `json:"entries"`
			}
			if err := opt.call(cmd.Context(), "watch.digest", map[string]any{"id": id, "limit": effective}, &result); err != nil {
				return err
			}
			if opt.jsonOutput {
				rows, truncated := agentjson.Capped(result.Entries, effective)
				return printPage(opt, "entries", rows, truncated)
			}
			for _, entry := range result.Entries {
				// entry.Title/Authors/DOI are third-party bibliographic metadata:
				// watch.Store normalizes them with only strings.TrimSpace before
				// persisting a digest row (internal/watch/store.go), so whoever
				// registers the record in Crossref/OpenAlex/RSS controls these
				// strings verbatim. Route them through the same
				// StripTerminalControls choke point ActivityText uses, or a
				// watch match on an attacker-registered title injects ANSI/OSC
				// escapes into `papio watch digest`.
				if _, err := fmt.Fprintf(
					opt.out, "%d | %s | %s | %s | %s\n",
					entry.Year,
					store.StripTerminalControls(entry.Authors),
					store.StripTerminalControls(entry.Title),
					store.StripTerminalControls(entry.DOI),
					oaMarker(entry.IsOA),
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
	var all, dryRun bool
	command := &cobra.Command{
		Use:   "clear <id>",
		Short: "Clear pending works from an alert watch digest",
		Long: "Discard every pending work in an alert watch digest, including works\n" +
			"`papio watch digest` did not show (it lists 100 by default). A cleared work\n" +
			"is never reported again. Clearing requires --all; --dry-run counts the\n" +
			"pending works without clearing them. To discard single works instead,\n" +
			"use `papio inbox decide <item-id> --op dismiss`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseWatchID(args[0])
			if err != nil {
				return err
			}
			if dryRun || !all {
				pending, atLeast, err := watchPendingDigest(cmd, opt, id)
				if err != nil {
					return err
				}
				if !dryRun {
					return fmt.Errorf("watch %d has %s pending digest work(s), including any `papio watch digest` did not show; nothing was cleared. Rerun with --all to clear every one of them", id, watchPendingCount(pending, atLeast))
				}
				result := watchDigestClearPreview{WatchID: id, DryRun: true, WouldClear: pending, Truncated: atLeast}
				return opt.printResult(result, "Would clear %s pending digest work(s) from watch %d (dry run: nothing cleared)", watchPendingCount(pending, atLeast), id)
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
	command.Flags().BoolVar(&all, "all", false, "confirm clearing every pending work, including works the digest view did not show")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "count the pending works without clearing them")
	return command
}

// watchDigestClearPreview is the `watch digest clear --dry-run` receipt.
// Truncated means watch.digest returned a full page, so at least WouldClear
// works are pending.
type watchDigestClearPreview struct {
	WatchID    int64 `json:"watch_id"`
	DryRun     bool  `json:"dry_run"`
	WouldClear int   `json:"would_clear"`
	Truncated  bool  `json:"truncated"`
}

// watchPendingDigest counts a watch's pending digest works. watch.digest has
// no total, so a full page means "at least this many".
func watchPendingDigest(cmd *cobra.Command, opt *options, id int64) (count int, atLeast bool, err error) {
	var digest api.WatchDigestResult
	if err := opt.call(cmd.Context(), "watch.digest", map[string]any{"id": id, "limit": watch.DigestLimitMax}, &digest); err != nil {
		return 0, false, err
	}
	return len(digest.Entries), len(digest.Entries) >= watch.DigestLimitMax, nil
}

func watchPendingCount(count int, atLeast bool) string {
	if atLeast {
		return fmt.Sprintf("%d or more", count)
	}
	return strconv.Itoa(count)
}

func newWatchRemoveCommand(opt *options) *cobra.Command {
	var discardDigest bool
	command := &cobra.Command{
		Use: "remove <id>", Short: "Remove a scheduled discovery watch", Args: cobra.ExactArgs(1),
		Long: "Remove a scheduled discovery watch. Jobs and Zotero items earlier runs\n" +
			"created stay. The watch's digest is deleted with it, so removing a watch\n" +
			"that still has pending (unreviewed) digest works is refused unless\n" +
			"--discard-digest confirms that those works are discarded too.",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseWatchID(args[0])
			if err != nil {
				return err
			}
			if !discardDigest {
				pending, atLeast, err := watchPendingDigest(cmd, opt, id)
				if err != nil {
					return err
				}
				if pending > 0 {
					return fmt.Errorf("watch %d has %s pending digest work(s) that removing it would delete; nothing was removed. Review them with `papio watch digest %d` or `papio export watch %d`, then rerun with --discard-digest", id, watchPendingCount(pending, atLeast), id, id)
				}
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
	command.Flags().BoolVar(&discardDigest, "discard-digest", false, "remove the watch even though pending digest works are deleted with it")
	return command
}

func newWatchResumeCommand(opt *options) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <id>",
		Short: "Re-enable a watch that repeated failures disabled",
		Long: "Re-enable a watch that five consecutive failures disabled, keeping its id, query, and digest history.\n\n" +
			"Fix the failing source first, then force a run with `papio watch run <id>`. Resume refuses until a run has succeeded since the watch was disabled.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseWatchID(args[0])
			if err != nil {
				return err
			}
			var result watch.Watch
			if err := opt.call(cmd.Context(), "watch.resume", watch.IDInput{ID: id}, &result); err != nil {
				return watchResumeError(id, err)
			}
			return opt.printResult(result, "Resumed watch %d (%s); it runs again every %dh", result.ID, result.Label, result.CadenceHours)
		},
	}
}

// watchResumeError turns a watch.resume refusal into the next step the user
// must take. Other errors pass through unchanged.
func watchResumeError(id int64, err error) error {
	var remote *ipc.RemoteError
	if !errors.As(err, &remote) {
		return err
	}
	class := ""
	if remote.Detail != nil {
		class = remote.Detail.ErrorClass
	}
	switch {
	case remote.Code == "not_found":
		return fmt.Errorf("watch %d not found (papio watch list shows existing watches): %w", id, err)
	case class == api.WatchResumeNotDisabledClass:
		return fmt.Errorf("watch %d is already enabled; there is nothing to resume: %w", id, err)
	case class == api.WatchResumeRecoveryNeededClass:
		return fmt.Errorf("watch %d has not run successfully since it was disabled; fix the failing source, run `papio watch run %d` until it succeeds, then resume it: %w", id, id, err)
	default:
		return err
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
			incomplete := ""
			if result.Degraded {
				incomplete = "; discovery scan incomplete — see papio watch list"
			}
			if result.Reported > 0 {
				return opt.printResult(result, "Watch %d reported %d new work(s) — papio watch digest %d%s", result.WatchID, result.Reported, result.WatchID, incomplete)
			}
			return opt.printResult(result, "Watch %d queued %d paper(s)%s", result.WatchID, result.Queued, incomplete)
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
