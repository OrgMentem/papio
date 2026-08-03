// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"papio/internal/ipc"
	"papio/internal/ownership"
	"papio/internal/protocol"
	"papio/internal/work"
	"papio/internal/zotio"
)

// Caller is the narrow daemon RPC contract used by every batch submitter.
type Caller interface {
	Call(context.Context, string, any, any) error
}

// SubmitOptions selects the standard batch acquisition policy.
type SubmitOptions struct {
	AutoImport   *bool
	Collection   string
	Label        string
	Resolver     string
	IncludeOwned bool
	// Holdings routes ownership through the generic holdings providers
	// (library.lookup_works) instead of zotio. The caller decides, because only it
	// knows whether zotio is configured; mixing the two is out of scope
	// (ADR-0008).
	Holdings bool
	// LibraryFingerprint binds generic holdings lookups to the config that
	// selected them, preventing a shared daemon from answering against another
	// client's sources.
	LibraryFingerprint string
	// Consumer names the submitting consumer on every work in the batch. One
	// name for the whole batch, because a batch is one caller's submission.
	Consumer string
	Now      time.Time
}

// Submission records one successfully created acquisition job.
type Submission struct {
	RequestID string `json:"request_id"`
	JobID     string `json:"job_id"`
	State     string `json:"state"`
}

// SubmitOutput describes one persisted batch submission.
type SubmitOutput struct {
	BatchID          string                 `json:"batch_id"`
	Submitted        []Submission           `json:"submitted"`
	SkippedOwned     []protocol.WorkRequest `json:"skipped_owned"`
	ExistingItem     []protocol.WorkRequest `json:"existing_item"`
	StalenessWarning string                 `json:"staleness_warning,omitempty"`
	Failed           int                    `json:"failed"`
}

type workInput struct {
	DOI            string   `json:"doi,omitempty"`
	PMID           string   `json:"pmid,omitempty"`
	ArXiv          string   `json:"arxiv,omitempty"`
	ISBN           string   `json:"isbn,omitempty"`
	OpenAlex       string   `json:"openalex,omitempty"`
	Title          string   `json:"title,omitempty"`
	Authors        []string `json:"authors,omitempty"`
	Year           int      `json:"year,omitempty"`
	DesiredVersion string   `json:"desired_version,omitempty"`
	Container      string   `json:"container,omitempty"`
}

type discoveredWorkEnvelope struct {
	Work         json.RawMessage `json:"work"`
	OpenAlexID   json.RawMessage `json:"openalex_id"`
	IsOA         json.RawMessage `json:"is_oa"`
	OAURL        json.RawMessage `json:"oa_url"`
	CitedBy      json.RawMessage `json:"cited_by"`
	Abstract     json.RawMessage `json:"abstract"`
	Owned        json.RawMessage `json:"owned"`
	OwnedItemKey json.RawMessage `json:"owned_item_key"`
}

// ParseWork decodes one bare work or discovered-work envelope with the same
// canonicalization and validation used by acquire --batch.
func ParseWork(data json.RawMessage) (protocol.WorkRequest, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return protocol.WorkRequest{}, fmt.Errorf("decoding JSON object: %w", err)
	}
	if envelope == nil {
		return protocol.WorkRequest{}, fmt.Errorf("work must be a JSON object")
	}
	if _, ok := envelope["work"]; ok {
		var discovered discoveredWorkEnvelope
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&discovered); err != nil {
			return protocol.WorkRequest{}, fmt.Errorf("decoding discovered work: %w", err)
		}
		data = discovered.Work
	}
	var input workInput
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		return protocol.WorkRequest{}, fmt.Errorf("decoding work: %w", err)
	}
	identifiers, err := identifiers(input)
	if err != nil {
		return protocol.WorkRequest{}, err
	}
	authors := trimNonempty(append([]string(nil), input.Authors...))
	desired := strings.TrimSpace(input.DesiredVersion)
	if desired == "" {
		desired = "any"
	}
	request := protocol.WorkRequest{
		SchemaVersion: protocol.WorkRequestSchemaVersion,
		RequestID:     batchRequestID(identifiers, input.Title, authors, input.Year),
		Identifiers:   identifiers, Title: strings.TrimSpace(input.Title), Authors: authors, Year: input.Year, DesiredVersion: desired,
	}
	if err := request.Validate(); err != nil {
		return protocol.WorkRequest{}, err
	}
	return request, nil
}

