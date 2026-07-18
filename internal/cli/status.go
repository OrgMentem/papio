// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/config"
	"papio/internal/errcat"
	"papio/internal/job"
)

const statusRecentWindow = 24 * time.Hour

type statusSnapshot struct {
	GeneratedAt string        `json:"generated_at"`
	Groups      []statusGroup `json:"groups"`
}

type statusGroup struct {
	Phase string      `json:"phase"`
	Jobs  []statusJob `json:"jobs"`
}

type statusJob struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Provider     string `json:"provider"`
	State        string `json:"state"`
	Age          string `json:"age"`
	Reason       string `json:"reason,omitempty"`
	Category     string `json:"category,omitempty"`
	Guidance     string `json:"guidance,omitempty"`
	ImportStatus string `json:"import_status,omitempty"`
}

func newStatusCommand(opt *options) *cobra.Command {
	var follow bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Show active and recent acquisition jobs",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			refresh := func() error {
				snapshot, err := loadStatusSnapshot(cmd.Context(), opt, time.Now())
				if err != nil {
					return err
				}
				if opt.jsonOutput {
					return opt.printJSON(snapshot)
				}
				return renderStatusRefresh(opt.out, snapshot, follow && statusTTY(opt.out))
			}
			if !follow {
				return refresh()
			}
			if err := refresh(); err != nil {
				return err
			}
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-cmd.Context().Done():
					return nil
				case <-ticker.C:
					if err := refresh(); err != nil {
						return err
					}
				}
			}
		},
	}
	command.Flags().BoolVar(&follow, "follow", false, "refresh every 2 seconds")
	return command
}

func loadStatusSnapshot(ctx context.Context, opt *options, now time.Time) (statusSnapshot, error) {
	var rows []job.Row
	if err := opt.call(ctx, "jobs.list", map[string]any{"limit": 500}, &rows); err != nil {
		return statusSnapshot{}, err
	}

	details := make(map[string]api.JobDetail, len(rows))
	for _, row := range rows {
		if !showStatusRow(row, now) {
			continue
		}
		var detail api.JobDetail
		if err := opt.call(ctx, "jobs.get", map[string]string{"job_id": row.ID}, &detail); err != nil {
			return statusSnapshot{}, err
		}
		details[row.ID] = detail
	}
	cfg, _ := opt.loadConfig() // best-effort; guidance degrades to generic without it
	return buildStatusSnapshot(rows, details, now, cfg), nil
}

func buildStatusSnapshot(rows []job.Row, details map[string]api.JobDetail, now time.Time, cfg config.Config) statusSnapshot {
	groups := map[string][]statusJob{
		"working":            nil,
		"awaiting_human":     nil,
		"needs_review":       nil,
		"ready":              nil,
		"failed_unavailable": nil,
	}
	for _, row := range rows {
		if !showStatusRow(row, now) {
			continue
		}
		group := statusPhase(row.State)
		if group == "" {
			continue
		}
		detail := details[row.ID]
		item := statusJob{
			ID:       row.ID,
			Title:    shortTitle(row.Work.Describe()),
			Provider: eventProvider(detail.Events),
			State:    row.State,
			Age:      formatStatusAge(row.UpdatedAt, now),
		}
		if item.Title == "" {
			item.Title = "(untitled)"
		}
		if group == "awaiting_human" || group == "needs_review" || group == "failed_unavailable" {
			item.Reason = transitionReason(detail.Events, row.State)
			exp := errcat.Explain(row.State, item.Reason, row.Policy.Resolver, row.Policy.AccessMode, cfg)
			item.Category = exp.Category
			item.Guidance = exp.Guidance
		}
		if group == "ready" {
			item.ImportStatus = autoImportStatus(detail.Events)
		}
		groups[group] = append(groups[group], item)
	}

	ordered := []struct {
		phase string
		label string
	}{
		{"working", "working"},
		{"awaiting_human", "awaiting_human"},
		{"needs_review", "needs_review"},
		{"ready", "ready (last 24h)"},
		{"failed_unavailable", "failed / unavailable"},
	}
	snapshot := statusSnapshot{GeneratedAt: now.UTC().Format(time.RFC3339Nano)}
	for _, group := range ordered {
		if len(groups[group.phase]) == 0 {
			continue
		}
		snapshot.Groups = append(snapshot.Groups, statusGroup{Phase: group.label, Jobs: groups[group.phase]})
	}
	return snapshot
}

func showStatusRow(row job.Row, now time.Time) bool {
	if !job.Terminal(row.State) {
		return true
	}
	updated, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
	return err == nil && !updated.Before(now.Add(-statusRecentWindow))
}

func statusPhase(state string) string {
	switch state {
	case job.StateQueued, job.StateResolving, job.StateFetching, job.StateValidating, job.StateRetryWait:
		return "working"
	case job.StateAwaitingHuman:
		return "awaiting_human"
	case job.StateNeedsReview:
		return "needs_review"
	case job.StateReady:
		return "ready"
	case job.StateFailed, job.StateUnavailable, job.StateCancelled:
		return "failed_unavailable"
	default:
		return ""
	}
}

func eventProvider(events []map[string]any) string {
	for i := len(events) - 1; i >= 0; i-- {
		if value := eventDetailString(events[i], "source"); value != "" {
			return value
		}
	}
	return "—"
}

func transitionReason(events []map[string]any, state string) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] != "job.transition" || eventDetailString(events[i], "to") != state {
			continue
		}
		if reason := eventDetailString(events[i], "reason"); reason != "" {
			return shortText(reason, 72)
		}
	}
	return "—"
}

func autoImportStatus(events []map[string]any) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["kind"] == "zotio.auto_import" {
			if status := eventDetailString(events[i], "status"); status != "" {
				return status
			}
		}
	}
	return "—"
}

func eventDetailString(event map[string]any, key string) string {
	detail, _ := event["detail"].(map[string]any)
	value, _ := detail[key].(string)
	return value
}

func shortTitle(value string) string { return shortText(value, 50) }

func shortText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func formatStatusAge(timestamp string, now time.Time) string {
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "—"
	}
	age := now.Sub(at)
	if age <= 0 || age < time.Minute {
		return "now"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd", int(age/(24*time.Hour)))
}

func renderStatusRefresh(out io.Writer, snapshot statusSnapshot, terminal bool) error {
	if terminal {
		if _, err := fmt.Fprint(out, "\x1b[H\x1b[2J"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "papio status  %s\n", snapshot.GeneratedAt); err != nil {
		return err
	}
	if len(snapshot.Groups) == 0 {
		_, err := fmt.Fprintln(out, "No active or recent jobs.")
		return err
	}
	for _, group := range snapshot.Groups {
		if _, err := fmt.Fprintf(out, "\n%s\n", strings.ToUpper(group.Phase)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, "TITLE                                               PROVIDER            STATE             AGE     DETAIL"); err != nil {
			return err
		}
		for _, item := range group.Jobs {
			detail := item.Category
			if detail == "" {
				detail = item.Reason
			}
			if item.ImportStatus != "" {
				detail = "import=" + item.ImportStatus
			}
			if _, err := fmt.Fprintf(out, "%-50s  %-18s  %-16s  %-6s  %s\n", item.Title, item.Provider, item.State, item.Age, detail); err != nil {
				return err
			}
			if item.Guidance != "" {
				if _, err := fmt.Fprintf(out, "    → %s\n", item.Guidance); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func statusTTY(out io.Writer) bool {
	file, ok := out.(interface{ Fd() uintptr })
	return ok && (isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd()))
}
