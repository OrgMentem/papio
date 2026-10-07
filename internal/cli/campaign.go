// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"papio/internal/api"
	"papio/internal/batch"
	"papio/internal/bibparse"
	"papio/internal/config"
	"papio/internal/job"
	"papio/internal/protocol"
	"papio/internal/store"

	"github.com/spf13/cobra"
)

const campaignCeiling = 200
const campaignWindow = 50

var campaignIDPattern = regexp.MustCompile(`^campaign_[a-zA-Z0-9_-]{1,80}$`)

type campaignRecord struct {
	Ordinal   int             `json:"ordinal"`
	SourceKey string          `json:"source_key,omitempty"`
	Citation  bibparse.Record `json:"citation"`
	WorkIndex int             `json:"work_index"`
	Error     string          `json:"error,omitempty"`
}
type campaignChunk struct {
	Start   int    `json:"start"`
	End     int    `json:"end"`
	BatchID string `json:"batch_id"`
	State   string `json:"state"`
}
type campaignManifest struct {
	Version            int                    `json:"version"`
	ID                 string                 `json:"id"`
	CreatedAt          string                 `json:"created_at"`
	SourceDigest       string                 `json:"source_digest"`
	Format             bibparse.Format        `json:"format"`
	Collection         string                 `json:"collection,omitempty"`
	Resolver           string                 `json:"resolver,omitempty"`
	LibraryFingerprint string                 `json:"library_fingerprint"`
	ZotioExecutable    string                 `json:"zotio_executable"`
	Paused             bool                   `json:"paused"`
	Records            []campaignRecord       `json:"records"`
	Works              []protocol.WorkRequest `json:"works"`
	Chunks             []campaignChunk        `json:"chunks"`
}

