// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package livecohort

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Journal durably remembers what a run submitted, so a LATER run can tell
// a work it already asked the daemon for from a work it has not.
//
// The daemon commits the acquisition before it answers the submit RPC, so a
// lost response leaves a real job whose id this process never learned. The
// daemon's own deduplication cannot repair that across runs: it reuses a
// LIVE job for a request id and mints a fresh one once that job is terminal
// (internal/job/job.go createRequest). A rerun with a new run id therefore
// asks for the same paper a second time. The journal is the run's own
// durable association from cohort work to request id, and it is what makes
// the second run able to find the first run's job instead of repeating it.
type Journal interface {
	// Note records or updates one submission intent by its request id. It
	// is written BEFORE the submit RPC, because an intent recorded after a
	// lost response is an intent that was never recorded.
	Note(entry JournalEntry) error
	// Resolve marks one request id as accounted for: its job was measured
	// and reported, so a later run may measure the work again.
	Resolve(requestID string) error
	// Unresolved lists this cohort's submissions that no run has accounted
	// for yet.
	Unresolved(cohortID string) ([]JournalEntry, error)
}

// JournalEntry is one durable submission association.
type JournalEntry struct {
	CohortID  string `json:"cohort_id"`
	WorkKey   string `json:"work_key"`
	RequestID string `json:"request_id"`
	// JobID is empty while the daemon's answer is unknown, which is the
	// state this journal exists for.
	JobID    string `json:"job_id,omitempty"`
	RunID    string `json:"run_id"`
	At       string `json:"at"`
	Resolved bool   `json:"resolved,omitempty"`
}

// FileJournal stores the associations as one JSON document. A cohort is tens
// of works and a run writes a handful of entries per work, so a whole-file
// rewrite per write costs nothing and keeps the file readable by an operator
// who has to clean up by hand.
type FileJournal struct {
	path string
}

// OpenFileJournal prepares the journal at path, creating its directory.
func OpenFileJournal(path string) (*FileJournal, error) {
	if path == "" {
		return nil, errors.New("livecohort: a journal path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating journal directory: %w", err)
	}
	return &FileJournal{path: path}, nil
}

// Path is the file this journal writes, for the operator who has to read it.
func (j *FileJournal) Path() string { return j.path }

// Note records or updates one entry by request id.
func (j *FileJournal) Note(entry JournalEntry) error {
	if entry.RequestID == "" {
		return errors.New("livecohort: a journal entry needs its request id")
	}
	entries, err := j.load()
	if err != nil {
		return err
	}
	if entry.At == "" {
		entry.At = time.Now().UTC().Format(time.RFC3339)
	}
	for i := range entries {
		if entries[i].RequestID != entry.RequestID {
			continue
		}
		// A later note never erases a known job id with an unknown one: the
		// id is the only handle an operator has on a stranded acquisition.
		if entry.JobID == "" {
			entry.JobID = entries[i].JobID
		}
		entries[i] = entry
		return j.save(entries)
	}
	return j.save(append(entries, entry))
}

// Resolve marks one request id accounted for.
func (j *FileJournal) Resolve(requestID string) error {
	entries, err := j.load()
	if err != nil {
		return err
	}
	changed := false
	for i := range entries {
		if entries[i].RequestID == requestID && !entries[i].Resolved {
			entries[i].Resolved = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return j.save(entries)
}

// Unresolved lists this cohort's unaccounted submissions, oldest first.
func (j *FileJournal) Unresolved(cohortID string) ([]JournalEntry, error) {
	entries, err := j.load()
	if err != nil {
		return nil, err
	}
	var out []JournalEntry
	for _, entry := range entries {
		if entry.Resolved || entry.CohortID != cohortID {
			continue
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].At < out[k].At })
	return out, nil
}

func (j *FileJournal) load() ([]JournalEntry, error) {
	data, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		// No journal yet is the first run, and the only safe empty case:
		// nothing was ever submitted under it.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading journal %s: %w", j.path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		// A file that EXISTS and holds nothing is a truncated journal, not
		// a first run: a write may have been interrupted between its
		// creation and its content, and the entries it should hold are the
		// ones that prevent duplicate acquisitions. Fail closed.
		return nil, fmt.Errorf("journal %s exists but is empty; it may have been truncated, so it cannot be trusted to list this cohort's submissions", j.path)
	}
	var entries []JournalEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		// A journal this process cannot read is NOT an empty journal: an
		// unreadable one may name a stranded acquisition, and treating it as
		// empty is exactly the duplicate submission it exists to prevent.
		return nil, fmt.Errorf("decoding journal %s: %w", j.path, err)
	}
	return entries, nil
}

func (j *FileJournal) save(entries []JournalEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing journal %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, j.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("publishing journal %s: %w", j.path, err)
	}
	return nil
}
