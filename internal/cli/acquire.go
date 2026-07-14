// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/work"
	"papio/internal/zotio"
)

func newAcquireCommand(opt *options) *cobra.Command {
	var doi, pmid, arxivID, isbn, openalex string
	var title, requestID, zotioKey, collection, desiredVersion, accessMode string
	var authors, allowSources, denySources []string
	var year, queueLimit int
	var maxCost float64
	var wait, fromZotio bool
	command := &cobra.Command{
		Use:   "acquire [identifier]",
		Short: "Submit one paper-acquisition request",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if fromZotio {
				if len(args) != 0 || doi != "" || pmid != "" || arxivID != "" || isbn != "" || openalex != "" ||
					title != "" || requestID != "" || zotioKey != "" || len(authors) != 0 || year != 0 {
					return fmt.Errorf("--from-zotio cannot be combined with one-work identity flags")
				}
				if wait {
					return fmt.Errorf("--wait is not supported with --from-zotio; inspect the returned job IDs")
				}
				options := zotio.QueueOptions{
					Collection:         strings.TrimSpace(collection),
					Limit:              queueLimit,
					DesiredVersion:     desiredVersion,
					AccessModeOverride: accessMode,
					SourcesAllow:       trimNonempty(allowSources),
					SourcesDeny:        trimNonempty(denySources),
				}
				if cmd.Flags().Changed("max-cost") {
					options.MaxCostUSD = &maxCost
				}
				var queued zotio.QueueResult
				if err := opt.call(cmd.Context(), "zotio.queue", options, &queued); err != nil {
					return err
				}
				return opt.printResult(queued, "Queued %d Zotio item(s); skipped %d", len(queued.Queued), len(queued.Skipped))
			}
			identifiers, err := normalizeIdentifiers(args, doi, pmid, arxivID, isbn, openalex)
			if err != nil {
				return err
			}
			if requestID == "" {
				requestID = job.NewID("request")
			}
			request := protocol.WorkRequest{
				SchemaVersion:      protocol.WorkRequestSchemaVersion,
				RequestID:          requestID,
				Identifiers:        identifiers,
				Title:              strings.TrimSpace(title),
				Authors:            trimNonempty(authors),
				Year:               year,
				ZotioItemKey:       strings.TrimSpace(zotioKey),
				Collection:         strings.TrimSpace(collection),
				DesiredVersion:     desiredVersion,
				AccessModeOverride: accessMode,
				SourcesAllow:       trimNonempty(allowSources),
				SourcesDeny:        trimNonempty(denySources),
			}
			if cmd.Flags().Changed("max-cost") {
				request.MaxCostUSD = &maxCost
			}
			var submitted api.SubmitResult
			if err := opt.call(cmd.Context(), "acquire.submit", request, &submitted); err != nil {
				return err
			}
			if !wait {
				return opt.printResult(submitted, "Queued %s", submitted.JobID)
			}
			detail, err := waitForJob(cmd.Context(), opt, submitted.JobID)
			if err != nil {
				return err
			}
			return opt.printResult(detail, "%s: %s", detail.Job.ID, detail.Job.State)
		},
	}
	flags := command.Flags()
	flags.StringVar(&doi, "doi", "", "DOI")
	flags.StringVar(&pmid, "pmid", "", "PubMed ID")
	flags.StringVar(&arxivID, "arxiv", "", "arXiv ID")
	flags.StringVar(&isbn, "isbn", "", "ISBN")
	flags.StringVar(&openalex, "openalex", "", "OpenAlex work ID")
	flags.StringVar(&title, "title", "", "work title")
	flags.StringSliceVar(&authors, "author", nil, "author (repeatable)")
	flags.IntVar(&year, "year", 0, "publication year")
	flags.StringVar(&requestID, "request-id", "", "stable idempotency key")
	flags.StringVar(&zotioKey, "zotio-item-key", "", "existing Zotero item key")
	flags.StringVar(&collection, "collection", "", "collection context")
	flags.StringVar(&desiredVersion, "desired-version", "any", "published, accepted, preprint, or any")
	flags.StringVar(&accessMode, "access-mode", "", "per-request access-mode override")
	flags.Float64Var(&maxCost, "max-cost", 0, "maximum paid-source cost in USD")
	flags.StringSliceVar(&allowSources, "source", nil, "allow only this source (repeatable)")
	flags.StringSliceVar(&denySources, "deny-source", nil, "deny this source (repeatable)")
	flags.BoolVar(&wait, "wait", false, "wait for a terminal or human-action state")
	flags.BoolVar(&fromZotio, "from-zotio", false, "queue Zotio items missing an attached PDF")
	flags.IntVar(&queueLimit, "limit", 25, "maximum Zotio queue rows (1-500)")
	return command
}

