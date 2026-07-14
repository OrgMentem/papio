// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"papio/internal/api"
	"papio/internal/daemon"
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
	var wait, fromZotio, autoImport bool
	var batchPath string
	command := &cobra.Command{
		Use:   "acquire [identifier]",
		Short: "Submit one paper-acquisition request",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			autoImportOverride := boolOverride(cmd, "auto-import", autoImport)
			if cmd.Flags().Changed("batch") {
				if batchPath == "" {
					return fmt.Errorf("--batch requires a JSONL file path or - for standard input")
				}
				if err := validateBatchFlags(cmd, args, fromZotio, wait); err != nil {
					return err
				}
				return acquireBatch(cmd.Context(), cmd, opt, batchPath, autoImportOverride)
			}
			if fromZotio {
				if len(args) != 0 || doi != "" || pmid != "" || arxivID != "" || isbn != "" || openalex != "" ||
					title != "" || requestID != "" || zotioKey != "" || len(authors) != 0 || year != 0 {
					return fmt.Errorf("--from-zotio cannot be combined with one-work identity flags")
				}
				if wait {
					return fmt.Errorf("--wait is not supported with --from-zotio; inspect the returned job IDs")
				}
				if autoImportOverride != nil {
					return fmt.Errorf("--auto-import is not supported with --from-zotio")
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
			if err := submitAcquire(cmd.Context(), opt, request, autoImportOverride, &submitted); err != nil {
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
	flags.StringVar(&batchPath, "batch", "", "submit JSONL works from a file or - for standard input")
	flags.BoolVar(&autoImport, "auto-import", false, "plan and apply Zotio import automatically when ready")
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

type acquireSubmitParams struct {
	Request    protocol.WorkRequest `json:"request"`
	AutoImport *bool                `json:"auto_import,omitempty"`
}

type batchWorkInput struct {
	DOI            string   `json:"doi,omitempty"`
	PMID           string   `json:"pmid,omitempty"`
	ArXiv          string   `json:"arxiv,omitempty"`
	ISBN           string   `json:"isbn,omitempty"`
	OpenAlex       string   `json:"openalex,omitempty"`
	Title          string   `json:"title,omitempty"`
	Authors        []string `json:"authors,omitempty"`
	Year           int      `json:"year,omitempty"`
	DesiredVersion string   `json:"desired_version,omitempty"`
}

type batchSubmission struct {
	JobID string `json:"job_id"`
	State string `json:"state"`
}

type batchSubmitResult struct {
	Jobs      []batchSubmission `json:"jobs"`
	Submitted int               `json:"submitted"`
	Failed    int               `json:"failed"`
}

func boolOverride(cmd *cobra.Command, name string, value bool) *bool {
	if !cmd.Flags().Changed(name) {
		return nil
	}
	return &value
}

func validateBatchFlags(cmd *cobra.Command, args []string, fromZotio, wait bool) error {
	if len(args) != 0 {
		return fmt.Errorf("--batch cannot be combined with a positional identifier")
	}
	if fromZotio {
		return fmt.Errorf("--batch cannot be combined with --from-zotio")
	}
	if wait {
		return fmt.Errorf("--wait is not supported with --batch")
	}
	for _, name := range []string{
		"doi", "pmid", "arxiv", "isbn", "openalex", "title", "author", "year",
		"request-id", "zotio-item-key", "collection", "desired-version", "access-mode",
		"max-cost", "source", "deny-source", "limit",
	} {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--batch cannot be combined with --%s; put per-work values in JSONL", name)
		}
	}
	return nil
}

func parseBatch(r io.Reader) ([]protocol.WorkRequest, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4<<10), 1<<20)
	var requests []protocol.WorkRequest
	for line := 1; scanner.Scan(); line++ {
		data := []byte(strings.TrimSpace(scanner.Text()))
		if len(data) == 0 {
			continue
		}
		values := []json.RawMessage{data}
		if data[0] == '[' {
			if err := json.Unmarshal(data, &values); err != nil {
				return nil, fmt.Errorf("batch line %d: decoding JSON array: %w", line, err)
			}
		}
		for _, value := range values {
			request, err := parseBatchWork(value)
			if err != nil {
				return nil, fmt.Errorf("batch line %d: %w", line, err)
			}
			requests = append(requests, request)
			if len(requests) > 50 {
				return nil, fmt.Errorf("batch exceeds maximum of 50 works")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading batch: %w", err)
	}
	if len(requests) == 0 {
		return nil, fmt.Errorf("batch contains no works")
	}
	return requests, nil
}

func parseBatchWork(data json.RawMessage) (protocol.WorkRequest, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return protocol.WorkRequest{}, fmt.Errorf("decoding JSON object: %w", err)
	}
	if envelope == nil {
		return protocol.WorkRequest{}, fmt.Errorf("work must be a JSON object")
	}
	if wrapped, ok := envelope["work"]; ok {
		data = wrapped
	}
	var input batchWorkInput
	if err := json.Unmarshal(data, &input); err != nil {
		return protocol.WorkRequest{}, fmt.Errorf("decoding work: %w", err)
	}
	identifiers, err := batchIdentifiers(input)
	if err != nil {
		return protocol.WorkRequest{}, err
	}
	authors := trimNonempty(append([]string(nil), input.Authors...))
	desired := strings.TrimSpace(input.DesiredVersion)
	if desired == "" {
		desired = "any"
	}
	request := protocol.WorkRequest{
		SchemaVersion:  protocol.WorkRequestSchemaVersion,
		RequestID:      batchRequestID(identifiers, input.Title, authors, input.Year),
		Identifiers:    identifiers,
		Title:          strings.TrimSpace(input.Title),
		Authors:        authors,
		Year:           input.Year,
		DesiredVersion: desired,
	}
	if err := request.Validate(); err != nil {
		return protocol.WorkRequest{}, err
	}
	return request, nil
}

func batchIdentifiers(input batchWorkInput) (*protocol.Identifiers, error) {
	ids := &protocol.Identifiers{}
	var err error
	for _, field := range []struct {
		name  string
		raw   string
		value *string
		parse func(string) (string, error)
	}{
		{"doi", input.DOI, &ids.DOI, work.NormalizeDOI},
		{"pmid", input.PMID, &ids.PMID, work.NormalizePMID},
		{"arxiv", input.ArXiv, &ids.ArXiv, work.NormalizeArXiv},
		{"isbn", input.ISBN, &ids.ISBN, work.NormalizeISBN},
		{"openalex", input.OpenAlex, &ids.OpenAlex, work.NormalizeOpenAlex},
	} {
		if strings.TrimSpace(field.raw) == "" {
			continue
		}
		*field.value, err = field.parse(field.raw)
		if err != nil {
			return nil, fmt.Errorf("normalizing %s: %w", field.name, err)
		}
	}
	if ids.DOI == "" && ids.PMID == "" && ids.ArXiv == "" && ids.ISBN == "" && ids.OpenAlex == "" {
		return nil, nil
	}
	return ids, nil
}

func batchRequestID(ids *protocol.Identifiers, title string, authors []string, year int) string {
	key := ""
	if ids != nil {
		switch {
		case ids.DOI != "":
			key = "doi:" + ids.DOI
		case ids.ArXiv != "":
			key = "arxiv:" + ids.ArXiv
		case ids.PMID != "":
			key = "pmid:" + ids.PMID
		case ids.ISBN != "":
			key = "isbn:" + ids.ISBN
		case ids.OpenAlex != "":
			key = "openalex:" + ids.OpenAlex
		}
	}
	if key == "" {
		key = fmt.Sprintf("title:%s\nauthors:%s\nyear:%d", strings.TrimSpace(title), strings.Join(authors, "\x00"), year)
	}
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("batch-%x", sum[:4])
}

func submitAcquire(ctx context.Context, opt *options, request protocol.WorkRequest, autoImport *bool, result *api.SubmitResult) error {
	var params any = request
	if autoImport != nil {
		params = acquireSubmitParams{Request: request, AutoImport: autoImport}
	}
	return opt.call(ctx, "acquire.submit", params, result)
}

func acquireBatch(ctx context.Context, cmd *cobra.Command, opt *options, path string, autoImport *bool) error {
	var reader io.Reader = cmd.InOrStdin()
	var file *os.File
	if path != "-" {
		var err error
		file, err = os.Open(path)
		if err != nil {
			return fmt.Errorf("opening batch %q: %w", path, err)
		}
		defer file.Close()
		reader = file
	}
	requests, err := parseBatch(reader)
	if err != nil {
		return err
	}
	cfg, err := opt.loadConfig()
	if err != nil {
		return err
	}
	socket := filepath.Join(cfg.DataDir, "papio.sock")
	starter := daemon.NewAutostarter(socket)
	starter.Args = []string{"--config", cfg.Path, "daemon", "--socket", socket}
	if err := starter.Ensure(ctx); err != nil {
		return err
	}

	results := make([]batchSubmission, len(requests))
	errs := make([]error, len(requests))
	var group sync.WaitGroup
	for index, request := range requests {
		group.Add(1)
		go func(index int, request protocol.WorkRequest) {
			defer group.Done()
			params := any(request)
			if autoImport != nil {
				params = acquireSubmitParams{Request: request, AutoImport: autoImport}
			}
			var submitted api.SubmitResult
			if err := callSocket(ctx, socket, "acquire.submit", params, &submitted); err != nil {
				errs[index] = err
				return
			}
			results[index].JobID = submitted.JobID
			var detail api.JobDetail
			if err := callSocket(ctx, socket, "jobs.get", map[string]string{"job_id": submitted.JobID}, &detail); err != nil {
				results[index].State = "unknown"
				errs[index] = fmt.Errorf("getting state for %s: %w", submitted.JobID, err)
				return
			}
			if detail.Job == nil {
				results[index].State = "unknown"
				errs[index] = fmt.Errorf("daemon returned no job for %s", submitted.JobID)
				return
			}
			results[index].State = detail.Job.State
		}(index, request)
	}
	group.Wait()

	output := batchSubmitResult{Jobs: make([]batchSubmission, 0, len(results))}
	var firstErr error
	for index, result := range results {
		if result.JobID == "" {
			output.Failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("submitting batch work %d: %w", index+1, errs[index])
			}
			continue
		}
		output.Jobs = append(output.Jobs, result)
		output.Submitted++
		if firstErr == nil && errs[index] != nil {
			firstErr = errs[index]
		}
	}
	if opt.jsonOutput {
		if err := opt.printJSON(output); err != nil {
			return err
		}
	} else {
		for _, result := range output.Jobs {
			if _, err := fmt.Fprintf(opt.out, "%s %s\n", result.JobID, result.State); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(opt.out, "Submitted %d job(s); %d failed\n", output.Submitted, output.Failed); err != nil {
			return err
		}
	}
	return firstErr
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
