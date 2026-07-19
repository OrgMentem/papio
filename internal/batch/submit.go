// Copyright 2026 OrgMentem. Licensed under MIT.

package batch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

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
	Now          time.Time
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
}

type submitResult struct {
	JobID string `json:"job_id"`
}

type jobDetail struct {
	Job *struct {
		State string `json:"state"`
	} `json:"job"`
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

	lookupRequest := zotio.LookupWorksRequest{Works: make([]zotio.LookupWork, len(requests))}
	for i, request := range requests {
		if request.Identifiers != nil {
			lookupRequest.Works[i] = zotio.LookupWork{DOI: request.Identifiers.DOI, ArXiv: request.Identifiers.ArXiv}
		}
	}
	var ownership zotio.LookupWorksResult
	if err := caller.Call(ctx, "zotio.lookup_works", lookupRequest, &ownership); err != nil {
		return nil, err
	}
	if len(ownership.Works) != len(requests) {
		return nil, fmt.Errorf("Zotio ownership lookup returned %d results for %d works", len(ownership.Works), len(requests))
	}
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
	requests, _, err := ApplyOwnership(requests, ownership, options.Collection, options.IncludeOwned)
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
			params := any(request)
			if options.AutoImport != nil {
				params = submitParams{Request: request, AutoImport: options.AutoImport}
			}
			var submitted submitResult
			if err := caller.Call(ctx, "acquire.submit", params, &submitted); err != nil {
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
			if detail.Job == nil {
				results[index].State = "unknown"
				errs[index] = fmt.Errorf("daemon returned no job for %s", submitted.JobID)
				return
			}
			results[index].State = detail.Job.State
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
