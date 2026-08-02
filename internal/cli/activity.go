// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
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
					formatActivityAt(entry.At), shortID, entry.Kind, compactActivitySummary(entry)); err != nil {
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

func compactActivitySummary(entry store.ActivityEntry) string {
	parts := make([]string, 0, 2)
	if title := strings.Join(strings.Fields(entry.JobTitle), " "); title != "" {
		parts = append(parts, title)
	}
	if len(entry.Detail) != 0 {
		if detail, err := json.Marshal(entry.Detail); err == nil && string(detail) != "{}" {
			parts = append(parts, string(detail))
		}
	}
	if len(parts) != 0 {
		return strings.Join(parts, " ")
	}
	if state := strings.TrimSpace(entry.JobState); state != "" {
		return state
	}
	return "-"
}