func newCampaignCommand(opt *options) *cobra.Command {
	command := &cobra.Command{Use: "campaign", Short: "Stage a selected citation export in bounded acquisition chunks"}
	var collection, resolver string
	create := &cobra.Command{Use: "import <file>", Short: "Preview and persist up to 200 source records without submitting jobs", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		file, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
		if err != nil {
			return err
		}
		if len(data) > 8<<20 {
			return errors.New("campaign input exceeds 8 MiB")
		}
		format := bibparse.Detect(args[0], data)
		records, err := bibparse.ParseRecords(format, data)
		if err != nil {
			return err
		}
		if len(records) == 0 || len(records) > campaignCeiling {
			return fmt.Errorf("campaign requires 1-%d records", campaignCeiling)
		}
		cfg, err := opt.loadConfig()
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		manifest := campaignManifest{Version: 1, ID: job.NewID("campaign"), CreatedAt: store.FormatTime(time.Now()), SourceDigest: hex.EncodeToString(sum[:]), Format: format, Collection: collection, Resolver: resolver, LibraryFingerprint: cfg.LibraryFingerprint(), ZotioExecutable: cfg.Zotio.Executable, Records: []campaignRecord{}, Works: []protocol.WorkRequest{}, Chunks: []campaignChunk{}}
		indices := map[string]int{}
		for i, record := range records {
			source := campaignRecord{Ordinal: i + 1, SourceKey: record.SourceKey, Citation: record, WorkIndex: -1}
			if len(source.SourceKey) > 1024 {
				return fmt.Errorf("source key at record %d exceeds 1024 bytes", i+1)
			}
			encoded, _ := json.Marshal(map[string]any{"doi": record.DOI, "pmid": record.PMID, "arxiv": record.ArXiv, "isbn": record.ISBN, "title": record.Title, "authors": record.Authors, "year": record.Year})
			request, err := batch.ParseWork(encoded)
			if err != nil {
				source.Error = err.Error()
			} else {
				index, exists := indices[request.RequestID]
				if !exists {
					index = len(manifest.Works)
					indices[request.RequestID] = index
					manifest.Works = append(manifest.Works, request)
				}
				source.WorkIndex = index
			}
			manifest.Records = append(manifest.Records, source)
		}
		created, _ := time.Parse(time.RFC3339Nano, manifest.CreatedAt)
		for start := 0; start < len(manifest.Works); start += 50 {
			end := min(start+50, len(manifest.Works))
			manifest.Chunks = append(manifest.Chunks, campaignChunk{Start: start, End: end, BatchID: batch.ScopedID(manifest.Works[start:end], created, manifest.ID), State: "pending"})
		}
		preview, err := campaignPreview(cmd.Context(), opt, cfg, &manifest)
		if err != nil {
			return err
		}
		if err := writeCampaign(cfg.DataDir, &manifest); err != nil {
			return err
		}
		return opt.printJSON(preview)
	}}
	create.Flags().StringVar(&collection, "collection", "", "destination collection; automatic Zotero import stays off")
	create.Flags().StringVar(&resolver, "resolver", "", "institution resolver profile")
	command.AddCommand(create)
	for _, verb := range []string{"preview", "run", "pause", "resume", "report"} {
		verb := verb
		annotations := map[string]string{}
		if verb == "preview" || verb == "report" {
			annotations["mcp:read-only"] = "true"
		}
		sub := &cobra.Command{Use: verb + " <id>", Short: map[string]string{"preview": "Classify all campaign works without submitting", "run": "Submit at most one chunk within the 50-active-work window", "pause": "Prevent further staged submission", "resume": "Allow staged submission without submitting yet", "report": "Return one outcome for every original source record"}[verb], Args: cobra.ExactArgs(1), Annotations: annotations, RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := opt.loadConfig()
			if err != nil {
				return err
			}
			path, err := campaignPath(cfg.DataDir, args[0])
			if err != nil {
				return err
			}
			// Reuse the platform advisory lock used by other durable CLI journals.
			unlock, err := lockPendingCredentials(path + ".lock")
			if err != nil {
				return err
			}
			defer unlock()
			manifest, err := loadCampaign(path, args[0])
			if err != nil {
				return err
			}
			switch verb {
			case "pause", "resume":
				manifest.Paused = verb == "pause"
				if err := writeCampaign(cfg.DataDir, manifest); err != nil {
					return err
				}
				return opt.printJSON(manifest)
			case "preview":
				preview, err := campaignPreview(cmd.Context(), opt, cfg, manifest)
				if err != nil {
					return err
				}
				return opt.printJSON(preview)
			case "report":
				report, err := campaignReport(cmd.Context(), opt, cfg, manifest)
				if err != nil {
					return err
				}
				return opt.printJSON(report)
			case "run":
				if manifest.Paused {
					return errors.New("campaign is paused; resume it before running")
				}
				if err := campaignPolicyMatches(cfg, manifest); err != nil {
					return err
				}
				// Account for prior and partially acknowledged chunks before advancing.
				active, err := campaignActive(cmd.Context(), opt, cfg, manifest)
				if err != nil {
					return err
				}
				for i := range manifest.Chunks {
					chunk := &manifest.Chunks[i]
					if chunk.State == "acknowledged" {
						continue
					}
					if chunk.State == "pending" && active+chunk.End-chunk.Start > campaignWindow {
						return fmt.Errorf("campaign has %d active works; next chunk would exceed window %d", active, campaignWindow)
					}
					chunk.State = "inflight"
					if err := writeCampaign(cfg.DataDir, manifest); err != nil {
						return err
					}
					options := campaignOptions(cfg, manifest)
					requests := append([]protocol.WorkRequest(nil), manifest.Works[chunk.Start:chunk.End]...)
					output, submitErr := batch.Submit(cmd.Context(), optionCaller{opt}, cfg.DataDir, requests, options)
					if submitErr == nil {
						chunk.State = "acknowledged"
					}
					if err := writeCampaign(cfg.DataDir, manifest); err != nil {
						return err
					}
					result := struct {
						CampaignID string              `json:"campaign_id"`
						Chunk      int                 `json:"chunk"`
						Window     int                 `json:"active_window"`
						Result     *batch.SubmitOutput `json:"result"`
					}{manifest.ID, i, campaignWindow, output}
					if err := opt.printJSON(result); err != nil {
						return err
					}
					return submitErr
				}
				return opt.printJSON(map[string]any{"campaign_id": manifest.ID, "complete": true, "detail": "all selected chunks acknowledged; use report for acquisition outcomes"})
			}
			return nil
		}}
		command.AddCommand(sub)
	}
	return command
}

