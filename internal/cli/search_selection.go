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
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"papio/internal/batch"
	"papio/internal/discovery"
	"papio/internal/protocol"
	"papio/internal/store"
	"papio/internal/zotio"

	"github.com/spf13/cobra"
)

type searchSnapshot struct {
	Version int                    `json:"version"`
	Search  discovery.SearchParams `json:"search"`
	Rows    []searchSnapshotRow    `json:"rows"`
}
type searchSnapshotRow struct {
	Key string                   `json:"key"`
	Hit discovery.DiscoveredWork `json:"hit"`
}

func searchHitKey(hit discovery.DiscoveredWork) string {
	data, _ := json.Marshal(hit.Work)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:12])
}
func writeSearchSnapshot(path string, params discovery.SearchParams, works []discovery.DiscoveredWork) error {
	if len(works) > discovery.MaxLimit {
		return fmt.Errorf("search snapshot exceeds %d works", discovery.MaxLimit)
	}
	snapshot := searchSnapshot{Version: 1, Search: params, Rows: make([]searchSnapshotRow, 0, len(works))}
	seen := map[string]bool{}
	for _, hit := range works {
		key := searchHitKey(hit)
		if seen[key] {
			continue
		}
		seen[key] = true
		snapshot.Rows = append(snapshot.Rows, searchSnapshotRow{Key: key, Hit: hit})
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 2<<20 {
		return fmt.Errorf("search snapshot exceeds 2 MiB")
	}
	// Write a private temp file, sync it, then publish it with a hard link,
	// which fails if path exists (exclusive, like O_EXCL). A crash mid-write
	// leaves only the temp file, never a partial snapshot at path that would
	// block every retry. Filesystems without hard links (exFAT) fall back to an
	// exclusive create of the final name.
	payload := append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".papio-snapshot-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if snapshotBeforePublish != nil {
		snapshotBeforePublish()
	}
	err = os.Link(tmpName, path)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return err
	}
	return writeSnapshotExclusive(path, payload)
}

// snapshotBeforePublish lets tests simulate a crash between the durable temp
// write and publication.
var snapshotBeforePublish func()

