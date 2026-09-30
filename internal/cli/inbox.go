// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"papio/internal/store"
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
	var op string
	var watchScope string
	var deleteGrab bool
	decide := &cobra.Command{
		Use:   "decide <item-id>",
		Short: "Acquire or dismiss one triage inbox item",
		Long: "Acquire or dismiss one triage inbox item.\n\n" +
			"Dismissing a captured PDF grab (an item id starting with \"" + triage.PdfGrabIDPrefix + "\")\n" +
			"permanently deletes the captured file and its grab record; `papio grabs\n" +
			"suggest` and `papio grabs identify` cannot recover it afterwards. That\n" +
			"dismissal is refused unless --delete-grab confirms it.\n\n" +
			"A decision that did not apply (conflict or invalid) exits nonzero after\n" +
			"printing its outcome.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if op != "acquire" && op != "dismiss" {
				return fmt.Errorf("--op must be acquire or dismiss, got %q", op)
			}
			params := map[string]any{"item_id": args[0], "op": op}
			// watch_scope is required for a dismiss and meaningless otherwise: the
			// daemon needs to know whether one watch or every watch that surfaced
			// this work is being answered.
			if op == "dismiss" {
				// Dismissing a grab is not a notification dismissal: the daemon
				// deletes the only copy of the captured bytes. Make that an
				// explicit, scriptable confirmation rather than a side effect.
				if strings.HasPrefix(args[0], triage.PdfGrabIDPrefix) && !deleteGrab {
					return fmt.Errorf("%s is a captured PDF grab: dismissing it permanently deletes the captured file and its grab record; nothing was changed. Rerun with --delete-grab to confirm, or file it with `papio grabs suggest %s`", args[0], strings.TrimPrefix(args[0], triage.PdfGrabIDPrefix))
				}
				scope, err := parseWatchScope(watchScope)
				if err != nil {
					return err
				}
				params["watch_scope"] = scope
			}
			var result triageDecision
			if err := opt.call(cmd.Context(), "triage.decide", params, &result); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("triage.decide")
				}
				return err
			}
			if err := printTriageDecision(opt, args[0], result); err != nil {
				return err
			}
			// The outcome is already on stdout; the exit status must agree with
			// it, so a decision that did not apply is a failed command.
			switch triage.DecisionOutcome(result.Outcome) {
			case triage.DecisionApplied, triage.DecisionAlreadyApplied:
				return nil
			default:
				return fmt.Errorf("inbox decision for %s was not applied: %s", args[0], result.Outcome)
			}
		},
	}
	decide.Flags().StringVar(&op, "op", "", "acquire or dismiss")
	decide.Flags().StringVar(&watchScope, "watch-scope", "all", "for dismiss: all, or a comma-separated list of watch IDs")
	decide.Flags().BoolVar(&deleteGrab, "delete-grab", false, "confirm that dismissing a captured PDF grab permanently deletes its file")

	command.AddCommand(counts, decide)
	return command
}

// triageDecision mirrors the daemon's triage.decide result. The daemon's own
// type is unexported, and its outcome vocabulary is the point: an item someone
// else already answered is a conflict to report, not an error to raise.
type triageDecision struct {
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

func printTriageDecision(opt *options, itemID string, result triageDecision) error {
	if opt.jsonOutput {
		return opt.printJSON(result)
	}
	// The daemon reports conflict and already_applied as outcomes rather
	// than errors, so never print a success line for a decision that did
	// not apply.
	if result.Detail != "" {
		_, err := fmt.Fprintf(opt.out, "%s\t%s\t%s\n", itemID, result.Outcome, result.Detail)
		return err
	}
	_, err := fmt.Fprintf(opt.out, "%s\t%s\n", itemID, result.Outcome)
	return err
}

// parseWatchScope converts the CLI's comma-separated form into the two shapes
// triage.decide accepts: the literal string "all", or a list of watch IDs.
func parseWatchScope(value string) (any, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "all" {
		return "all", nil
	}
	var ids []int64
	for _, field := range strings.Split(trimmed, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("--watch-scope must be all or positive watch IDs, got %q", value)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func printInboxItem(opt *options, item triage.Item) error {
	switch item.Kind {
	case triage.KindWatchHit:
		labels := make([]string, 0, len(item.WatchHit.Watches))
		for _, watched := range item.WatchHit.Watches {
			labels = append(labels, watched.Label)
		}
		// item.Title is third-party bibliographic metadata for this kind: it is
		// hit.Work.Title from a Crossref/OpenAlex/RSS watch match (see
		// internal/triage/triage.go), bounded in length but never stripped of
		// control bytes. Route it through the same StripTerminalControls choke
		// point as the watch digest row, or an attacker-registered title
		// injects ANSI/OSC escapes into `papio inbox`.
		_, err := fmt.Fprintf(opt.out, "%d\twatch hit\t%s\t[%s]\n", item.Rank, store.StripTerminalControls(item.Title), strings.Join(labels, ", "))
		return err
	case triage.KindHumanAction:
		_, err := fmt.Fprintf(opt.out, "%d\taction\t%s\t%s\n", item.Rank, item.HumanAction.ActionKind, item.HumanAction.JobID)
		return err
	case triage.KindRetraction:
		// item.Retraction.DOI is third-party bibliographic metadata from a
		// Retraction Watch feed match (internal/retraction), normalized only by
		// work.NormalizeDOI. NormalizeDOI does NOT strip control bytes: its
		// doiCoreRE uses \S, which in RE2 excludes only [\t\n\f\r ] — ESC, BEL,
		// DEL and the whole C1 block all match \S and survive normalization
		// intact. Route it through StripTerminalControls like the watch-hit
		// row above, or a poisoned retraction notice DOI injects ANSI/OSC
		// escapes into `papio inbox`.
		_, err := fmt.Fprintf(opt.out, "%d\t%s\t%s\n", item.Rank, item.Retraction.Nature, store.StripTerminalControls(item.Retraction.DOI))
		return err
	case triage.KindPdfGrab:
		// item.Title is the grab's label, which is derived from the captured
		// file's own name — attacker-influenced in exactly the way the watch-hit
		// and retraction rows above document, so it takes the same choke point.
		// Printing a bare error for this kind made ONE parked grab fail the
		// whole `papio inbox` render, hiding every other pending item.
		_, err := fmt.Fprintf(opt.out, "%d\tpdf grab\t%s\t%s\t%s\n", item.Rank,
			item.PdfGrab.GrabID, item.PdfGrab.State, store.StripTerminalControls(item.Title))
		return err
	default:
		return fmt.Errorf("unsupported triage item kind %q", item.Kind)
	}
}