func campaignPolicyMatches(cfg config.Config, manifest *campaignManifest) error {
	if cfg.LibraryFingerprint() != manifest.LibraryFingerprint || cfg.Zotio.Executable != manifest.ZotioExecutable {
		return errors.New("campaign ownership configuration changed; restore the original source configuration before submission")
	}
	return nil
}
func campaignOptions(cfg config.Config, manifest *campaignManifest) batch.SubmitOptions {
	autoImport := false
	created, _ := time.Parse(time.RFC3339Nano, manifest.CreatedAt)
	return batch.SubmitOptions{IdentityScope: manifest.ID, AutoImport: &autoImport, Collection: manifest.Collection, Resolver: manifest.Resolver, Label: manifest.ID, Holdings: strings.TrimSpace(cfg.Zotio.Executable) == "" && len(cfg.Library.Sources) > 0, LibraryFingerprint: manifest.LibraryFingerprint, Now: created}
}
func campaignPreview(ctx context.Context, opt *options, cfg config.Config, manifest *campaignManifest) (any, error) {
	if err := campaignPolicyMatches(cfg, manifest); err != nil {
		return nil, err
	}
	classifications := make([]string, len(manifest.Works))
	for _, chunk := range manifest.Chunks {
		result, err := batch.Preview(ctx, optionCaller{opt}, manifest.Works[chunk.Start:chunk.End], campaignOptions(cfg, manifest))
		if err != nil {
			return nil, err
		}
		for i, item := range result.Works {
			classifications[chunk.Start+i] = item.Status
		}
	}
	return struct {
		Manifest   *campaignManifest `json:"manifest"`
		Ownership  []string          `json:"ownership"`
		Ceiling    int               `json:"record_ceiling"`
		Window     int               `json:"active_window"`
		AutoImport bool              `json:"auto_import"`
		CostPolicy string            `json:"cost_policy"`
	}{manifest, classifications, campaignCeiling, campaignWindow, false, "configured daemon budgets and default per-job cost policy; no override"}, nil
}
func campaignPath(dataDir, id string) (string, error) {
	if !campaignIDPattern.MatchString(id) {
		return "", errors.New("invalid campaign id")
	}
	return filepath.Join(dataDir, "campaigns", id+".json"), nil
}
func writeCampaign(dataDir string, manifest *campaignManifest) error {
	path, err := campaignPath(dataDir, manifest.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "campaign-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
func loadCampaign(path, id string) (*campaignManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 16<<20 {
		return nil, errors.New("campaign manifest exceeds 16 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest campaignManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("campaign has trailing data")
	}
	if manifest.Version != 1 || manifest.ID != id || len(manifest.Records) < 1 || len(manifest.Records) > campaignCeiling || len(manifest.Works) > campaignCeiling {
		return nil, errors.New("invalid campaign manifest")
	}
	created, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt)
	if err != nil {
		return nil, err
	}
	if len(manifest.Chunks) != (len(manifest.Works)+49)/50 {
		return nil, errors.New("invalid campaign chunks")
	}
	for i, chunk := range manifest.Chunks {
		if chunk.Start != i*50 || chunk.End != min((i+1)*50, len(manifest.Works)) || chunk.BatchID != batch.ScopedID(manifest.Works[chunk.Start:chunk.End], created, manifest.ID) {
			return nil, errors.New("campaign chunk identity changed")
		}
		if chunk.State != "pending" && chunk.State != "inflight" && chunk.State != "acknowledged" {
			return nil, errors.New("invalid campaign chunk state")
		}
	}
	for i, row := range manifest.Records {
		if row.Ordinal != i+1 || row.WorkIndex < -1 || row.WorkIndex >= len(manifest.Works) || (row.WorkIndex == -1 && row.Error == "") {
			return nil, errors.New("invalid campaign source mapping")
		}
	}
	for _, request := range manifest.Works {
		if err := request.Validate(); err != nil {
			return nil, err
		}
	}
	return &manifest, nil
}
func campaignActive(ctx context.Context, opt *options, cfg config.Config, manifest *campaignManifest) (int, error) {
	active := 0
	seen := map[string]bool{}
	for _, chunk := range manifest.Chunks {
		if chunk.State == "pending" {
			continue
		}
		stored, err := batch.Load(cfg.DataDir, chunk.BatchID)
		if errors.Is(err, batch.ErrManifestNotFound) && chunk.State == "inflight" {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, item := range stored.Works {
			if item.JobID == "" || seen[item.JobID] {
				continue
			}
			seen[item.JobID] = true
			var detail api.JobDetail
			if err := opt.call(ctx, "jobs.get", map[string]any{"job_id": item.JobID}, &detail); err != nil {
				return 0, err
			}
			if detail.Job == nil {
				return 0, errors.New("campaign job is missing; refusing to advance")
			}
			if !job.Terminal(detail.Job.State) {
				active++
			}
		}
	}
	return active, nil
}

type campaignOutcome struct {
	campaignRecord
	SourceDigest   string          `json:"source_digest"`
	Format         bibparse.Format `json:"format"`
	Outcome        string          `json:"outcome"`
	JobID          string          `json:"job_id,omitempty"`
	ArtifactSHA256 string          `json:"artifact_sha256,omitempty"`
	Reason         string          `json:"reason,omitempty"`
}

func campaignReport(ctx context.Context, opt *options, cfg config.Config, manifest *campaignManifest) (any, error) {
	byIndex := make(map[int]campaignOutcome, len(manifest.Works))
	for _, chunk := range manifest.Chunks {
		if chunk.State == "pending" {
			continue
		}
		stored, err := batch.Load(cfg.DataDir, chunk.BatchID)
		if errors.Is(err, batch.ErrManifestNotFound) && chunk.State == "inflight" {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(stored.Works) != chunk.End-chunk.Start {
			return nil, errors.New("batch source mapping is incomplete")
		}
		for i, item := range stored.Works {
			outcome := campaignOutcome{Outcome: item.Status, JobID: item.JobID, Reason: item.Error}
			if item.JobID != "" {
				var detail api.JobDetail
				if err := opt.call(ctx, "jobs.get", map[string]any{"job_id": item.JobID}, &detail); err != nil {
					return nil, err
				}
				if detail.Job == nil {
					return nil, errors.New("campaign job is missing")
				}
				outcome.Outcome = detail.Job.State
				outcome.ArtifactSHA256 = detail.Job.ArtifactSHA256
				outcome.Reason = detail.Job.TerminalReason
				var disposition job.DispositionResult
				if err := opt.call(ctx, "jobs.disposition", map[string]any{"job_id": item.JobID}, &disposition); err != nil {
					return nil, err
				}
				if disposition.Disposition == job.DispositionArchived {
					outcome.Outcome = "archived"
				}
			}
			byIndex[chunk.Start+i] = outcome
		}
	}
	rows := make([]campaignOutcome, 0, len(manifest.Records))
	counts := map[string]int{}
	for _, record := range manifest.Records {
		outcome, ok := byIndex[record.WorkIndex]
		if !ok {
			outcome.Outcome = "not_submitted"
		}
		if record.WorkIndex < 0 {
			outcome.Outcome = "invalid"
			outcome.Reason = record.Error
		}
		outcome.campaignRecord = record
		outcome.SourceDigest = manifest.SourceDigest
		outcome.Format = manifest.Format
		rows = append(rows, outcome)
		counts[outcome.Outcome]++
	}
	return struct {
		CampaignID string            `json:"campaign_id"`
		Paused     bool              `json:"paused"`
		Records    []campaignOutcome `json:"records"`
		Summary    map[string]int    `json:"summary"`
		Truncated  bool              `json:"truncated"`
	}{manifest.ID, manifest.Paused, rows, counts, false}, nil
}