// writeSnapshotExclusive is the no-hard-link fallback. O_EXCL created the
// file, so it is ours to remove on failure.
func writeSnapshotExclusive(path string, payload []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}
func readSearchSnapshot(path string) (searchSnapshot, error) {
	var snapshot searchSnapshot
	file, err := os.Open(path)
	if err != nil {
		return snapshot, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if err != nil {
		return snapshot, err
	}
	if len(data) > 2<<20 {
		return snapshot, fmt.Errorf("snapshot exceeds 2 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return snapshot, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return snapshot, fmt.Errorf("snapshot has trailing data")
	}
	if snapshot.Version != 1 || len(snapshot.Rows) > discovery.MaxLimit {
		return snapshot, fmt.Errorf("unsupported or oversized search snapshot")
	}
	seen := map[string]bool{}
	for _, row := range snapshot.Rows {
		if row.Key != searchHitKey(row.Hit) || seen[row.Key] {
			return snapshot, fmt.Errorf("snapshot key is invalid or repeated")
		}
		seen[row.Key] = true
	}
	return snapshot, nil
}

func searchRowProse(row searchSnapshotRow) string {
	w := row.Hit.Work
	return fmt.Sprintf("%s | %d | %s | %s | %s | %s", row.Key, w.Year, store.StripTerminalControls(firstAuthor(w.Authors)),
		store.StripTerminalControls(w.Title), emptyMarker(store.StripTerminalControls(w.DOI)), oaMarker(row.Hit.IsOA))
}

func searchSaveProse(path string, snapshot searchSnapshot, continuation string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Saved %d candidate(s) to %s; select keys with `papio search acquire %s --keys <key>[,<key>]`", len(snapshot.Rows), path, path)
	for _, row := range snapshot.Rows {
		out.WriteString("\n" + searchRowProse(row))
	}
	if continuation != "" {
		fmt.Fprintf(&out, "\nContinuation: %s", continuation)
	}
	return out.String()
}

func searchPreviewProse(rows []searchSnapshotRow, ownership []zotio.WorkOwnership, autoImport bool, costPolicy string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Previewed %d selected work(s); nothing submitted. Repeat with --confirm to submit them.", len(rows))
	for i, row := range rows {
		status := "unknown"
		if i < len(ownership) {
			status = ownership[i].Status
			if ownership[i].ItemKey != "" {
				status += " (" + store.StripTerminalControls(ownership[i].ItemKey) + ")"
			}
		}
		fmt.Fprintf(&out, "\n%s | ownership: %s", searchRowProse(row), status)
	}
	fmt.Fprintf(&out, "\nAuto-import: %t\nCost policy: %s", autoImport, costPolicy)
	return out.String()
}

func searchAcquireProse(count int, output *batch.SubmitOutput) string {
	if output == nil {
		return fmt.Sprintf("Selected %d work(s); no batch was submitted.", count)
	}
	return fmt.Sprintf("Submitted batch %s: %d submitted, %d already owned, %d already in the library, %d failed.",
		output.BatchID, len(output.Submitted), len(output.SkippedOwned), len(output.ExistingItem), output.Failed)
}

type optionCaller struct{ opt *options }

func (c optionCaller) Call(ctx context.Context, method string, params, result any) error {
	return c.opt.call(ctx, method, params, result)
}

func newSearchAcquireCommand(opt *options) *cobra.Command {
	var keys []string
	var confirm bool
	var collection, resolver, label string
	command := &cobra.Command{Use: "acquire <snapshot>", Short: "Preview selected search keys; submit only with --confirm", Args: cobra.ExactArgs(1), Annotations: map[string]string{"mcp:read-only": "false"}, RunE: func(cmd *cobra.Command, args []string) error {
		snapshot, err := readSearchSnapshot(args[0])
		if err != nil {
			return err
		}
		if len(keys) == 0 || len(keys) > 50 {
			return fmt.Errorf("--keys must select 1-50 exact snapshot keys")
		}
		selected := map[string]bool{}
		for _, key := range keys {
			if key == "" || selected[key] {
				return fmt.Errorf("selection keys must be nonempty and unique")
			}
			selected[key] = true
		}
		requests := make([]protocol.WorkRequest, 0, len(keys))
		selectedRows := []searchSnapshotRow{}
		for _, row := range snapshot.Rows {
			if !selected[row.Key] {
				continue
			}
			delete(selected, row.Key)
			w := row.Hit.Work
			data, _ := json.Marshal(map[string]any{"doi": w.DOI, "pmid": w.PMID, "arxiv": w.ArXiv, "isbn": w.ISBN, "openalex": w.OpenAlex, "title": w.Title, "authors": w.Authors, "year": w.Year})
			request, err := batch.ParseWork(data)
			if err != nil {
				return fmt.Errorf("selected key %s: %w", row.Key, err)
			}
			requests = append(requests, request)
			selectedRows = append(selectedRows, row)
		}
		if len(selected) != 0 {
			return fmt.Errorf("one or more selection keys are absent from the snapshot")
		}
		cfg, err := opt.loadConfig()
		if err != nil {
			return err
		}
		autoImport := false
		options := batch.SubmitOptions{AutoImport: &autoImport, Collection: collection, Resolver: resolver, Label: label, Holdings: strings.TrimSpace(cfg.Zotio.Executable) == "" && len(cfg.Library.Sources) > 0}
		if options.Holdings {
			options.LibraryFingerprint = cfg.LibraryFingerprint()
		}
		ownership, err := batch.Preview(cmd.Context(), optionCaller{opt}, requests, options)
		if err != nil {
			return err
		}
		preview := struct {
			Rows       []searchSnapshotRow   `json:"rows"`
			Ownership  []zotio.WorkOwnership `json:"ownership"`
			Count      int                   `json:"count"`
			Confirmed  bool                  `json:"confirmed"`
			AutoImport bool                  `json:"auto_import"`
			CostPolicy string                `json:"cost_policy"`
			Batch      *batch.SubmitOutput   `json:"batch,omitempty"`
		}{Rows: selectedRows, Ownership: ownership.Works, Count: len(requests), Confirmed: confirm, CostPolicy: "configured daemon budgets and default per-job cost policy; no override"}
		if confirm {
			output, submitErr := batch.Submit(cmd.Context(), optionCaller{opt}, cfg.DataDir, requests, options)
			preview.Batch = output
			if err := opt.printResult(preview, "%s\n%s", searchPreviewProse(preview.Rows, preview.Ownership, preview.AutoImport, preview.CostPolicy), searchAcquireProse(preview.Count, preview.Batch)); err != nil {
				return err
			}
			return submitErr
		}
		return opt.printResult(preview, "%s", searchPreviewProse(preview.Rows, preview.Ownership, preview.AutoImport, preview.CostPolicy))
	}}
	command.Flags().StringSliceVar(&keys, "keys", nil, "comma-separated exact keys from the search snapshot")
	command.Flags().BoolVar(&confirm, "confirm", false, "submit exactly the selected works after current ownership classification")
	command.Flags().StringVar(&collection, "collection", "", "destination collection")
	command.Flags().StringVar(&resolver, "resolver", "", "institution resolver profile")
	command.Flags().StringVar(&label, "label", "search selection", "batch label")
	return command
}

func newSearchSaveCommand(opt *options) *cobra.Command {
	var params discovery.SearchParams
	var newOnly bool
	command := &cobra.Command{Use: "save <snapshot> <query>", Short: "Save a bounded search result set for explicit selection", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			params.Query = strings.TrimSpace(args[1])
			if params.Query == "" {
				return &usageError{err: fmt.Errorf("query is required")}
			}
			// Fail before the (possibly paid) discovery call: writeSearchSnapshot
			// refuses an existing path with O_EXCL, but only after the search.
			if _, err := os.Lstat(args[0]); err == nil {
				return fmt.Errorf("snapshot %s already exists; choose a new path", args[0])
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			params.Limit = effectiveLimitFloored(params.Limit, discovery.MaxLimit, discovery.DefaultLimit)
			var works []discovery.DiscoveredWork
			var continuation string
			if newOnly {
				var result discovery.UnownedResult
				if err := opt.call(cmd.Context(), "discovery.search_unowned_v1", discovery.UnownedRequest{Search: params}, &result); err != nil {
					return err
				}
				works, continuation = result.Works, result.Continuation
			} else if err := opt.call(cmd.Context(), "discovery.search", params, &works); err != nil {
				return err
			}
			if err := writeSearchSnapshot(args[0], params, works); err != nil {
				return err
			}
			snapshot, err := readSearchSnapshot(args[0])
			if err != nil {
				return err
			}
			return opt.printResult(struct {
				Snapshot     searchSnapshot `json:"snapshot"`
				Continuation string         `json:"continuation,omitempty"`
			}{snapshot, continuation}, "%s", searchSaveProse(args[0], snapshot, continuation))
		},
	}
	command.Flags().IntVar(&params.Limit, "limit", 20, "maximum selected search candidates (1-50)")
	command.Flags().StringVar(&params.Source, "source", "", "configured discovery backend")
	command.Flags().BoolVar(&params.OAOnly, "oa-only", false, "open-access results only")
	command.Flags().IntVar(&params.YearFrom, "year-from", 0, "minimum publication year")
	command.Flags().IntVar(&params.YearTo, "year-to", 0, "maximum publication year")
	command.Flags().BoolVar(&newOnly, "new-only", false, "fill the bounded snapshot with unowned results")
	return command
}