func normalizeIdentifiers(args []string, doi, pmid, arxivID, isbn, openalex string) (*protocol.Identifiers, error) {
	explicit := 0
	for _, value := range []string{doi, pmid, arxivID, isbn, openalex} {
		if strings.TrimSpace(value) != "" {
			explicit++
		}
	}
	if len(args) != 0 && explicit != 0 {
		return nil, fmt.Errorf("use either the positional identifier or identifier flags, not both")
	}
	if explicit > 1 {
		return nil, fmt.Errorf("only one identifier flag may be used per work request")
	}
	ids := &protocol.Identifiers{}
	var err error
	if len(args) != 0 {
		raw := strings.TrimSpace(args[0])
		lower := strings.ToLower(raw)
		switch {
		case strings.HasPrefix(lower, "10."), strings.Contains(lower, "doi.org/"), strings.HasPrefix(lower, "doi:"):
			ids.DOI, err = work.NormalizeDOI(raw)
		case strings.HasPrefix(lower, "arxiv:"), strings.Contains(lower, "arxiv.org/"):
			ids.ArXiv, err = work.NormalizeArXiv(raw)
		case strings.HasPrefix(lower, "openalex:"), strings.Contains(lower, "openalex.org/"), strings.HasPrefix(strings.ToUpper(raw), "W"):
			ids.OpenAlex, err = work.NormalizeOpenAlex(raw)
		case strings.HasPrefix(lower, "pmid:"):
			ids.PMID, err = work.NormalizePMID(raw)
		case strings.HasPrefix(lower, "isbn:"):
			ids.ISBN, err = work.NormalizeISBN(strings.TrimSpace(raw[len("isbn:"):]))
		case allDigits(raw) && len(raw) <= 9:
			ids.PMID, err = work.NormalizePMID(raw)
		default:
			return nil, fmt.Errorf("cannot infer identifier type %q; use --doi, --arxiv, --pmid, --isbn, or --openalex", raw)
		}
	} else {
		if doi != "" {
			ids.DOI, err = work.NormalizeDOI(doi)
		} else if pmid != "" {
			ids.PMID, err = work.NormalizePMID(pmid)
		} else if arxivID != "" {
			ids.ArXiv, err = work.NormalizeArXiv(arxivID)
		} else if isbn != "" {
			ids.ISBN, err = work.NormalizeISBN(isbn)
		} else if openalex != "" {
			ids.OpenAlex, err = work.NormalizeOpenAlex(openalex)
		}
	}
	if err != nil {
		return nil, err
	}
	if ids.DOI == "" && ids.PMID == "" && ids.ArXiv == "" && ids.ISBN == "" && ids.OpenAlex == "" {
		return nil, nil // a complete title/author/year tuple may identify the work
	}
	return ids, nil
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func trimNonempty(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func waitForJob(ctx context.Context, opt *options, id string) (*api.JobDetail, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		var detail api.JobDetail
		if err := opt.call(ctx, "jobs.get", map[string]string{"job_id": id}, &detail); err != nil {
			return nil, err
		}
		if detail.Job == nil {
			return nil, fmt.Errorf("daemon returned no job for %s", id)
		}
		if job.Terminal(detail.Job.State) || detail.Job.State == job.StateAwaitingHuman || detail.Job.State == job.StateNeedsReview {
			return &detail, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
