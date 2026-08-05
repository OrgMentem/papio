// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/store"
)

// newActivityCommand exposes the daemon's durable event stream as a bounded,
// newest-first operator view. The JSON branch deliberately goes through the
// same page envelope as every other list-shaped command.
func newActivityCommand(opt *options) *cobra.Command {
	var limit int
	var jobID string
	command := &cobra.Command{
		Use:         "activity",
		Short:       "Show recent daemon activity",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var page api.ActivityPage
			if err := opt.call(cmd.Context(), "activity.list", map[string]any{
				"limit":      limit,
				"before_seq": int64(0),
				"job_id":     jobID,
			}, &page); err != nil {
				if isUnknownMethod(err) {
					return daemonUpgradeRequired("activity.list")
				}
				return err
			}
			if opt.jsonOutput {
				return printPage(opt, "entries", page.Entries, page.Truncated)
			}
			for _, entry := range page.Entries {
				shortID := "-"
				if entry.JobID != "" {
					shortID = shortActivityJobID(entry.JobID)
				}
				if _, err := fmt.Fprintf(opt.out, "%s  %s  %s  %s\n",
					formatActivityAt(entry.At), shortID, store.ActivityText(entry.Kind, entry.Detail), compactActivitySummary(entry)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().IntVar(&limit, "limit", 30, "maximum activity rows (1-200)")
	command.Flags().StringVar(&jobID, "job", "", "filter activity to one job ID")
	return command
}

func shortActivityJobID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func formatActivityAt(at time.Time) string {
	age := time.Since(at)
	if age >= 0 && age < 24*time.Hour {
		switch {
		case age < time.Minute:
			return fmt.Sprintf("%ds ago", int(age.Seconds()))
		case age < time.Hour:
			return fmt.Sprintf("%dm ago", int(age.Minutes()))
		default:
			return fmt.Sprintf("%dh ago", int(age.Hours()))
		}
	}
	return at.Local().Format("15:04:05")
}

// compactActivitySummary names the job on the human line: title when known,
// else current state. Raw detail stays on the --json branch — the friendly
// text column already folds the interesting detail fields in.
//
// JobTitle is third-party bibliographic metadata (enrichment stores it after
// only TrimSpace), so it reaches this terminal-printed row un-sanitized; run
// it through the same StripTerminalControls choke point as ActivityText, or
// an ESC/BEL/C1 byte in a DOI-registered title re-injects into the operator's
// terminal on this column even though the other column is already clamped.
func compactActivitySummary(entry store.ActivityEntry) string {
	if title := strings.Join(strings.Fields(store.StripTerminalControls(entry.JobTitle)), " "); title != "" {
		return title
	}
	if state := strings.TrimSpace(entry.JobState); state != "" {
		return state
	}
	return "-"
}
