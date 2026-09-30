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

// EnumerateLibraryRecords reads one configured source — a file, or one run of
// a command — through the bounded bibliographic parser used by the ownership
// snapshot provider. A command that fails, times out, or overflows its output
// cap is an error, never an empty library.
func EnumerateLibraryRecords(ctx context.Context, source config.LibrarySource) ([]LibraryRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(source.Name)
	if name == "" {
		return nil, fmt.Errorf("library source name is required")
	}
	var data []byte
	pathHint := ""
	switch source.Kind {
	case config.LibraryKindFile:
		path := expandHome(strings.TrimSpace(source.Path))
		if path == "" {
			return nil, fmt.Errorf("library source %q: path is required", name)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("library source %q: %w", name, err)
		}
		defer func() { _ = file.Close() }()
		data, err = readBounded(ctx, file)
		if err != nil {
			return nil, fmt.Errorf("library source %q: %w", name, err)
		}
		pathHint = path
	case config.LibraryKindCommand:
		argv, limits, err := commandSettings(source)
		if err != nil {
			return nil, fmt.Errorf("library source %q: %w", name, err)
		}
		out, failure := runCommand(ctx, argv, limits.Timeout, limits.MaxOutputBytes)
		if failure != "" {
			// The failure code, never the command's output: it inherits the
			// daemon environment and may print credentials.
			return nil, fmt.Errorf("library source %q: command failed (%s)", name, failure)
		}
		data = out
	default:
		return nil, fmt.Errorf("library source %q: kind %q is not supported (only %q or %q)", name, source.Kind, config.LibraryKindFile, config.LibraryKindCommand)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	format := bibparse.Format(source.Format)
	if format == "" {
		format = bibparse.Detect(pathHint, data)
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
