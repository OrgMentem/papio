// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/batch"
	"papio/internal/cite"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/watch"
	"papio/internal/work"
)

// exportResult is the --json operation result: with the global JSON flag the
// citation bytes go to the file named by -o, and stdout carries this receipt
// instead of mixing papio JSON with CSL-JSON.
type exportResult struct {
	Format              string `json:"format"`
	Records             int    `json:"records"`
	DuplicatesCollapsed int    `json:"duplicates_collapsed"`
	SHA256              string `json:"sha256"`
	Output              string `json:"output,omitempty"`
}

// newExportCommand exports normalized citation records — never a raw
// round-trip (dev/scratch/oracle/papio-integrations-r2.md §4A): only known
// values are projected, author names stay literal, and no field is guessed.
func newExportCommand(opt *options) *cobra.Command {
	var format, output string
	var includeDuplicates bool
	command := &cobra.Command{
		Use:   "export",
		Short: "Export normalized citation records (CSL-JSON, RIS, BibTeX)",
	}
	command.PersistentFlags().StringVar(&format, "format", "", "citation format: csl-json, ris, or bibtex (default csl-json, inferred from -o's extension)")
	command.PersistentFlags().StringVarP(&output, "output", "o", "", "write citations to this file instead of stdout (required with --json)")
	command.PersistentFlags().BoolVar(&includeDuplicates, "include-duplicates", false, "keep records whose canonical identity repeats instead of collapsing them")

	emit := func(cmd *cobra.Command, records []cite.Record) error {
		collapsed := 0
		if !includeDuplicates {
			records, collapsed = cite.Dedupe(records)
		}
		resolved, err := resolveExportFormat(format, output)
		if err != nil {
			return err
		}
		if opt.jsonOutput && output == "" {
			return fmt.Errorf("--json requires -o/--output: stdout carries the operation result, not citation bytes")
		}
		payload, err := cite.Render(resolved, records)
		if err != nil {
			return err
		}
		if output == "" {
			_, err := opt.out.Write(payload)
			return err
		}
		if err := os.WriteFile(output, payload, 0o644); err != nil {
			return fmt.Errorf("writing export: %w", err)
		}
		sum := sha256.Sum256(payload)
		return opt.printResult(exportResult{
			Format:              resolved,
			Records:             len(records),
			DuplicatesCollapsed: collapsed,
			SHA256:              hex.EncodeToString(sum[:]),
			Output:              output,
		}, "Exported %d record(s) to %s", len(records), output)
	}

	jobCommand := &cobra.Command{
		Use:   "job <job-id>...",
		Short: "Export the named jobs in argument order (any state: citation metadata stays useful when retrieval failed)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			records := make([]cite.Record, 0, len(args))
			for _, id := range args {
				detail, err := jobDetail(cmd.Context(), opt, id)
				if err != nil {
					return err
				}
				if detail.Job == nil {
					return fmt.Errorf("job %q: daemon returned no job row", id)
				}
				records = append(records, cite.FromWork(detail.Job.Work))
			}
			return emit(cmd, records)
		},
	}

	batchCommand := &cobra.Command{
		Use:   "batch <batch-id>",
		Short: "Export every work in the batch manifest in manifest order, including skipped and unavailable works",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var report batch.Report
			if err := opt.call(cmd.Context(), "acquire.report", api.AcquireReportParams{BatchID: args[0]}, &report); err != nil {
				return err
			}
			records := make([]cite.Record, 0, len(report.Works))
			for _, reportWork := range report.Works {
				records = append(records, recordFromWorkRequest(reportWork.Work))
			}
			return emit(cmd, records)
		},
	}

	watchCommand := &cobra.Command{
		Use:   "watch <watch-id>",
		Short: "Export a watch's pending digest entries",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid watch id %q", args[0])
			}
			var digest api.WatchDigestResult
			if err := opt.call(cmd.Context(), "watch.digest", map[string]any{"id": id, "limit": 500}, &digest); err != nil {
				return err
			}
			records := make([]cite.Record, 0, len(digest.Entries))
			for _, entry := range digest.Entries {
				records = append(records, recordFromDigestEntry(entry))
			}
			return emit(cmd, records)
		},
	}

	var state, since, consumer string
	ledgerCommand := &cobra.Command{
		Use:   "ledger",
		Short: "Export one record per canonical work (ready acquisitions by default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cutoff, err := parseSinceInstant(since)
			if err != nil {
				return err
			}
			params := map[string]any{"limit": job.ListLimitMax}
			if state == "ready" {
				params["state"] = job.StateReady
			} else if state != "any" {
				return fmt.Errorf("--state must be \"ready\" or \"any\", got %q", state)
			}
			if consumer != "" {
				params["consumer"] = consumer
			}
			rows, truncated, err := listAttributedJobsPage(cmd.Context(), opt, params, consumer, job.ListLimitMax)
			if err != nil {
				return err
			}
			if truncated {
				fmt.Fprintln(opt.errOut, "papio: the ledger page was truncated at the daemon's row limit; the export is incomplete")
			}
			records := make([]cite.Record, 0, len(rows))
			for _, row := range rows {
				if !cutoff.IsZero() {
					created, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
					if err != nil || created.Before(cutoff) {
						continue
					}
				}
				records = append(records, cite.FromWork(row.Work))
			}
			return emit(cmd, records)
		},
	}
	ledgerCommand.Flags().StringVar(&state, "state", "ready", "which works to export: ready (validated acquisitions) or any (every job)")
	ledgerCommand.Flags().StringVar(&since, "since", "", "only works submitted after this instant (RFC3339) or within this duration (Go form, e.g. 720h)")
	ledgerCommand.Flags().StringVar(&consumer, "consumer", "", "only works submitted by this consumer")

	command.AddCommand(jobCommand, batchCommand, watchCommand, ledgerCommand)
	return command
}

