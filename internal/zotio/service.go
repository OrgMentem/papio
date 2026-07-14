// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package zotio

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"papio/internal/bundle"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/work"
)

// CLI is the credential-owning Zotio process boundary used by the service.
type CLI interface {
	Preflight(context.Context) (*PreflightResult, error)
	MissingPDF(context.Context, string, int) ([]MissingPDFItem, error)
	GetItem(context.Context, string) (*Item, error)
	Sync(context.Context) error
	RunJSON(context.Context, ...string) (json.RawMessage, error)
}

// Submitter is Papio's command-independent acquisition application service.
type Submitter interface {
	Submit(context.Context, protocol.WorkRequest) (string, error)
}

// Service coordinates Zotio observations with Papio acquisition requests.
type Service struct {
	CLI            CLI
	Submitter      Submitter
	Bundle         *bundle.Exporter
	Store          *store.Store
	DataDir        string
	AttachmentMode string
	Now            func() time.Time
}

// QueueOptions configures one bounded import of Zotio's missing-PDF queue.
type QueueOptions struct {
	Collection         string   `json:"collection,omitempty"`
	Limit              int      `json:"limit"`
	DesiredVersion     string   `json:"desired_version,omitempty"`
	AccessModeOverride string   `json:"access_mode_override,omitempty"`
	MaxCostUSD         *float64 `json:"max_cost_usd,omitempty"`
	SourcesAllow       []string `json:"sources_allow,omitempty"`
	SourcesDeny        []string `json:"sources_deny,omitempty"`
}

// QueuedJob links one Zotero parent item to its idempotent Papio request/job.
type QueuedJob struct {
	ZotioItemKey string `json:"zotio_item_key"`
	RequestID    string `json:"request_id"`
	JobID        string `json:"job_id"`
	Title        string `json:"title,omitempty"`
}

// SkippedItem records a queue row that lacks enough bibliographic identity.
type SkippedItem struct {
	ZotioItemKey string `json:"zotio_item_key"`
	Title        string `json:"title,omitempty"`
	Reason       string `json:"reason"`
}

// QueueResult is stable machine output for `acquire --from-zotio`.
type QueueResult struct {
	Preflight *PreflightResult `json:"preflight"`
	Queued    []QueuedJob      `json:"queued"`
	Skipped   []SkippedItem    `json:"skipped"`
}

// QueueMissingPDF preflights Zotio, reads one bounded queue slice, and submits
// deterministic Papio requests. Re-running it returns existing live jobs.
func (s *Service) QueueMissingPDF(ctx context.Context, options QueueOptions) (*QueueResult, error) {
	if s == nil || s.CLI == nil || s.Submitter == nil {
		return nil, fmt.Errorf("Zotio integration is not configured")
	}
	if options.Limit == 0 {
		options.Limit = 25
	}
	preflight, err := s.CLI.Preflight(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.CLI.MissingPDF(ctx, strings.TrimSpace(options.Collection), options.Limit)
	if err != nil {
		return nil, err
	}
	result := &QueueResult{
		Preflight: preflight,
		Queued:    make([]QueuedJob, 0, len(items)),
		Skipped:   make([]SkippedItem, 0),
	}
	for _, row := range items {
		request, reason := s.requestForQueueItem(ctx, row, options)
		if reason != "" {
			result.Skipped = append(result.Skipped, SkippedItem{
				ZotioItemKey: row.Key,
				Title:        row.Title,
				Reason:       reason,
			})
			continue
		}
		jobID, err := s.Submitter.Submit(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("submitting Zotio item %s: %w", row.Key, err)
		}
		result.Queued = append(result.Queued, QueuedJob{
			ZotioItemKey: row.Key,
			RequestID:    request.RequestID,
			JobID:        jobID,
			Title:        request.Title,
		})
	}
	return result, nil
}

func (s *Service) requestForQueueItem(ctx context.Context, row MissingPDFItem, options QueueOptions) (protocol.WorkRequest, string) {
	request := protocol.WorkRequest{
		SchemaVersion:      protocol.WorkRequestSchemaVersion,
		RequestID:          "request_zotio_" + row.Key,
		Title:              strings.TrimSpace(row.Title),
		ZotioItemKey:       row.Key,
		Collection:         strings.TrimSpace(options.Collection),
		DesiredVersion:     defaultVersion(options.DesiredVersion),
		AccessModeOverride: strings.TrimSpace(options.AccessModeOverride),
		MaxCostUSD:         options.MaxCostUSD,
		SourcesAllow:       trimValues(options.SourcesAllow),
		SourcesDeny:        trimValues(options.SourcesDeny),
	}
	if row.DOI != "" {
		doi, err := work.NormalizeDOI(row.DOI)
		if err != nil {
			return protocol.WorkRequest{}, "invalid DOI from Zotio: " + err.Error()
		}
		request.Identifiers = &protocol.Identifiers{DOI: doi}
	} else {
		item, err := s.CLI.GetItem(ctx, row.Key)
		if err != nil {
			return protocol.WorkRequest{}, err.Error()
		}
		request.Title = item.Title
		request.Authors = append([]string(nil), item.Authors...)
		request.Year = item.Year
		if item.DOI != "" {
			doi, err := work.NormalizeDOI(item.DOI)
			if err != nil {
				return protocol.WorkRequest{}, "invalid DOI from Zotio item: " + err.Error()
			}
			request.Identifiers = &protocol.Identifiers{DOI: doi}
		}
	}
	if err := request.Validate(); err != nil {
		return protocol.WorkRequest{}, "insufficient Zotio identity: " + err.Error()
	}
	return request, ""
}

func defaultVersion(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "any"
}

func trimValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