func identifiers(input workInput) (*protocol.Identifiers, error) {
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

func trimNonempty(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

const batchIdentityHashBytes = 16

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
	return fmt.Sprintf("batch-%x", sum[:batchIdentityHashBytes])
}

// InitialRequestID returns the pre-manifest deterministic request identity
// used when parsing CLI or MCP batch input.
func InitialRequestID(ids *protocol.Identifiers, title string, authors []string, year int) string {
	return batchRequestID(ids, title, authors, year)
}

// ApplyOwnership assigns the batch collection, skips complete owned works by
// default, and routes missing attachments through their existing Zotero parent.
func ApplyOwnership(requests []protocol.WorkRequest, ownership zotio.LookupWorksResult, collection string, includeOwned bool) ([]protocol.WorkRequest, int, error) {
	if len(ownership.Works) != len(requests) {
		return nil, 0, fmt.Errorf("Zotio ownership lookup returned %d results for %d works", len(ownership.Works), len(requests))
	}
	collection = strings.TrimSpace(collection)
	pending := make([]protocol.WorkRequest, 0, len(requests))
	skipped := 0
	for i, request := range requests {
		request.Collection = collection
		classification := ownership.Works[i]
		switch classification.Status {
		case zotio.OwnershipNotOwned:
			pending = append(pending, request)
		case zotio.OwnershipOwnedWithPDF:
			if includeOwned {
				pending = append(pending, request)
			} else {
				skipped++
			}
		case zotio.OwnershipOwnedMissingPDF:
			if strings.TrimSpace(classification.ItemKey) == "" {
				return nil, 0, fmt.Errorf("Zotio ownership result %d is missing its parent item key", i+1)
			}
			request.ZotioItemKey = classification.ItemKey
			pending = append(pending, request)
		default:
			return nil, 0, fmt.Errorf("Zotio ownership result %d has unknown status %q", i+1, classification.Status)
		}
	}
	return pending, skipped, nil
}

type submitParams struct {
	Request    protocol.WorkRequest `json:"request"`
	AutoImport *bool                `json:"auto_import,omitempty"`
	Consumer   string               `json:"consumer,omitempty"`
}

// submitResult decodes acquire.submit_v2. Existing is declared because it must
// be, not because batch reports it: internal/ipc decodes results with
// DisallowUnknownFields, so a struct carrying only job_id would reject every
// v2 response outright. Surfacing "this work was already in flight" in the
// batch manifest is a separate, deliberate change.
type submitResult struct {
	JobID    string `json:"job_id"`
	Existing bool   `json:"existing"`
}

// submitOne prefers acquire.submit_v2 and falls back to the retained v1 method
// when the daemon predates it (v2 shipped in 0.13.0).
//
// The fallback is not optional. A batch is one goroutine per work against a
// possibly older running daemon, and without it every work in a mixed-version
// batch records submission_failed with unknown_method. The single-work CLI path
// has carried this same fallback since v2 was introduced; batch must match it.
func submitOne(ctx context.Context, caller Caller, request protocol.WorkRequest, autoImport *bool, consumer string) (submitResult, error) {
	var submitted submitResult
	// v2 always takes the {request, ...} wrapper; v1 additionally accepts a
	// bare WorkRequest, which is what it was sent before this fallback existed.
	err := caller.Call(ctx, "acquire.submit_v2", submitParams{Request: request, AutoImport: autoImport, Consumer: consumer}, &submitted)
	if err == nil {
		return submitted, nil
	}
	var remote *ipc.RemoteError
	if !errors.As(err, &remote) || remote.Code != "unknown_method" {
		return submitResult{}, err
	}
	if consumer != "" {
		// Dropping the attribution to reach an older daemon would record the
		// batch as nobody's work; say so instead.
		return submitResult{}, fmt.Errorf("consumer attribution requires a daemon that records it; upgrade or restart the daemon from the same installation as this CLI")
	}
	var params any = request
	if autoImport != nil {
		params = submitParams{Request: request, AutoImport: autoImport}
	}
	var legacy submitResult
	if err := caller.Call(ctx, "acquire.submit", params, &legacy); err != nil {
		return submitResult{}, err
	}
	return legacy, nil
}

// jobDetail samples one field out of the jobs.get result. It decodes into raw
// messages on purpose: internal/ipc's client rejects unknown fields, and the
// daemon's result carries the whole job row plus events and actions. Declaring a
// narrow struct made every batch fail on the first field this client did not
// know about, so `papio acquire --batch` reported a decode error after
// successfully creating its jobs. A caller that reads one field must not be
// coupled to the shape of everything beside it.
type jobDetail struct {
	Job     json.RawMessage `json:"job"`
	Events  json.RawMessage `json:"events"`
	Actions json.RawMessage `json:"actions"`
}

// state reads the job state, leniently: unknown siblings inside the row are none
// of this caller's business.
func (d jobDetail) state() (string, bool) {
	if len(d.Job) == 0 || string(d.Job) == "null" {
		return "", false
	}
	var row struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(d.Job, &row); err != nil {
		return "", false
	}
	return row.State, true
}

// Submit runs the batch CLI's ownership lookup, asynchronous job submission,
// state lookup, and manifest write against a daemon caller.
func Submit(ctx context.Context, caller Caller, dataDir string, requests []protocol.WorkRequest, options SubmitOptions) (*SubmitOutput, error) {
	if caller == nil {
		return nil, fmt.Errorf("batch RPC is not configured")
	}
	if len(requests) == 0 {
		return nil, fmt.Errorf("batch contains no works")
	}
	if len(requests) > 50 {
		return nil, fmt.Errorf("batch exceeds maximum of 50 works")
	}
	if resolver := strings.TrimSpace(options.Resolver); resolver != "" {
		for i := range requests {
			requests[i].Resolver = resolver
		}
	}
	if options.Now.IsZero() {
		options.Now = time.Now()
	}
	// Default the target collection to the batch's query context (label) so
	// imported papers are filed under the search that produced them instead of
	// landing loose in the library root.
	if strings.TrimSpace(options.Collection) == "" {
		options.Collection = strings.TrimSpace(options.Label)
	}
	manifest := NewManifest(requests, options.Label, options.Collection, options.Now)
	for i := range requests {
		requests[i].RequestID = manifest.Works[i].RequestID
	}
	manifestIndices := make(map[string]int, len(manifest.Works))
	for i := range manifest.Works {
		manifestIndices[manifest.Works[i].RequestID] = i
	}

	classified, err := classifyBatchOwnership(ctx, caller, requests, options)
	if err != nil {
		return nil, err
	}
	ownership := *classified
	output := &SubmitOutput{BatchID: manifest.ID, Submitted: make([]Submission, 0, len(requests)), StalenessWarning: ownership.StalenessWarning}
	for i, classification := range ownership.Works {
		switch classification.Status {
		case zotio.OwnershipNotOwned:
		case zotio.OwnershipOwnedWithPDF:
			if !options.IncludeOwned {
				manifest.Works[i].Status = "skipped_owned"
				output.SkippedOwned = append(output.SkippedOwned, manifest.Works[i].Work)
			}
		case zotio.OwnershipOwnedMissingPDF:
			manifest.Works[i].Status = "existing_item_attached"
			existing := manifest.Works[i].Work
			existing.ZotioItemKey = classification.ItemKey
			output.ExistingItem = append(output.ExistingItem, existing)
		default:
			return nil, fmt.Errorf("Zotio ownership result %d has unknown status %q", i+1, classification.Status)
		}
	}
	requests, _, err = ApplyOwnership(requests, ownership, options.Collection, options.IncludeOwned)
	if err != nil {
		return nil, err
	}

	results := make([]Submission, len(requests))
	errs := make([]error, len(requests))
	var group sync.WaitGroup
	for index, request := range requests {
		group.Add(1)
		go func(index int, request protocol.WorkRequest) {
			defer group.Done()
			submitted, err := submitOne(ctx, caller, request, options.AutoImport, options.Consumer)
			if err != nil {
				errs[index] = err
				return
			}
			results[index].RequestID, results[index].JobID = request.RequestID, submitted.JobID
			var detail jobDetail
			if err := caller.Call(ctx, "jobs.get", map[string]string{"job_id": submitted.JobID}, &detail); err != nil {
				results[index].State = "unknown"
				errs[index] = fmt.Errorf("getting state for %s: %w", submitted.JobID, err)
				return
			}
			state, ok := detail.state()
			if !ok {
				results[index].State = "unknown"
				errs[index] = fmt.Errorf("daemon returned no job for %s", submitted.JobID)
				return
			}
			results[index].State = state
		}(index, request)
	}
	group.Wait()

	var firstErr error
	for index, result := range results {
		manifestWorkIndex, ok := manifestIndices[requests[index].RequestID]
		if !ok {
			return nil, fmt.Errorf("batch manifest is missing request %q", requests[index].RequestID)
		}
		manifestWork := &manifest.Works[manifestWorkIndex]
		if result.JobID == "" {
			output.Failed++
			manifestWork.Status, manifestWork.Error = "submission_failed", "submit"
			if firstErr == nil {
				if errs[index] == nil {
					firstErr = fmt.Errorf("submitting batch work %d failed without a daemon error", index+1)
				} else {
					firstErr = fmt.Errorf("submitting batch work %d: %w", index+1, errs[index])
				}
			}
			continue
		}
		manifestWork.JobID = result.JobID
		output.Submitted = append(output.Submitted, result)
		if firstErr == nil && errs[index] != nil {
			firstErr = errs[index]
		}
	}
	if err := Write(dataDir, manifest); err != nil {
		return nil, err
	}
	return output, firstErr
}

// classifyBatchOwnership resolves ownership for a batch through whichever
// provider owns the answer, and normalizes both onto the shape the rest of the
// batch pipeline already consumes.
func classifyBatchOwnership(ctx context.Context, caller Caller, requests []protocol.WorkRequest, options SubmitOptions) (*zotio.LookupWorksResult, error) {
	if options.Holdings {
		return classifyFromHoldings(ctx, caller, requests, options)
	}
	lookupRequest := zotio.LookupWorksRequest{Works: make([]zotio.LookupWork, len(requests))}
	for i, request := range requests {
		if request.Identifiers != nil {
			lookupRequest.Works[i] = zotio.LookupWork{DOI: request.Identifiers.DOI, ArXiv: request.Identifiers.ArXiv}
		}
	}
	var result zotio.LookupWorksResult
	if err := caller.Call(ctx, "zotio.lookup_works", lookupRequest, &result); err != nil {
		return nil, err
	}
	if len(result.Works) != len(requests) {
		return nil, fmt.Errorf("Zotio ownership lookup returned %d results for %d works", len(result.Works), len(requests))
	}
	return &result, nil
}

// classifyFromHoldings asks the generic holdings providers and maps their
// decisions onto the two statuses that can apply outside Zotero: skip, or
// acquire. owned_missing_pdf is deliberately unreachable here — it carries a
// Zotero parent key that a generic source cannot supply, and papio has no
// write-back protocol for attaching a file to another manager's existing record
// (ADR-0008 invariant 3).
func classifyFromHoldings(ctx context.Context, caller Caller, requests []protocol.WorkRequest, options SubmitOptions) (*zotio.LookupWorksResult, error) {
	queries := make([]ownership.Query, len(requests))
	for i, request := range requests {
		var doi, arxiv, pmid string
		if request.Identifiers != nil {
			doi, arxiv, pmid = request.Identifiers.DOI, request.Identifiers.ArXiv, request.Identifiers.PMID
		}
		queries[i] = ownership.QueryFor(doi, arxiv, pmid, request.DesiredVersion, "")
	}
	var lookup ownership.Result
	if err := caller.Call(ctx, "library.lookup_works", libraryLookupParams{
		Works:               queries,
		ExpectedFingerprint: options.LibraryFingerprint,
	}, &lookup); err != nil {
		// An older daemon has no such method. Falling back keeps a mixed install
		// working, but it falls back to zotio's *own* semantics rather than
		// pretending the generic sources answered: with zotio unconfigured that
		// yields not-owned plus zotio's staleness warning, which is the honest
		// answer for a daemon that cannot consult the user's library at all.
		var remote *ipc.RemoteError
		if errors.As(err, &remote) && remote.Code == "unknown_method" {
			options.Holdings = false
			return classifyBatchOwnership(ctx, caller, requests, options)
		}
		return nil, fmt.Errorf("library ownership lookup: %w", err)
	}
	if len(lookup.Works) != len(requests) {
		return nil, fmt.Errorf("library ownership lookup returned %d results for %d works", len(lookup.Works), len(requests))
	}
	// A source papio could not read is not a source that holds nothing. Creating
	// jobs anyway would re-acquire the whole batch; the explicit override for
	// "proceed despite ownership uncertainty" is --include-owned.
	if incomplete := lookup.Incomplete(); len(incomplete) != 0 && !options.IncludeOwned {
		return nil, fmt.Errorf("library sources unavailable (%s); ownership could not be verified, so no jobs were created — fix the source or pass --include-owned to acquire anyway", strings.Join(incomplete, ", "))
	}

	result := &zotio.LookupWorksResult{Works: make([]zotio.WorkOwnership, len(requests))}
	for i := range requests {
		result.Works[i].Status = zotio.OwnershipNotOwned
		// RecordPresent alone must not skip: a citation without full text is
		// exactly what a backfill user wants acquired.
		if ownership.Decide(queries[i], lookup.Works[i]).Suppress {
			result.Works[i].Status = zotio.OwnershipOwnedWithPDF
		}
	}
	if incomplete := lookup.Incomplete(); len(incomplete) != 0 {
		result.StalenessWarning = fmt.Sprintf("library sources unavailable (%s); ownership classification is incomplete", strings.Join(incomplete, ", "))
	}
	return result, nil
}

// libraryLookupParams mirrors api.LibraryLookupWorksRequest without importing it
// (internal/api imports this package).
type libraryLookupParams struct {
	Works               []ownership.Query `json:"works"`
	ExpectedFingerprint string            `json:"expected_fingerprint"`
}
