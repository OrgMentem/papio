// Copyright 2026 OrgMentem. Licensed under MIT.

package retraction

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"papio/internal/bibparse"
	"papio/internal/config"
	"papio/internal/work"
	"papio/internal/zotio"
)

const (
	maxLibrarySourceBytes int64 = 32 << 20
	zotioPageSize               = 100
	maxZotioPages               = 1000
)

// LibraryCatalog enumerates DOI-bearing records from configured bibliographic
// exports and the personal Zotero library. It never changes either source.
type LibraryCatalog struct {
	sources []config.LibrarySource
	zotio   *zotio.Client

	mu     sync.RWMutex
	titles map[string]string
}

// NewLibraryCatalog constructs the production library DOI dependency.
func NewLibraryCatalog(sources []config.LibrarySource, client *zotio.Client) *LibraryCatalog {
	return &LibraryCatalog{sources: append([]config.LibrarySource(nil), sources...), zotio: client}
}

// LibraryDOIs returns normalized acquisition DOIs from every configured source.
func (c *LibraryCatalog) LibraryDOIs(ctx context.Context) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	byDOI := make(map[string]string)
	for _, source := range c.sources {
		if err := enumerateFileSource(ctx, source, byDOI); err != nil {
			return nil, err
		}
	}
	if c.zotio != nil {
		if err := c.enumerateZotio(ctx, byDOI); err != nil {
			return nil, err
		}
	}
	dois := make([]string, 0, len(byDOI))
	for doi := range byDOI {
		dois = append(dois, doi)
	}
	sort.Strings(dois)
	c.mu.Lock()
	c.titles = byDOI
	c.mu.Unlock()
	return dois, nil
}

// LibraryDOITitle returns the title recorded by the latest enumeration.
func (c *LibraryCatalog) LibraryDOITitle(doi string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.titles[doi]
}

func enumerateFileSource(ctx context.Context, source config.LibrarySource, byDOI map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(source.Path)
	if err != nil {
		return fmt.Errorf("library source %q: %w", source.Name, err)
	}
	defer file.Close()
	data, err := readBounded(file, maxLibrarySourceBytes)
	if err != nil {
		return fmt.Errorf("library source %q: %w", source.Name, err)
	}
	format := bibparse.Format(source.Format)
	if format == "" {
		format = bibparse.Detect(source.Path, data)
	}
	records, err := bibparse.ParseRecords(format, data)
	if err != nil {
		return fmt.Errorf("library source %q: %w", source.Name, err)
	}
	for _, record := range records {
		addLibraryRecord(byDOI, record.DOI, record.Title)
	}
	return nil
}

func (c *LibraryCatalog) enumerateZotio(ctx context.Context, byDOI map[string]string) error {
	for page := range maxZotioPages {
		start := page * zotioPageSize
		raw, err := c.zotio.RunJSON(ctx, "--agent", "items", "list", "--limit", strconv.Itoa(zotioPageSize), "--start", strconv.Itoa(start))
		if err != nil {
			return fmt.Errorf("list Zotero library page %d: %w", page+1, err)
		}
		items, err := decodeZotioPage(raw)
		if err != nil {
			return fmt.Errorf("decode Zotero library page %d: %w", page+1, err)
		}
		for _, item := range items {
			addLibraryRecord(byDOI, item.Data.DOI, item.Data.Title)
		}
		if len(items) < zotioPageSize {
			return nil
		}
	}
	log.Printf("papio: Zotero library enumeration reached the %d-page safety bound; retraction scope is truncated", maxZotioPages)
	return nil
}

type zotioListItem struct {
	Data struct {
		DOI   string `json:"DOI"`
		Title string `json:"title"`
	} `json:"data"`
}

func decodeZotioPage(raw json.RawMessage) ([]zotioListItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var items []zotioListItem
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, err
		}
		return items, nil
	}
	var envelope struct {
		Results []zotioListItem `json:"results"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, err
	}
	if envelope.Results == nil {
		return nil, fmt.Errorf("missing results array")
	}
	return envelope.Results, nil
}

func addLibraryRecord(byDOI map[string]string, rawDOI, rawTitle string) {
	doi, err := work.NormalizeDOI(rawDOI)
	if err != nil {
		return
	}
	title := strings.TrimSpace(rawTitle)
	if current, exists := byDOI[doi]; !exists || (current == "" && title != "") {
		byDOI[doi] = title
	}
}
