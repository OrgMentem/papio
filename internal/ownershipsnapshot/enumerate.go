// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package ownershipsnapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"papio/internal/bibparse"
	"papio/internal/config"
)

// LibraryRecord is one DOI-bearing record from a configured library source.
type LibraryRecord struct {
	DOI   string
	Title string
}

// EnumerateLibraryRecords reads one configured file through the bounded
// bibliographic parser used by the ownership snapshot provider.
func EnumerateLibraryRecords(ctx context.Context, source config.LibrarySource) ([]LibraryRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(source.Name)
	if name == "" {
		return nil, fmt.Errorf("library source name is required")
	}
	if source.Kind != config.LibraryKindFile {
		return nil, fmt.Errorf("library source %q: kind %q is not supported yet (only %q)", name, source.Kind, config.LibraryKindFile)
	}
	path := expandHome(strings.TrimSpace(source.Path))
	if path == "" {
		return nil, fmt.Errorf("library source %q: path is required", name)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("library source %q: %w", name, err)
	}
	defer func() { _ = file.Close() }()
	data, err := readBounded(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("library source %q: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	format := bibparse.Format(source.Format)
	if format == "" {
		format = bibparse.Detect(path, data)
	}
	records, err := bibparse.ParseRecords(format, data)
	if err != nil && !errors.Is(err, bibparse.ErrNoEntries) {
		return nil, fmt.Errorf("library source %q: %w", name, err)
	}
	out := make([]LibraryRecord, 0, len(records))
	for _, record := range records {
		if strings.TrimSpace(record.DOI) != "" {
			out = append(out, LibraryRecord{DOI: record.DOI, Title: record.Title})
		}
	}
	return out, nil
}
