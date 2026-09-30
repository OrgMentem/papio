// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
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
	// Replaced is true when --force replaced a file that already existed at
	// Output; without --force an existing file is refused, never replaced.
	Replaced bool `json:"replaced"`
}

// newExportCommand exports normalized citation records — never a raw
// round-trip (dev/scratch/oracle/papio-integrations-r2.md §4A): only known
// values are projected, author names stay literal, and no field is guessed.
func newExportCommand(opt *options) *cobra.Command {
	var format, output string
	var includeDuplicates, force bool
	command := &cobra.Command{
		Use:   "export",
		Short: "Export normalized citation records (CSL-JSON, RIS, BibTeX)",
	}
	command.PersistentFlags().StringVar(&format, "format", "", "citation format: csl-json, ris, or bibtex (default csl-json, inferred from -o's extension)")
	command.PersistentFlags().StringVarP(&output, "output", "o", "", "write citations to this file instead of stdout (required with --json)")
	command.PersistentFlags().BoolVar(&includeDuplicates, "include-duplicates", false, "keep records whose canonical identity repeats instead of collapsing them")
	command.PersistentFlags().BoolVar(&force, "force", false, "replace the -o file when it already exists (without it, an existing file is refused)")

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
		replaced, err := writeExportFile(output, payload, force)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(payload)
		result := exportResult{
			Format:              resolved,
			Records:             len(records),
			DuplicatesCollapsed: collapsed,
			SHA256:              hex.EncodeToString(sum[:]),
			Output:              output,
			Replaced:            replaced,
		}
		if replaced {
			return opt.printResult(result, "Exported %d record(s) to %s, replacing the existing file", len(records), output)
		}
		return opt.printResult(result, "Exported %d record(s) to %s", len(records), output)
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
			if err := opt.call(cmd.Context(), "watch.digest", map[string]any{"id": id, "limit": watch.DigestLimitMax}, &digest); err != nil {
				return err
			}
			// watch.digest has no cursor and no total, so a full page is
			// indistinguishable from a digest with more pending works behind
			// it. Refuse rather than write a file that looks complete.
			if len(digest.Entries) >= watch.DigestLimitMax {
				return fmt.Errorf("watch %d has %d or more pending digest works, the most one export can read; nothing was written. Acquire or clear some with `papio inbox decide` and export again", id, watch.DigestLimitMax)
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
			if truncated && ledgerPageMissesCutoff(rows, cutoff) {
				return fmt.Errorf("the ledger has more than %d matching jobs, the most one export can read; nothing was written. Narrow it with --since or --consumer", job.ListLimitMax)
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

// writeExportFile writes the citation payload to path through a temporary
// sibling, so a failed write never leaves a partial file behind. An existing
// file is replaced only with --force: an export target is often a curated
// citation file, and a silent overwrite destroys it. Without --force the
// finished temp file is hard-linked into place, which fails rather than
// clobbers when path already exists. replaced reports whether --force
// replaced an existing file.
func writeExportFile(path string, payload []byte, force bool) (replaced bool, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return false, fmt.Errorf("writing export: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("writing export: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("writing export: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("writing export: %w", err)
	}
	if !force {
		if err := os.Link(name, path); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return false, fmt.Errorf("%s already exists; nothing was written. Pass --force to replace it", path)
			}
			return false, fmt.Errorf("writing export: %w", err)
		}
		return false, nil
	}
	if _, err := os.Lstat(path); err == nil {
		replaced = true
	}
	if err := os.Rename(name, path); err != nil {
		return false, fmt.Errorf("writing export: %w", err)
	}
	return replaced, nil
}

// ledgerPageMissesCutoff reports whether a truncated jobs page can still be
// missing works the export asked for. The page is newest-first, so once it
// holds a job created before the --since cutoff every job after the cutoff
// is already on it; without a cutoff, a truncated page is always incomplete.
func ledgerPageMissesCutoff(rows []api.JobRow, cutoff time.Time) bool {
	if cutoff.IsZero() {
		return true
	}
	for _, row := range rows {
		created, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
		if err == nil && created.Before(cutoff) {
			return false
		}
	}
	return true
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
