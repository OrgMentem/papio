// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package ownershipsnapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

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
	records, err := EnumerateBibliographicRecords(ctx, source)
	if err != nil {
		return nil, err
	}
	out := make([]LibraryRecord, 0, len(records))
	for _, record := range records {
		if strings.TrimSpace(record.DOI) != "" {
			out = append(out, LibraryRecord{DOI: record.DOI, Title: record.Title})
		}
	}
	return out, nil
}

// enumerateOpen and enumerateRead are seams for the stall tests: production
// opens with the nonblocking source opener, tests substitute a reader that
// blocks forever like a syscall stuck in the kernel.
var (
	enumerateOpen = openSource
	enumerateRead = readBounded
)

// enumFlights tracks the paths with one in-flight library file read. When its
// worker is abandoned — the caller left at its context or at the refresh
// bound while the read stayed blocked — the entry remains until the worker
// finally returns, and every later enumeration for the same path fails fast
// instead of joining the blocked read.
var (
	enumMu      sync.Mutex
	enumFlights = make(map[string]chan struct{})
)

// enumTryBegin claims the single read slot for path. It reports false when
// another read for the same path is still blocked, in which case the caller
// must fail fast without waiting.
func enumTryBegin(path string) bool {
	enumMu.Lock()
	defer enumMu.Unlock()
	if _, blocked := enumFlights[path]; blocked {
		return false
	}
	enumFlights[path] = make(chan struct{})
	return true
}

// enumEnd releases the read slot for path. Only the worker calls it, exactly
// once, when its read finally returns.
func enumEnd(path string) {
	enumMu.Lock()
	defer enumMu.Unlock()
	close(enumFlights[path])
	delete(enumFlights, path)
}

type enumResult struct {
	data []byte
	err  error
}

// EnumerateBibliographicRecords returns the complete parsed feed, including
// records without identifiers. It never infers PDF presence from file fields.
func EnumerateBibliographicRecords(ctx context.Context, source config.LibrarySource) ([]bibparse.Record, error) {
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
		var err error
		data, err = enumerateFile(ctx, name, path)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > defaultMaxBytes {
			// A prefix of an export is not a smaller library: parsing it would
			// silently drop every record past the cap.
			return nil, fmt.Errorf("library source %q: %s exceeds %d bytes", name, path, defaultMaxBytes)
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
	if strings.TrimSpace(string(source.Format)) == "" {
		// Holdings can never be JSONL, so the batch default must not apply:
		// blank input is an empty library and anything else undetectable
		// names the fix instead of failing as jsonl.
		if isBlankExport(data) {
			return nil, nil
		}
		format := bibparse.Detect(pathHint, stripLeadingBibTeXComments(data))
		if format == bibparse.FormatJSONL {
			return nil, fmt.Errorf("library source %q: cannot detect format, set library.sources[].format", name)
		}
		records, err := bibparse.ParseRecords(format, data)
		if err != nil && !errors.Is(err, bibparse.ErrNoEntries) {
			return nil, fmt.Errorf("library source %q: %w", name, err)
		}
		return records, nil
	}
	format := bibparse.Format(source.Format)
	records, err := bibparse.ParseRecords(format, data)
	if err != nil && !errors.Is(err, bibparse.ErrNoEntries) {
		return nil, fmt.Errorf("library source %q: %w", name, err)
	}
	return records, nil
}

// enumerateFile reads one library export file without ever blocking its
// caller in the kernel: the open and the read run on a worker, and the caller
// waits at most for its context or the refresh bound. A read abandoned while
// still blocked latches the path until the worker returns, so a stalled file
// (TCC consent, a dead network mount) fails fast instead of wedging every
// maintenance runner behind the shared goroutine.
func enumerateFile(ctx context.Context, name, path string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !enumTryBegin(path) {
		return nil, fmt.Errorf("library source %q: %w (a previous read is still blocked)", name, context.DeadlineExceeded)
	}
	// Capture the seams on the caller: the worker must never read the package
	// vars after launch, or a later reassignment races its in-flight read.
	open := enumerateOpen
	read := enumerateRead
	results := make(chan enumResult, 1)
	go func() {
		data, err := readEnumeratedFile(ctx, path, open, read)
		enumEnd(path)
		results <- enumResult{data: data, err: err}
	}()
	bound := time.NewTimer(refreshWaitBound + refreshStallGrace)
	defer bound.Stop()
	select {
	case result := <-results:
		if result.err != nil {
			return nil, fmt.Errorf("library source %q: %w", name, result.err)
		}
		return result.data, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("library source %q: %w", name, ctx.Err())
	case <-bound.C:
		return nil, fmt.Errorf("library source %q: %w (timed out reading the export)", name, context.DeadlineExceeded)
	}
}

// readEnumeratedFile opens one export and reads it whole, rejecting torn
// reads: the bytes must match the size of the descriptor they came from, and
// the descriptor must be stable across the read. A half-rewritten export
// parses cleanly with fewer records, so size and stability are the only
// defence before parsing. The opener and reader arrive as values captured at
// launch so the worker never touches the reassignable seams mid-read.
func readEnumeratedFile(ctx context.Context, path string, open func(string) (*os.File, error), read func(context.Context, io.Reader) ([]byte, error)) ([]byte, error) {
	file, err := open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("source is not a regular file")
	}
	beforeSize, beforeMod := info.Size(), info.ModTime()
	if beforeSize > defaultMaxBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", defaultMaxBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := read(ctx, io.LimitReader(file, defaultMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > defaultMaxBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", defaultMaxBytes)
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() {
		return nil, fmt.Errorf("source is not a regular file")
	}
	if int64(len(data)) != after.Size() || after.Size() != beforeSize || !after.ModTime().Equal(beforeMod) {
		return nil, fmt.Errorf("source changed during read")
	}
	return data, nil
}