// resolveExportFormat applies the precedence: an explicit --format wins, the
// -o extension infers one, csl-json is the default.
func resolveExportFormat(format, output string) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	if format != "" {
		return format, nil
	}
	switch strings.ToLower(filepath.Ext(output)) {
	case ".json":
		return "csl-json", nil
	case ".ris":
		return "ris", nil
	case ".bib", ".bibtex":
		return "bibtex", nil
	case "":
		return "csl-json", nil
	default:
		return "csl-json", nil
	}
}

func parseSinceInstant(since string) (time.Time, error) {
	since = strings.TrimSpace(since)
	if since == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(since); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("--since duration must be positive")
		}
		return time.Now().Add(-d), nil
	}
	if at, err := time.Parse(time.RFC3339, since); err == nil {
		return at, nil
	}
	return time.Time{}, fmt.Errorf("--since must be an RFC3339 instant or a Go duration, got %q", since)
}

func recordFromWorkRequest(request protocol.WorkRequest) cite.Record {
	w := work.Work{
		Title:   request.Title,
		Authors: request.Authors,
		Year:    request.Year,
	}
	if request.Identifiers != nil {
		w.DOI = request.Identifiers.DOI
		w.PMID = request.Identifiers.PMID
		w.ArXiv = request.Identifiers.ArXiv
		w.ISBN = request.Identifiers.ISBN
	}
	return cite.FromWork(w)
}

func recordFromDigestEntry(entry watch.DigestEntry) cite.Record {
	record := cite.Record{
		Title:    strings.TrimSpace(entry.Title),
		Year:     entry.Year,
		DOI:      strings.TrimSpace(entry.DOI),
		Abstract: strings.TrimSpace(entry.Abstract),
	}
	// The digest stores authors joined with ", " from the discovery names,
	// so splitting on that separator reconstructs the stored list.
	for _, author := range strings.Split(entry.Authors, ", ") {
		if name := strings.TrimSpace(author); name != "" {
			record.Authors = append(record.Authors, name)
		}
	}
	if entry.Identifiers != nil {
		if record.DOI == "" {
			record.DOI = strings.TrimSpace(entry.Identifiers.DOI)
		}
		record.PMID = strings.TrimSpace(entry.Identifiers.PMID)
		record.ArXiv = strings.TrimSpace(entry.Identifiers.ArXiv)
		record.ISBN = strings.TrimSpace(entry.Identifiers.ISBN)
	}
	return record
}
