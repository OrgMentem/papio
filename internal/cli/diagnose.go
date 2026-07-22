// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/redact"
)

var diagnoseURLRE = regexp.MustCompile(`https?://[^\s"'<>]+`)

// diagnosePathRE matches absolute filesystem paths (POSIX with two or more
// segments, or Windows drive paths) so quarantine/data-dir locations embedded
// in action and event details never reach a support report. DOIs are safe:
// they never start with a path separator.
var diagnosePathRE = regexp.MustCompile(`\B/(?:[\w.@+~-]+/)+[\w.@+~-]+|\b[A-Za-z]:\\[^\s"'<>]+`)

type diagnoseReport struct {
	GeneratedAt        string           `json:"generated_at"`
	DaemonVersion      string           `json:"daemon_version"`
	ExtensionConnected bool             `json:"extension_connected"`
	Job                diagnoseJob      `json:"job"`
	ProviderHosts      []string         `json:"provider_hosts"`
	Actions            []diagnoseAction `json:"actions"`
	Events             []diagnoseEvent  `json:"events"`
}

type diagnoseJob struct {
	ID        string         `json:"id"`
	State     string         `json:"state"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	Policy    diagnosePolicy `json:"policy"`
	Work      diagnoseWork   `json:"work"`
}

type diagnosePolicy struct {
	AccessMode      string `json:"access_mode"`
	ResolverProfile string `json:"resolver_profile"`
}

type diagnoseWork struct {
	DOI   string `json:"doi"`
	PMID  string `json:"pmid"`
	Title string `json:"title"`
}

type diagnoseAction struct {
	Kind         string `json:"kind"`
	State        string `json:"state"`
	RequiresAuth bool   `json:"requires_auth"`
	BlockedBy    string `json:"blocked_by"`
	Revision     int64  `json:"revision"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	Detail       string `json:"detail"`
}

type diagnoseEvent struct {
	Kind   string `json:"kind"`
	At     string `json:"at"`
	Detail string `json:"detail"`
}

func newAdapterCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "adapter", Short: "Inspect provider and adapter interactions"}
	diagnose := &cobra.Command{
		Use:         "diagnose <job-id>",
		Short:       "Build a sanitized support report for a job",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var detail api.JobDetail
			if err := opt.call(cmd.Context(), "jobs.get", map[string]string{"job_id": args[0]}, &detail); err != nil {
				return err
			}
			var ping daemonPingResult
			if err := opt.call(cmd.Context(), "ping", struct{}{}, &ping); err != nil {
				return err
			}

			report := buildDiagnoseReport(detail, ping)
			if opt.jsonOutput {
				return opt.printJSON(report)
			}
			return printDiagnoseReport(opt.out, report)
		},
	}
	command.AddCommand(diagnose)
	return command
}

func buildDiagnoseReport(detail api.JobDetail, ping daemonPingResult) diagnoseReport {
	report := diagnoseReport{
		GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
		DaemonVersion:      scrubDiagnoseText(ping.Version),
		ExtensionConnected: ping.ExtensionConnected,
		ProviderHosts:      make([]string, 0),
		Actions:            make([]diagnoseAction, 0, len(detail.Actions)),
		Events:             make([]diagnoseEvent, 0, len(detail.Events)),
	}
	if detail.Job != nil {
		report.Job = diagnoseJob{
			ID:        scrubDiagnoseText(detail.Job.ID),
			State:     scrubDiagnoseText(detail.Job.State),
			CreatedAt: scrubDiagnoseText(detail.Job.CreatedAt),
			UpdatedAt: scrubDiagnoseText(detail.Job.UpdatedAt),
			Policy: diagnosePolicy{
				AccessMode:      scrubDiagnoseText(detail.Job.Policy.AccessMode),
				ResolverProfile: scrubDiagnoseText(detail.Job.Policy.Resolver),
			},
			Work: diagnoseWork{
				DOI:   scrubDiagnoseText(detail.Job.Work.DOI),
				PMID:  scrubDiagnoseText(detail.Job.Work.PMID),
				Title: scrubDiagnoseText(detail.Job.Work.Title),
			},
		}
	}

	seenHosts := make(map[string]struct{})
	addHosts := func(value any) {
		for _, raw := range diagnoseURLs(diagnoseDetail(value)) {
			host := redact.Host(raw)
			if _, seen := seenHosts[host]; seen {
				continue
			}
			seenHosts[host] = struct{}{}
			report.ProviderHosts = append(report.ProviderHosts, host)
		}
	}
	for _, action := range detail.Actions {
		addHosts(action.Detail)
		report.Actions = append(report.Actions, diagnoseAction{
			Kind:         scrubDiagnoseText(action.Kind),
			State:        scrubDiagnoseText(action.Status),
			RequiresAuth: action.RequiresAuth,
			BlockedBy:    scrubDiagnoseText(action.BlockedBy),
			Revision:     action.Revision,
			CreatedAt:    scrubDiagnoseText(action.CreatedAt),
			// HumanAction exposes only its creation time over jobs.get. Do not
			// manufacture an update time for a support report.
			UpdatedAt: "",
			Detail:    scrubDiagnoseText(action.Detail),
		})
	}
	for _, event := range detail.Events {
		value := event["detail"]
		addHosts(value)
		report.Events = append(report.Events, diagnoseEvent{
			Kind:   scrubDiagnoseText(diagnoseDetail(event["kind"])),
			At:     scrubDiagnoseText(diagnoseDetail(event["at"])),
			Detail: scrubDiagnoseText(diagnoseDetail(value)),
		})
	}
	return report
}

func diagnoseDetail(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func diagnoseURLs(text string) []string {
	return diagnoseURLRE.FindAllString(text, -1)
}

func scrubDiagnoseText(text string) string {
	text = diagnoseURLRE.ReplaceAllStringFunc(text, redact.URL)
	return diagnosePathRE.ReplaceAllString(text, "<local-path>")
}

func printDiagnoseReport(out interface{ Write([]byte) (int, error) }, report diagnoseReport) error {
	if _, err := fmt.Fprintf(out, "Job: %s %s (%s; resolver %s) — daemon %s; extension connected: %t\n", report.Job.ID, report.Job.State, report.Job.Policy.AccessMode, report.Job.Policy.ResolverProfile, report.DaemonVersion, report.ExtensionConnected); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Provider hosts: %s\n", strings.Join(report.ProviderHosts, ", ")); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "Actions:"); err != nil {
		return err
	}
	for _, action := range report.Actions {
		hints := make([]string, 0, 2)
		if action.RequiresAuth {
			hints = append(hints, "requires authentication")
		}
		if action.BlockedBy != "" {
			hints = append(hints, "blocked by "+action.BlockedBy)
		}
		if len(hints) == 0 {
			hints = append(hints, "no access restriction")
		}
		if _, err := fmt.Fprintf(out, "  %s %s (%s): %s\n", action.Kind, action.State, strings.Join(hints, "; "), action.Detail); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(out, "Events:"); err != nil {
		return err
	}
	for _, event := range report.Events {
		if _, err := fmt.Fprintf(out, "%s %s %s\n", event.At, event.Kind, event.Detail); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out, "This report is sanitized and safe to attach to an issue.")
	return err
}
